package main

import (
	"archive/zip"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	beamauth "dylaris-pkg/beam/auth"
	"dylaris-pkg/beam/quota"
	"dylaris-pkg/fileperms"
	pb "dylaris-proto/beam"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

const beamChunkSize = 64 * 1024 // 64KB

// beamServer implements the BeamNodeService gRPC interface.
// It listens on BEAM_GRPC_PORT (default :25521), reachable on the container's
// overlay network so the Link sidecar in a separate Swarm container can
// forward Yamux streams to it. Public exposure is still blocked at the Swarm
// boundary — every RPC must present a valid BEAM_JWT_SECRET-signed ticket via
// Authenticate before any file op runs, so overlay-internal reachability is
// safe.
type beamServer struct {
	pb.UnimplementedBeamNodeServiceServer
	storageMgr *StorageManager
	throttle   *BeamThrottle
	rdb        *redis.Client // for the beam-upload disk-quota pre-check
	jwtSecret  string        // BEAM_JWT_SECRET — must match the gateway's beam-relay
	nodeID     string        // local node id; tickets must claim this same id

	// serverUUIDByPeer remembers which server a gRPC peer (= one Beam.exe
	// session's TCP connection) is authenticated for. Authenticate writes
	// it; every other RPC reads it via extractServerUUID. Without this
	// the file-op handlers see an empty serverUUID and reject every call
	// with "server_uuid required" — the symptom Beam.exe surfaces as EOF
	// on the first upload chunk.
	//
	// Entries are keyed by peer address and cleared when the connection ends
	// (beamConnCleaner, wired as a gRPC StatsHandler), so a recycled peer
	// address can't inherit a previous session's binding.
	serverUUIDByPeer sync.Map // map[string]string

	// usernameByPeer mirrors serverUUIDByPeer for the ticket's username, kept
	// so UploadFile can attribute a per-user daily upload quota. Authenticate
	// stores claims.Username (only when non-empty); beamConnCleaner clears it
	// alongside the serverUUID binding when the connection ends.
	usernameByPeer sync.Map // map[string]string

	// permsByPeer mirrors the two above for the ticket's file permissions, so
	// every file RPC can ask what this session may DO rather than only which
	// server it may reach. Until it existed the node had nothing to check and
	// allowed every operation to anyone holding a valid ticket - so an account
	// invited as a Builder, a role defined as write-but-not-delete, was refused
	// a delete over HTTP and could remove server.jar through the beam client.
	//
	// Stored as a POINTER: nil means the ticket carried no permissions at all,
	// which is what a Core older than this field mints, and that is a different
	// thing from a ticket granting none. Both are refused; only one is worth
	// telling an operator to update Core about.
	permsByPeer sync.Map // map[string]*fileperms.Perms

	// sessionByPeer keeps the ticket a session authenticated with, so every
	// operation can ask again whether access changed since (sessionLive). An
	// open session used to keep its rights for as long as the connection
	// lived - and the client's health pings kept it alive indefinitely.
	sessionByPeer sync.Map // map[string]*beamSession

	// inflight is the declared size of uploads in progress, per account
	// ("u:<name>") and per server ("s:<uuid>"). The daily quota and the disk
	// headroom were checked per upload against what was already BOOKED, and
	// booking happens when an upload finishes - so N uploads started at once
	// each saw the whole remaining allowance.
	inflight sync.Map // map[string]*atomic.Int64
}

// maxBeamReadContent is the largest file ReadFileContent returns.
const maxBeamReadContent = 10 << 20

// beamMaxStreams bounds the RPCs one connection may run at once.
const beamMaxStreams = 64

// auditBeam sends one change made through Beam to Core's server audit trail,
// on the channel SFTP uses. The panel's file operations were recorded there;
// the same operations through Beam - the file path in production - were not.
func (s *beamServer) auditBeam(ctx context.Context, serverUUID, kind, rel string) {
	if s.rdb == nil || serverUUID == "" {
		return
	}
	a := newSFTPAudit(s.extractUsername(ctx), beamPeerAddr(ctx))
	a.via = "beam"
	a.note(serverUUID, kind, rel)
	publishSFTPAudit(s.rdb, a)
}

// reserveUpload adds n to the in-flight count under key and returns the new
// total and a release. A nil release is never returned.
func (s *beamServer) reserveUpload(key string, n int64) (int64, func()) {
	v, _ := s.inflight.LoadOrStore(key, &atomic.Int64{})
	c := v.(*atomic.Int64)
	total := c.Add(n)
	var once sync.Once
	return total, func() { once.Do(func() { c.Add(-n) }) }
}

// beamSession is one authenticated connection's ticket and when its access
// stamp was last read.
type beamSession struct {
	claims  *beamauth.BeamClaims
	opened  time.Time
	checked atomic.Int64 // unix nanoseconds
}

// beamRecheckEvery bounds how often a session's access stamp is read from
// Redis; between reads an operation goes through on the last answer.
var beamRecheckEvery = 10 * time.Second

// beamMaxSession is the longest a session lives on one ticket. Access stamps
// are kept longer than this, so a change can never age out from under an open
// session; the client reconnects with a fresh ticket.
var beamMaxSession = 24 * time.Hour

// sessionLive reports whether the session on addr may still act. A stamp Core
// set after its ticket was minted - a member removed, a password reset, a role
// changed, an account suspended - ends it: the bindings are dropped and every
// later call is refused until the client reconnects with a new ticket. Fails
// open on a Redis fault, like the check at Authenticate.
func (s *beamServer) sessionLive(ctx context.Context, addr string) bool {
	v, ok := s.sessionByPeer.Load(addr)
	if !ok {
		return true
	}
	sess := v.(*beamSession)
	now := time.Now().UnixNano()
	expired := time.Since(sess.opened) > beamMaxSession
	if !expired && now-sess.checked.Load() < int64(beamRecheckEvery) {
		return true
	}
	// Its own context: the caller's can be cancelled by the client, and a read
	// that fails because the CLIENT hung up must not count as Redis being down.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	stale, err := beamauth.TicketPredatesAccessChange(rctx, s.rdb, sess.claims)
	if err != nil && !expired {
		return true
	}
	if stale || expired {
		log.Printf("beam: ending the session from %s: access to %s changed after its ticket was issued", addr, sess.claims.ServerUUID)
		s.serverUUIDByPeer.Delete(addr)
		s.usernameByPeer.Delete(addr)
		s.permsByPeer.Delete(addr)
		s.sessionByPeer.Delete(addr)
		return false
	}
	sess.checked.Store(now)
	return true
}

// beamConnKey carries the connection's remote address from TagConn through to
// the ConnEnd callback.
type beamConnKey struct{}

// beamConnCleaner clears a peer's serverUUIDByPeer binding when its gRPC
// connection ends. Without this the binding outlives the session, so a later
// connection that reuses the same (recycled) peer address would inherit the
// previous session's authorization without authenticating.
type beamConnCleaner struct {
	uuid  *sync.Map
	user  *sync.Map
	perms *sync.Map
	sess  *sync.Map
}

func (c *beamConnCleaner) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}
func (c *beamConnCleaner) HandleRPC(context.Context, stats.RPCStats) {}
func (c *beamConnCleaner) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	if info != nil && info.RemoteAddr != nil {
		return context.WithValue(ctx, beamConnKey{}, info.RemoteAddr.String())
	}
	return ctx
}
func (c *beamConnCleaner) HandleConn(ctx context.Context, st stats.ConnStats) {
	if _, ended := st.(*stats.ConnEnd); ended {
		if addr, _ := ctx.Value(beamConnKey{}).(string); addr != "" {
			c.uuid.Delete(addr)
			c.user.Delete(addr)
			c.perms.Delete(addr)
			if c.sess != nil {
				c.sess.Delete(addr)
			}
		}
	}
}

// StartBeamServer starts the BeamNodeService gRPC server on BEAM_GRPC_PORT
// (default :25521).
//
// Binds to all interfaces (was 127.0.0.1) so a Link in a sibling Swarm
// container — different network namespace, so different loopback — can
// reach it via the overlay using the Node's service name. Without this,
// Link's stream forwards fail at dial time, the relay's Yamux stream
// closes, and Beam.exe surfaces the misleading "error reading server
// preface: EOF". Auth (JWT ticket) gates all RPCs so wider reachability
// on the overlay doesn't open new attack surface.
//
// Also publishes the Node's BeamNodeService endpoint to Redis (key
// `beam:node-endpoint:<NodeID>`) so Link can discover the right
// overlay IP without relying on Docker-DNS service-name conventions.
// This is the canonical discovery path — works in any Swarm topology
// (cross-stack, custom service names, mixed global/replicated), where
// the NodeID is not necessarily a resolvable hostname.
func StartBeamServer(ctx context.Context, rdb *redis.Client, storageMgr *StorageManager, throttle *BeamThrottle, jwtSecret, nodeID string) {
	beamPort := os.Getenv("BEAM_GRPC_PORT")
	if beamPort == "" {
		beamPort = "25521"
	}
	listenAddr := ":" + beamPort
	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		// Distinguish a busy port (the most common, actionable cause — another
		// process already holds BEAM_GRPC_PORT) from other listen failures, so
		// the LAN fast-path's "port busy" condition is unambiguous in the logs.
		if errors.Is(err, syscall.EADDRINUSE) {
			log.Printf("beam-server: PORT_BUSY — %s is already in use; set BEAM_GRPC_PORT to a free port or stop the conflicting process (LAN fast-path disabled until resolved)", listenAddr)
		} else {
			log.Printf("beam-server: failed to listen on %s: %v", listenAddr, err)
		}
		return
	}

	bs := &beamServer{
		storageMgr: storageMgr,
		throttle:   throttle,
		rdb:        rdb,
		jwtSecret:  jwtSecret,
		nodeID:     nodeID,
	}
	srv := grpc.NewServer(grpc.MaxConcurrentStreams(beamMaxStreams), grpc.StatsHandler(&beamConnCleaner{uuid: &bs.serverUUIDByPeer, user: &bs.usernameByPeer, perms: &bs.permsByPeer, sess: &bs.sessionByPeer}))
	pb.RegisterBeamNodeServiceServer(srv, bs)

	log.Printf("beam-server: listening on %s (reachable via overlay; JWT-gated)", listenAddr)

	// Publish endpoint to Redis so Link can discover us via overlay IP.
	go publishBeamEndpoint(ctx, rdb, nodeID, beamPort)

	// Sweep stale .beam-upload-* temp files. The UploadFile handler's
	// defer normally removes them on cancel/error, but a kill -9 or
	// container restart mid-stream can leak. Anything untouched for >15s
	// is fair game — successful uploads rename atomically the moment the
	// stream EOFs, so a live partial is always actively being written.
	go sweepStaleUploadTemps(ctx, storageMgr)

	// LAN fast-path TLS listener (opt-in, default on). The plain :25521 listener
	// above is reached only via the encrypted overlay (relay hop). Direct LAN
	// clients (the Beam app on the same network) instead hit this TLS listener so
	// the hop is encrypted; the cert is deterministically derived, and Core hands
	// the app the matching fingerprint to pin, which also defeats MITM. Same
	// handler/auth (the JWT ticket) as the relay path.
	//
	// Keyed on the PER-NODE secret, not BEAM_JWT_SECRET. A BYON machine never gets
	// fleet secrets - the deploy snippet withholds them deliberately - so the old
	// derivation failed with "auth: empty secret" on every customer node while the
	// snippet still set BEAM_LAN_FASTPATH=true and the panel still advertised the
	// port. The per-node secret is the one Core and this node already share, and
	// it never crosses the wire.
	if os.Getenv("BEAM_LAN_FASTPATH") != "false" {
		go startBeamLANListener(ctx, bs, nodeID)
	}

	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()

	if err := srv.Serve(lis); err != nil {
		log.Printf("beam-server: serve error: %v", err)
	}
}

// startBeamLANListener serves the BeamNodeService over TLS on the LAN fast-path
// port using the deterministic pinned certificate. Failures are non-fatal — the
// relay path keeps working regardless.
func startBeamLANListener(ctx context.Context, bs *beamServer, nodeID string) {
	lanPort := os.Getenv("BEAM_LAN_PORT")
	if lanPort == "" {
		lanPort = "25523"
	}
	// The per-node secret arrives over the authenticated gRPC bootstrap, which
	// can lag this goroutine on a cold start. Wait for it rather than deriving
	// from an empty secret and disabling the listener for the process' lifetime.
	secret, ok := waitForNodeSecret(ctx, 2*time.Minute)
	if !ok {
		log.Printf("beam-server: LAN fast-path disabled, no per-node secret after 2m")
		return
	}
	cert, fp, derr := beamauth.DeriveLANCert(hex.EncodeToString(secret), nodeID)
	if derr != nil {
		log.Printf("beam-server: LAN fast-path disabled, cert derive failed: %v", derr)
		return
	}
	addr := ":" + lanPort
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			log.Printf("beam-server: PORT_BUSY (LAN) — %s is already in use; LAN fast-path disabled", addr)
		} else {
			log.Printf("beam-server: LAN fast-path listen on %s failed: %v", addr, err)
		}
		return
	}
	tlsSrv := grpc.NewServer(
		grpc.MaxConcurrentStreams(beamMaxStreams),
		grpc.Creds(credentials.NewServerTLSFromCert(&cert)),
		grpc.StatsHandler(&beamConnCleaner{uuid: &bs.serverUUIDByPeer, user: &bs.usernameByPeer, perms: &bs.permsByPeer, sess: &bs.sessionByPeer}),
	)
	pb.RegisterBeamNodeServiceServer(tlsSrv, bs)
	log.Printf("beam-server: LAN fast-path (TLS, pinned) listening on %s, fp=%s", addr, fp[:16]+"...")
	go func() {
		<-ctx.Done()
		tlsSrv.GracefulStop()
	}()
	if err := tlsSrv.Serve(lis); err != nil {
		log.Printf("beam-server: LAN fast-path serve error: %v", err)
	}
}

// publishBeamEndpoint refreshes a Redis key with this Node's BeamNodeService
// overlay endpoint every 10s (30s TTL). Link reads this key to find the
// right address for its NodeID, sidestepping Docker-DNS service-name
// conventions which break across Swarm stacks (Link in dylaris-gateway
// can't resolve "node-eu-v01" — the Node's NODE_ID is a host hostname,
// not a registered service alias).
//
// Best-effort: a Redis outage just causes Link to fall back to its
// service-name / loopback guess. Beam.exe surfaces "could not reach the
// Beam relay ..." in that case, so the failure mode is visible.
func publishBeamEndpoint(ctx context.Context, rdb *redis.Client, nodeID, port string) {
	if rdb == nil || strings.TrimSpace(nodeID) == "" {
		return
	}
	const (
		key         = "beam:node-endpoint:"
		ttl         = 30 * time.Second
		refreshTick = 10 * time.Second
	)
	var lastLoggedIP string
	publish := func() {
		// Only advertise when Beam is actually in use: file mode beam/both, or
		// an external node (which always forces beam locally). The listener
		// keeps running regardless so a runtime mode switch is cheap — we just
		// re-check beamAdvertiseEnabled() each tick and start/stop advertising.
		// fileAccessMode is refreshed from Redis by the 30s mode loop in main.
		if !beamAdvertiseEnabled() {
			lastLoggedIP = "" // re-log once when advertising resumes
			return
		}
		ip := overlayIP()
		if ip == "" {
			return
		}
		if err := rdb.Set(ctx, key+nodeID, ip+":"+port, ttl).Err(); err != nil {
			log.Printf("beam-server: endpoint publish failed: %v", err)
			return
		}
		// Log only when the published IP changes (typically once at boot,
		// then on container restart) so the log isn't spammed every 10s.
		if ip != lastLoggedIP {
			log.Printf("beam-server: endpoint published to Redis (%s:%s for node %q)", ip, port, nodeID)
			lastLoggedIP = ip
		}
	}
	publish()
	ticker := time.NewTicker(refreshTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publish()
		}
	}
}

// sweepStaleUploadTemps periodically removes orphaned .beam-upload-* temp
// files. The UploadFile handler's defer normally removes them when a
// session ends, but a process kill or sudden container shutdown can
// leave them. Anything not modified for more than the grace period is
// considered stale — a live upload is being actively written to, so its
// mtime is always fresh.
func sweepStaleUploadTemps(ctx context.Context, sm *StorageManager) {
	const (
		grace = 15 * time.Second
		// Five minutes, not thirty seconds, because the pass below now walks
		// the whole server tree instead of two directories. Nothing depends on
		// a dead temp disappearing quickly - it is a leftover file, and the
		// defer already removes it on every exit that runs one. Sweeping less
		// often also makes it less likely to catch a live upload whose client
		// stalled for longer than the grace period.
		every = 5 * time.Minute
	)
	sweep := func() {
		for _, base := range sm.Paths() {
			entries, err := os.ReadDir(base)
			if err != nil {
				continue
			}
			now := time.Now()
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				// Each direct child of a storage path is a server UUID dir, and
				// the whole tree below it has to be searched.
				//
				// This used to glob the server dir and one level below it, on the
				// note that "one level deep is enough". It is not: the temp is
				// created next to the upload's DESTINATION (filepath.Dir(destPath)),
				// and validateBeamPath puts no depth limit on that - an upload to
				// "survival/plugins/BlueMap/config.conf" leaves its temp three
				// levels down, where nothing ever looked. A kill mid-stream then
				// left the partial file on the server's disk for good, counting
				// against its limit.
				root, err := os.OpenRoot(filepath.Join(base, e.Name()))
				if err != nil {
					continue
				}
				_ = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
					if err != nil || d.IsDir() {
						// A directory that cannot be read is skipped, not fatal:
						// one unreadable corner must not stop the rest of the sweep.
						return nil
					}
					if !strings.HasPrefix(d.Name(), ".beam-upload-") {
						return nil
					}
					info, ierr := d.Info()
					if ierr != nil || now.Sub(info.ModTime()) < grace {
						return nil
					}
					if rerr := root.Remove(name); rerr == nil {
						log.Printf("beam-server: sweeper removed stale temp %s/%s", e.Name(), name)
					}
					return nil
				})
				root.Close()
			}
		}
	}
	sweep()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

// overlayIP returns the container's IP on the Swarm overlay network the
// Link needs to reach us on. The naive net.Dial("udp", "8.8.8.8:80") trick
// returns the wrong interface here — in Swarm the default route goes
// through docker_gwbridge (172.18.0.0/16), but overlay traffic to sibling
// services flows through a separate eth attached to the overlay (10.x).
// We need the latter, otherwise Link tries to dial a gwbridge IP that's
// per-host and not routable from other containers.
//
// Selection: walk all non-loopback IPv4 interfaces and prefer 10.0.0.0/8
// (Swarm overlay default range). Falls back to 172.x/192.x for unusual
// deploys where the overlay subnet isn't in the 10.x range.
func overlayIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	var fallback string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP.To4()
		if ip == nil || ip.IsLoopback() {
			continue
		}
		// 10.0.0.0/8 — Swarm overlay's default range. First match wins.
		if ip[0] == 10 {
			return ip.String()
		}
		// 172.16.0.0/12 or 192.168.0.0/16 — saved as fallback for
		// non-standard overlay subnets. The 172.18.x gwbridge IPs end
		// up here too; we only use them if no 10.x is found.
		if fallback == "" && (ip[0] == 172 || ip[0] == 192) {
			fallback = ip.String()
		}
	}
	return fallback
}

// isPlatformReservedName returns true for filenames the platform manages
// internally and users must NOT be able to overwrite via the Beam file
// browser (writing to them would silently break server orchestration).
// Read access stays allowed so the UI can still list them.
//
// The named set is isProtectedFile's, deliberately, because this used to be a
// second hand-written copy of it and the two had drifted: this one knew
// ".active_server" and the ".dylaris" prefix but NOT ".node_config.json" or
// ".pending-delete-*", so a beam client could upload straight over the file the
// node recreates a container from, while the same write over SFTP or the panel
// file browser was refused. Two spellings of one rule is one spelling too many.
//
// The prefix rule stays and is beam-specific: it is broader than the named set
// and also covers scratch directories such as .dylaris-mrpack.
func isPlatformReservedName(name string) bool {
	return strings.HasPrefix(name, ".dylaris") || isProtectedFile(name)
}

// reservedComponent returns the first path component that is platform-reserved,
// or "" when none is.
//
// The basename alone is not enough. ".dylaris-backups" is reserved, but a write
// to ".dylaris-backups/<id>.tar.gz" ends in an ordinary archive filename, so a
// basename check let the beam client delete and overwrite backup archives -
// exactly the tampering the reserved set exists to prevent. Reads are checked
// nowhere, which is deliberate: the desktop client downloads backups this way.
func reservedComponent(rel string) string {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part != "" && part != "." && isPlatformReservedName(part) {
			return part
		}
	}
	return ""
}

// validateBeamPath ensures the path stays within the server's data directory.
// When op is "write" (upload, save, create, delete, rename, copy-dst), it
// also refuses any platform-reserved filename. Read ops ("read", "list")
// pass even on reserved names so the UI can show them.
//
// The read-op carve-out is what lets the Beam.exe desktop app download a
// backup archive directly from .dylaris-backups/<id>.tar.gz: Core hands
// the client a ticket for the owning server, the client opens a
// DownloadFile stream with the relative path, and validateBeamPathRead
// resolves it against the server dir like any other file. The hidden
// .dylaris- prefix only blocks writes — perfect for read-only backup
// downloads while still preventing tampering through the regular file
// browser.
func (s *beamServer) validateBeamPath(reqPath, serverUUID string) (string, error) {
	return s.validateBeamPathOp(reqPath, serverUUID, "write")
}

func (s *beamServer) validateBeamPathOp(reqPath, serverUUID, op string) (string, error) {
	if serverUUID == "" {
		return "", fmt.Errorf("server_uuid required")
	}

	// Shared traversal guard (trailing-separator containment) — see
	// resolveWithinDir in grpc_handler.go. Beam layers its write-time
	// reserved-name check on top.
	serverDir := s.storageMgr.GetServerDir(serverUUID)
	cleanPath, err := resolveWithinDir(serverDir, reqPath)
	if err != nil {
		return "", err
	}
	// The server directory itself is not a write target. An empty path
	// resolves to it, which reading needs (that is the file browser's root)
	// and writing must never get: DeleteFile would RemoveAll the server and
	// its backups, RenameFile would move it out from under its UUID. The
	// reserved-name check below cannot see this, because the basename of the
	// root is the server UUID, which is not a reserved name.
	if op == "write" && filepath.Clean(cleanPath) == filepath.Clean(serverDir) {
		return "", fmt.Errorf("access denied: the server directory itself is not a valid target")
	}
	if op == "write" {
		rel, err := filepath.Rel(serverDir, cleanPath)
		if err != nil {
			return "", fmt.Errorf("access denied: %v", err)
		}
		if part := reservedComponent(rel); part != "" {
			return "", fmt.Errorf("access denied: %q is platform-managed and cannot be overwritten", part)
		}
	}
	return cleanPath, nil
}

// jailBeam is validateBeamPathOp plus the server directory opened as an
// os.Root, so the operation itself cannot be walked out of the directory by a
// link the tenant swaps in after the check (see rootfs.go). The caller closes
// the Root and operates on the returned name through it.
func (s *beamServer) jailBeam(reqPath, serverUUID, op string) (*os.Root, string, error) {
	abs, err := s.validateBeamPathOp(reqPath, serverUUID, op)
	if err != nil {
		return nil, "", err
	}
	serverDir := s.storageMgr.GetServerDir(serverUUID)
	if op == "write" {
		return openJailedForWrite(serverDir, reqPath)
	}
	name, err := rootName(serverDir, abs)
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(serverDir)
	if err != nil {
		return nil, "", err
	}
	return root, name, nil
}

// ─── Auth ────────────────────────────────────────────────────────────

// validateTicket reads a beam ticket with whichever key this node actually has.
//
// A BYON machine holds NO fleet secret - the deploy snippet withholds it on
// purpose - so it cannot check the JWT signature, and used to reject every
// ticket with "node beam auth not configured". The listener came up, TLS
// pinned, and file access still did not exist.
//
// Core therefore stamps a per-node proof into the ticket, keyed on the secret
// this node and Core already share and which never crosses the wire. That
// proof covers every claim that decides access, so verifying it is as strong
// a statement as verifying the signature - see pkg/beam/auth/node_proof.go.
//
// The per-node path is tried FIRST, so a machine that has both prefers the
// narrower key: a fleet-signed ticket is valid for every node at once, while a
// proof is valid for exactly this one.
//
// Refuses when neither key is available, rather than accepting blindly.
func (s *beamServer) validateTicket(ticket string) (*beamauth.BeamClaims, error) {
	if secret := getNodeSecret(); len(secret) > 0 {
		claims, err := beamauth.ValidateBeamTicketByNodeProof(secret, ticket)
		if err == nil {
			return claims, nil
		}
		// Fall through to the fleet key: a platform node that has both may still
		// be handed a ticket minted before Core could load its secret.
		if s.jwtSecret == "" {
			return nil, fmt.Errorf("invalid ticket: %w", err)
		}
	}
	if s.jwtSecret == "" {
		return nil, errors.New("node beam auth not configured")
	}
	claims, err := beamauth.ValidateBeamTicket(s.jwtSecret, ticket)
	if err != nil {
		return nil, fmt.Errorf("invalid ticket: %w", err)
	}
	return claims, nil
}

// beamPeerAddr names the caller for a log line, or "unknown" when gRPC did not
// give us one. Only ever used for logging.
func beamPeerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p != nil && p.Addr != nil {
		return p.Addr.String()
	}
	return "unknown"
}

func (s *beamServer) Authenticate(ctx context.Context, req *pb.BeamAuthReq) (*pb.BeamAuthResp, error) {
	claims, err := s.validateTicket(req.Ticket)
	if err != nil {
		// Logged because it was not. A refused ticket left no trace here at
		// all, so an attempt against a customer's own machine was invisible to
		// whoever runs it - and this is the hop that does not pass the relay,
		// which does log its refusals. The reason, never the ticket.
		log.Printf("beam: authentication refused from %s: %v", beamPeerAddr(ctx), err)
		return &pb.BeamAuthResp{Ok: false, Message: err.Error()}, nil
	}
	// Node-binding: the relay routes by node_id, but a stolen ticket for
	// another node should still be rejected at the destination.
	if s.nodeID != "" && claims.NodeID != s.nodeID {
		log.Printf("beam: authentication refused from %s: ticket is bound to node %s, this is %s",
			beamPeerAddr(ctx), claims.NodeID, s.nodeID)
		return &pb.BeamAuthResp{Ok: false, Message: "ticket bound to a different node"}, nil
	}
	// The ticket is a bearer token with a 30 minute life that nothing re-reads,
	// so taking somebody's access away used to leave an outstanding one working
	// to its expiry. Core stamps the server when that changes; a ticket minted
	// before the stamp does not open a new session, and an open one ends at its
	// next operation (sessionLive).
	//
	// Fails OPEN on a Redis fault, like every other Redis read on this path: an
	// outage must not stand between a customer and their own files, and what it
	// reopens is the window that existed before.
	actx, acancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	stale, aerr := beamauth.TicketPredatesAccessChange(actx, s.rdb, claims)
	acancel()
	if aerr != nil {
		log.Printf("beam: could not check the access stamp for %s, allowing: %v", claims.ServerUUID, aerr)
	} else if stale {
		log.Printf("beam: authentication refused from %s: access to %s changed after this ticket was issued",
			beamPeerAddr(ctx), claims.ServerUUID)
		return &pb.BeamAuthResp{Ok: false, Message: "your access to this server changed - reconnect to get a new ticket"}, nil
	}
	// Remember which server this gRPC connection is allowed to touch.
	// extractServerUUID reads the same key on every subsequent RPC from
	// the same Beam.exe session.
	if p, ok := peer.FromContext(ctx); ok && p != nil && p.Addr != nil {
		s.serverUUIDByPeer.Store(p.Addr.String(), claims.ServerUUID)
		// Keep the username too, but only when present, so an empty-username
		// ticket never shares a daily-quota bucket with another user.
		if claims.Username != "" {
			s.usernameByPeer.Store(p.Addr.String(), claims.Username)
		}
		// Stored even when nil, so extractFilePerms can tell "this session was
		// never authenticated" from "authenticated by a Core that sends no
		// permissions".
		s.permsByPeer.Store(p.Addr.String(), claims.Perms)
		sess := &beamSession{claims: claims, opened: time.Now()}
		sess.checked.Store(time.Now().UnixNano())
		s.sessionByPeer.Store(p.Addr.String(), sess)
	}
	return &pb.BeamAuthResp{
		Ok:         true,
		ServerUuid: claims.ServerUUID,
	}, nil
}

// ─── File Operations ─────────────────────────────────────────────────

// requireFilePerm answers whether this session may perform one kind of file
// operation, and returns the refusal to hand back when it may not.
//
// Three states, and they are deliberately not collapsed. An unauthenticated peer
// has no entry at all. An authenticated peer whose ticket predates the
// permissions field has a nil one, and the message says so, because the fix for
// that is to update Core and nothing about the account is wrong. Anything else
// is an ordinary refusal.
//
// Refuses when it cannot tell. The alternative reading - "no permissions
// recorded, so allow" - is the behaviour this replaced.
func (s *beamServer) requireFilePerm(ctx context.Context, want func(fileperms.Perms) bool, verb string) error {
	p, ok := peer.FromContext(ctx)
	if !ok || p == nil || p.Addr == nil {
		return status.Error(codes.PermissionDenied, "not authenticated")
	}
	if !s.sessionLive(ctx, p.Addr.String()) {
		return status.Error(codes.PermissionDenied, "your access to this server changed - reconnect to get a new ticket")
	}
	v, found := s.permsByPeer.Load(p.Addr.String())
	if !found {
		return status.Error(codes.PermissionDenied, "not authenticated")
	}
	perms, _ := v.(*fileperms.Perms)
	if perms == nil {
		return status.Error(codes.PermissionDenied,
			"this ticket carries no file permissions - the panel that issued it is older than this node, update it")
	}
	if !want(*perms) {
		return status.Errorf(codes.PermissionDenied, "you do not have permission to %s files on this server", verb)
	}
	return nil
}

func canRead(p fileperms.Perms) bool   { return p.Read }
func canWrite(p fileperms.Perms) bool  { return p.Write }
func canDelete(p fileperms.Perms) bool { return p.Delete }

func (s *beamServer) ListFiles(ctx context.Context, req *pb.BeamFileListReq) (*pb.BeamFileListResp, error) {
	if err := s.requireFilePerm(ctx, canRead, "list"); err != nil {
		return &pb.BeamFileListResp{}, err
	}
	serverUUID := s.extractServerUUID(ctx)
	root, dirName, err := s.jailBeam(req.Path, serverUUID, "read")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &pb.BeamFileListResp{Files: []*pb.BeamFileInfo{}}, nil
		}
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	defer root.Close()

	entries, err := fs.ReadDir(root.FS(), dirName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &pb.BeamFileListResp{Files: []*pb.BeamFileInfo{}}, nil
		}
		return nil, status.Errorf(codes.Internal, "read dir: %v", err)
	}

	var files []*pb.BeamFileInfo
	for _, e := range entries {
		// Hidden platform-managed entries — keep them out of the file
		// browser entirely. .dylaris-backups is the node-local backup
		// store: still readable via the dedicated DownloadFile path
		// (validateBeamPathRead allows reads on dot-prefixed names) so
		// Beam.exe can grab an archive when given a direct path, but
		// it must not appear in a regular directory listing.
		// .dylaris.json holds platform metadata; .pending-delete-* are
		// rename tombstones from sub-server cleanup.
		if e.Name() == ".active_server" || e.Name() == ".dylaris-backups" || e.Name() == ".dylaris.json" {
			continue
		}
		if strings.HasPrefix(e.Name(), ".pending-delete-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, &pb.BeamFileInfo{
			Name:     e.Name(),
			IsDir:    e.IsDir(),
			Size:     info.Size(),
			Modified: info.ModTime().Unix(),
		})
	}

	return &pb.BeamFileListResp{Files: files}, nil
}

func (s *beamServer) ReadFileContent(ctx context.Context, req *pb.BeamFileReadReq) (*pb.BeamFileContentResp, error) {
	if err := s.requireFilePerm(ctx, canRead, "read"); err != nil {
		return nil, err
	}
	serverUUID := s.extractServerUUID(ctx)
	root, name, err := s.jailBeam(req.Path, serverUUID, "read")
	if err != nil {
		return &pb.BeamFileContentResp{Success: false, Message: err.Error()}, nil
	}
	defer root.Close()

	// The whole file is held in memory, about three times over by the time it
	// is a message, and one member reading a world archive in parallel could
	// take the node agent - and every tenant's console and files with it -
	// down. Core caps opening a file at the same size; larger ones are
	// downloaded.
	f, err := root.Open(name)
	if err != nil {
		return &pb.BeamFileContentResp{Success: false, Message: err.Error()}, nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBeamReadContent+1))
	if err != nil {
		return &pb.BeamFileContentResp{Success: false, Message: err.Error()}, nil
	}
	if len(data) > maxBeamReadContent {
		return &pb.BeamFileContentResp{Success: false, Message: "this file is too large to open here (over 10 MB); download it instead"}, nil
	}

	return &pb.BeamFileContentResp{Success: true, Content: string(data)}, nil
}

func (s *beamServer) SaveFileContent(ctx context.Context, req *pb.BeamFileSaveReq) (*pb.BeamOpResp, error) {
	if err := s.requireFilePerm(ctx, canWrite, "write"); err != nil {
		return nil, err
	}
	serverUUID := s.extractServerUUID(ctx)
	root, name, err := s.jailBeam(req.Path, serverUUID, "write")
	if err != nil {
		return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
	}
	defer root.Close()

	// A direct content save writes bytes to the server dir just like an upload,
	// so the same admin caps apply — otherwise it would be a way to write past
	// the size cap / disk limit / daily quota. Size is known up front.
	size := int64(len(req.Content))
	username := s.extractUsername(ctx)
	if total, limit := serverDiskGauge(ctx, s.rdb, serverUUID); beamUploadExceedsDisk(total, limit, size) {
		return &pb.BeamOpResp{Success: false, Message: fmt.Sprintf("disk limit reached: %d of %d bytes used", total, limit)}, nil
	}
	if ok, capBytes := quota.CheckSizeCap(ctx, s.rdb, size); !ok {
		return &pb.BeamOpResp{Success: false, Message: fmt.Sprintf("file of %d bytes exceeds the %d byte per-upload limit", size, *capBytes)}, nil
	}
	if ok, used, limit := quota.CheckDailyQuota(ctx, s.rdb, username, size); !ok {
		return &pb.BeamOpResp{Success: false, Message: fmt.Sprintf("daily upload quota reached: %d of %d bytes used today", used, *limit)}, nil
	}

	if err := root.WriteFile(name, []byte(req.Content), 0644); err != nil {
		return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
	}
	// A NEW file is the node's (root) and the running server could not write
	// it; the start-time repair skips a directory that is already the
	// container's, so nothing fixed it later either.
	chownForMCIn(root, name)
	s.recordBeamDailyUsage(ctx, username, size)

	s.auditBeam(ctx, serverUUID, "write", req.Path)
	return &pb.BeamOpResp{Success: true, Message: "saved"}, nil
}

func (s *beamServer) CreateFile(ctx context.Context, req *pb.BeamFileCreateReq) (*pb.BeamOpResp, error) {
	if err := s.requireFilePerm(ctx, canWrite, "create"); err != nil {
		return nil, err
	}
	serverUUID := s.extractServerUUID(ctx)
	root, name, err := s.jailBeam(req.Path, serverUUID, "write")
	if err != nil {
		return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
	}
	defer root.Close()

	if req.IsDir {
		if err := root.MkdirAll(name, 0755); err != nil {
			return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
		}
	} else {
		f, err := createIn(root, name, 0644)
		if err != nil {
			return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
		}
		f.Close()
	}
	// As the panel's create (handleCreate): a folder made here was root's, and
	// the server could not put anything in it.
	chownForMCIn(root, name)

	s.auditBeam(ctx, serverUUID, "write", req.Path)
	return &pb.BeamOpResp{Success: true, Message: "created"}, nil
}

func (s *beamServer) DeleteFile(ctx context.Context, req *pb.BeamFileDeleteReq) (*pb.BeamOpResp, error) {
	if err := s.requireFilePerm(ctx, canDelete, "delete"); err != nil {
		return nil, err
	}
	serverUUID := s.extractServerUUID(ctx)
	root, name, err := s.jailBeam(req.Path, serverUUID, "write")
	if err != nil {
		return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
	}
	defer root.Close()

	if err := root.RemoveAll(name); err != nil {
		return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
	}

	s.auditBeam(ctx, serverUUID, "delete", req.Path)
	return &pb.BeamOpResp{Success: true, Message: "deleted"}, nil
}

func (s *beamServer) RenameFile(ctx context.Context, req *pb.BeamFileRenameReq) (*pb.BeamOpResp, error) {
	if err := s.requireFilePerm(ctx, canWrite, "rename"); err != nil {
		return nil, err
	}
	serverUUID := s.extractServerUUID(ctx)
	oldPath, err := s.validateBeamPathOp(req.OldPath, serverUUID, "write")
	if err != nil {
		return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
	}
	serverDir := s.storageMgr.GetServerDir(serverUUID)
	oldName, err := rootName(serverDir, oldPath)
	if err != nil {
		return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
	}

	// The DESTINATION goes through the same guard, which validateBeamPath's own
	// doc comment already lists rename under. It did not: NewName went straight
	// into filepath.Join, and Join CLEANS rather than confines, so a name of
	// "../<other-uuid>/plugins/x.jar" walked out of this ticket's server
	// directory into a neighbour's. Both live on the same storage path, so
	// os.Rename across them succeeds - and a jar moved into someone else's
	// plugins/ is code their Minecraft server loads. Reproduced end to end.
	//
	// Validating the request-relative destination rather than the resolved one
	// keeps it the same shape as every other path in this file, and picks up the
	// reserved-name check for free: a rename onto .active_server,
	// .node_config.json, .dylaris.json or .dylaris-backups is a write to a
	// platform-managed name like any other.
	newPath, err := s.validateBeamPath(filepath.Join(filepath.Dir(req.OldPath), req.NewName), serverUUID)
	if err != nil {
		return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
	}
	newName, err := rootName(s.storageMgr.GetServerDir(serverUUID), newPath)
	if err != nil {
		return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
	}

	// Both parents are reached without following a link (see writeScope).
	if err := renameNoFollow(serverDir, oldName, newName); err != nil {
		return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
	}

	s.auditBeam(ctx, serverUUID, "rename", req.OldPath+" -> "+req.NewName)
	return &pb.BeamOpResp{Success: true, Message: "renamed"}, nil
}

func (s *beamServer) CopyFile(ctx context.Context, req *pb.BeamFileCopyReq) (*pb.BeamOpResp, error) {
	if err := s.requireFilePerm(ctx, canWrite, "copy"); err != nil {
		return nil, err
	}
	serverUUID := s.extractServerUUID(ctx)
	srcPath, err := s.validateBeamPath(req.SrcPath, serverUUID)
	if err != nil {
		return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
	}
	dstPath, err := s.validateBeamPath(req.DstPath, serverUUID)
	if err != nil {
		return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
	}
	if err := validateCopyPaths(req.SrcPath, req.DstPath, srcPath, dstPath); err != nil {
		return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
	}
	root, srcName, err := s.jailBeam(req.SrcPath, serverUUID, "read")
	if err != nil {
		return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
	}
	defer root.Close()
	dstName, err := rootName(s.storageMgr.GetServerDir(serverUUID), dstPath)
	if err != nil {
		return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
	}
	// The destination is written, so it is reached without following a link.
	dstRoot, dstLeaf, err := writeScope(s.storageMgr.GetServerDir(serverUUID), dstName, true)
	if err != nil {
		return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
	}
	defer dstRoot.Close()

	stat, err := root.Stat(srcName)
	if err != nil {
		return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
	}

	if stat.IsDir() {
		if err := copyDirForTenant(root, srcName, s.storageMgr.GetServerDir(serverUUID), dstName); err != nil {
			return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
		}
	} else {
		if err := copyFileIn(root, srcName, dstRoot, dstLeaf); err != nil {
			return &pb.BeamOpResp{Success: false, Message: err.Error()}, nil
		}
		chownForMCIn(dstRoot, dstLeaf)
	}

	s.auditBeam(ctx, serverUUID, "write", req.DstPath)
	return &pb.BeamOpResp{Success: true, Message: "copied"}, nil
}

// ─── Streaming Downloads ─────────────────────────────────────────────

func (s *beamServer) DownloadFile(req *pb.BeamDownloadReq, stream grpc.ServerStreamingServer[pb.BeamChunk]) error {
	if err := s.requireFilePerm(stream.Context(), canRead, "download"); err != nil {
		return err
	}
	ctx := stream.Context()
	serverUUID := s.extractServerUUID(ctx)
	root, name, err := s.jailBeam(req.Path, serverUUID, "read")
	if err != nil {
		return status.Error(codes.PermissionDenied, err.Error())
	}
	defer root.Close()

	stat, err := root.Stat(name)
	if err != nil {
		return status.Errorf(codes.NotFound, "file not found")
	}

	if stat.IsDir() {
		// ZipIfDir is what the client sets when the user picked a folder. Refuse
		// rather than silently sending something else when it is not set: the
		// caller asked for a file and a zip is not that.
		if !req.ZipIfDir {
			return status.Error(codes.InvalidArgument, "path is a directory; set zip_if_dir to download it as an archive")
		}
		display := filepath.Join(s.storageMgr.GetServerDir(serverUUID), filepath.FromSlash(name))
		return s.streamZip(stream, zipNameFor(display), func(zw *zip.Writer) error {
			return addTreeToZip(zw, root, name, name)
		})
	}

	f, err := root.Open(name)
	if err != nil {
		return status.Errorf(codes.Internal, "open file: %v", err)
	}
	defer f.Close()

	buf := make([]byte, beamChunkSize)
	var offset int64
	first := true

	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			// Throttle: this is the download direction (disk → client).
			if err := s.throttle.WaitN(ctx, DirectionDown, n); err != nil {
				return status.Errorf(codes.Canceled, "throttle: %v", err)
			}

			chunk := &pb.BeamChunk{
				Data:   buf[:n],
				Offset: offset,
			}
			if first {
				chunk.Filename = path.Base(name)
				chunk.TotalSize = stat.Size()
				first = false
			}

			if err := stream.Send(chunk); err != nil {
				return err
			}
			offset += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return status.Errorf(codes.Internal, "read file: %v", readErr)
		}
	}

	return nil
}

// readUploadIDFromContext extracts the x-beam-upload-id gRPC metadata
// header the client attaches per call so the server can identify which
// session a chunk belongs to. Empty means non-resumable session — the
// server falls back to a random temp name in that case.
func readUploadIDFromContext(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	values := md.Get("x-beam-upload-id")
	if len(values) == 0 {
		return ""
	}
	// gRPC normalises metadata keys to lowercase; sanitise the value so a
	// malicious client can't escape the filename it ends up in.
	id := values[0]
	id = strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9':
			return r
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r == '-' || r == '_':
			return r
		default:
			return -1
		}
	}, id)
	if len(id) > 64 {
		id = id[:64]
	}
	return id
}

func (s *beamServer) UploadFile(stream grpc.ClientStreamingServer[pb.BeamUploadMsg, pb.BeamOpResp]) error {
	if err := s.requireFilePerm(stream.Context(), canWrite, "upload"); err != nil {
		return err
	}
	ctx := stream.Context()
	serverUUID := s.extractServerUUID(ctx)
	username := s.extractUsername(ctx)
	uploadID := readUploadIDFromContext(ctx)

	// upRoot is the server directory the upload writes into, open for the
	// whole stream; destName and tmpName are names inside it.
	var upRoot *os.Root
	var destName, auditPath string // auditPath: the requested path, for the audit trail
	var tmpFile *os.File
	var tmpName string
	// declaredSize is the client's BeamUploadStart TotalSize. The disk-headroom,
	// size-cap and daily-quota pre-checks are all evaluated against it, so the
	// chunk loop MUST enforce that the actual bytes written never exceed it —
	// otherwise a client declares a tiny size, passes every check, then streams
	// unbounded (defeating the size cap + disk limit and slipping the quota).
	var declaredSize int64
	// completed flips true after we receive the client's stream EOF — the
	// signal that all chunks made it through. Until then any exit path
	// (cancel, disconnect, error) must remove the temp file so a partial
	// upload never gets renamed into place.
	//
	// EXCEPT when we have a stable uploadID. Then a mid-stream drop leaves
	// the temp on disk so the client can resume into the same file on a
	// follow-up BeamUploadStart with the same id. The 15s sweeper removes
	// abandoned temps anyway.
	completed := false

	defer func() {
		if upRoot != nil {
			defer upRoot.Close()
		}
		if tmpFile == nil {
			return
		}
		tmpFile.Close()
		if completed {
			return
		}
		// Cancel / disconnect / error path.
		if uploadID != "" {
			// Stable id present → keep the temp so a resume can pick up
			// where this stream left off. Sweeper trims it if no resume
			// comes within the grace window.
			log.Printf("beam-server: upload %s interrupted, keeping partial temp %s for resume", uploadID, path.Base(tmpName))
			return
		}
		upRoot.Remove(tmpName)
		log.Printf("beam-server: upload aborted, removed partial temp %s", path.Base(tmpName))
	}()

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			completed = true
			break
		}
		if err != nil {
			// Either ctx cancelled (client aborted) or stream broke. The
			// defer above either drops the temp (no id) or keeps it for
			// resume (with id).
			return err
		}

		switch p := msg.Payload.(type) {
		case *pb.BeamUploadMsg_Start:
			if tmpFile != nil {
				return status.Error(codes.FailedPrecondition, "upload already started")
			}
			remotePath := filepath.Join(p.Start.Path, p.Start.Filename)
			root, name, err := s.jailBeam(remotePath, serverUUID, "write")
			if err != nil {
				return status.Error(codes.PermissionDenied, err.Error())
			}
			if upRoot != nil {
				upRoot.Close()
			}
			upRoot, destName = root, name
			auditPath = filepath.ToSlash(remotePath)
			declaredSize = p.Start.TotalSize

			// Create the destination's parent dir if it doesn't exist yet. The
			// HTTP write path does this (grpc_handler.go handleWrite); the beam
			// path did not, so an upload into a not-yet-created sub-server dir
			// (server import: .upload.zip lands before setup makes the dir)
			// failed at temp-file creation. Mirror it here.
			if err := mkdirParentIn(upRoot, destName); err != nil {
				return status.Errorf(codes.Internal, "create dir: %v", err)
			}

			// A negative size would subtract from what every other upload is
			// counted against.
			if p.Start.TotalSize < 0 {
				return status.Error(codes.InvalidArgument, "the upload size cannot be negative")
			}
			// Every upload of this server and of this account that is still in
			// progress counts against the same headroom and quota as this one.
			// Reserved before checking, so two Starts at once cannot both see
			// room that only one of them has.
			serverTotal, releaseServer := s.reserveUpload("s:"+serverUUID, p.Start.TotalSize)
			defer releaseServer()
			userTotal, releaseUser := s.reserveUpload("u:"+username, p.Start.TotalSize)
			defer releaseUser()

			// Disk-quota pre-check. The beam tunnel bypasses Core's HTTP body
			// size cap and its disk precheck, so enforce the same server disk
			// limit here against the declared upload size before streaming.
			if err := s.checkBeamUploadDiskHeadroom(ctx, serverUUID, serverTotal); err != nil {
				return err
			}

			// Absolute per-upload size cap (admin-configured, server-wide).
			if err := s.checkBeamUploadSizeCap(ctx, p.Start.TotalSize); err != nil {
				return err
			}

			// Per-user daily upload quota (admin-configured). Best-effort
			// pre-check against today's counter; the counter is bumped by the
			// final on-disk size on successful completion below.
			if username != "" {
				if err := s.checkBeamDailyQuota(ctx, username, userTotal); err != nil {
					return err
				}
			}

			if uploadID != "" {
				// Stable temp name so a follow-up Start with the same id
				// reattaches to the same file. RDWR so we don't truncate
				// what previous chunks already wrote; O_CREATE so the
				// very first Start makes it.
				tmpName = path.Join(path.Dir(destName), ".beam-upload-"+uploadID)
				f, err := upRoot.OpenFile(tmpName, os.O_RDWR|os.O_CREATE, 0644)
				if err != nil {
					return status.Errorf(codes.Internal, "open temp: %v", err)
				}
				tmpFile = f
				if info, statErr := f.Stat(); statErr == nil && info.Size() > 0 {
					log.Printf("beam-server: upload %s resumed at offset %d (%s)", uploadID, info.Size(), path.Base(tmpName))
				}
			} else {
				// No uploadID → legacy single-shot path. Random suffix,
				// dropped on any non-clean exit.
				f, name, err := createTempIn(upRoot, path.Dir(destName), ".beam-upload-", "")
				if err != nil {
					return status.Errorf(codes.Internal, "create temp: %v", err)
				}
				tmpFile = f
				tmpName = name
			}

		case *pb.BeamUploadMsg_Chunk:
			if tmpFile == nil {
				return status.Errorf(codes.FailedPrecondition, "no upload started")
			}

			// Hard ceiling on actual bytes: the disk, size-cap and quota
			// pre-checks all ran against declaredSize, so a client must not be
			// able to declare small and then stream past it. A truthful client
			// (offsets 0..TotalSize) is never affected. This is what binds the
			// beam path the way MaxBytesReader binds the core HTTP path.
			if p.Chunk.Offset+int64(len(p.Chunk.Data)) > declaredSize {
				return status.Errorf(codes.ResourceExhausted,
					"upload exceeds its declared size of %d bytes", declaredSize)
			}

			// Throttle: this is the upload direction (client → disk).
			if err := s.throttle.WaitN(ctx, DirectionUp, len(p.Chunk.Data)); err != nil {
				return status.Errorf(codes.Canceled, "throttle: %v", err)
			}

			// WriteAt is idempotent on identical offsets, so a client that
			// resends the last (failed) chunk on resume produces the same
			// bytes — no corruption risk.
			if _, err := tmpFile.WriteAt(p.Chunk.Data, p.Chunk.Offset); err != nil {
				return status.Errorf(codes.Internal, "write chunk: %v", err)
			}
		}
	}

	// EOF reached cleanly — promote temp to final.
	if tmpFile != nil && destName != "" {
		// Same reason as the SFTP write: handed to the container's uid so a file
		// uploaded into a running server is one that server can modify. On the
		// open file, not by name.
		if mcUser() != 0 {
			if err := tmpFile.Chown(mcUser(), mcUser()); err != nil {
				log.Printf("mc-user: cannot hand %s to uid %d: %v", tmpName, mcUser(), err)
			}
		}
		// Close before rename so Windows doesn't reject (no-op on Linux).
		tmpFile.Close()
		tmpFile = nil // skip the defer's removal — rename consumed it
		if err := upRoot.Rename(tmpName, destName); err != nil {
			upRoot.Remove(tmpName)
			return stream.SendAndClose(&pb.BeamOpResp{Success: false, Message: err.Error()})
		}
		// Count the completed upload against the user's daily quota by the final
		// on-disk size, so a resumed multi-session upload is counted once.
		if fi, statErr := upRoot.Stat(destName); statErr == nil {
			s.recordBeamDailyUsage(ctx, username, fi.Size())
		}
	}

	s.auditBeam(ctx, serverUUID, "write", auditPath)
	return stream.SendAndClose(&pb.BeamOpResp{Success: true, Message: "uploaded"})
}

// serverDiskGauge reads the advisory per-server disk gauge
// (dylaris:server:<uuid>:stats:disk, JSON {total,limit}). Returns (0,0) on a nil
// client, a missing key, or an unparseable value, which every caller treats as
// "no known limit". Shared by the beam upload, SFTP and SaveFileContent paths so
// they all read the gauge the same way.
func serverDiskGauge(ctx context.Context, rdb *redis.Client, serverUUID string) (total, limit int64) {
	if rdb == nil {
		return 0, 0
	}
	raw, err := rdb.Get(ctx, fmt.Sprintf("dylaris:server:%s:stats:disk", serverUUID)).Result()
	if err != nil {
		return 0, 0
	}
	var disk struct {
		Total int64 `json:"total"`
		Limit int64 `json:"limit"`
	}
	if json.Unmarshal([]byte(raw), &disk) != nil {
		return 0, 0
	}
	return disk.Total, disk.Limit
}

// checkBeamUploadDiskHeadroom rejects an upload that would push the server past
// its disk limit. A missing gauge / no rdb / a non-positive limit means "no known
// limit" and the upload proceeds: the gauge is advisory, and refusing on its
// absence would break every upload whenever stats have not been published yet.
func (s *beamServer) checkBeamUploadDiskHeadroom(ctx context.Context, serverUUID string, incoming int64) error {
	total, limit := serverDiskGauge(ctx, s.rdb, serverUUID)
	if beamUploadExceedsDisk(total, limit, incoming) {
		return status.Errorf(codes.ResourceExhausted,
			"disk limit reached: %d of %d bytes used, upload is %d bytes", total, limit, incoming)
	}
	return nil
}

// beamUploadExceedsDisk reports whether adding `incoming` bytes to `total` would
// exceed `limit`. A non-positive limit means "no limit".
func beamUploadExceedsDisk(total, limit, incoming int64) bool {
	if limit <= 0 {
		return false
	}
	return total+incoming > limit
}

// The upload size cap + per-user daily quota live in the shared dylaris-pkg/beam/quota
// package so the node (this beam path) and core (the browser HTTP path) enforce
// the identical limits against the identical per-user/day Redis bucket. These
// methods only translate the shared decision into a gRPC status error; the keys,
// thresholds and fail-open reads are defined once in that package.

// checkBeamUploadSizeCap rejects a single upload whose declared size exceeds the
// admin-configured absolute per-upload cap.
func (s *beamServer) checkBeamUploadSizeCap(ctx context.Context, incoming int64) error {
	if ok, capBytes := quota.CheckSizeCap(ctx, s.rdb, incoming); !ok {
		return status.Errorf(codes.ResourceExhausted,
			"upload of %d bytes exceeds the %d byte per-upload limit", incoming, *capBytes)
	}
	return nil
}

// checkBeamDailyQuota rejects an upload that would push the user's bytes uploaded
// today past the admin-configured daily limit.
func (s *beamServer) checkBeamDailyQuota(ctx context.Context, username string, incoming int64) error {
	if ok, used, limit := quota.CheckDailyQuota(ctx, s.rdb, username, incoming); !ok {
		return status.Errorf(codes.ResourceExhausted,
			"daily upload quota reached: %d of %d bytes used today, upload is %d bytes", used, *limit, incoming)
	}
	return nil
}

// recordBeamDailyUsage adds `n` bytes to the user's shared daily upload counter.
func (s *beamServer) recordBeamDailyUsage(ctx context.Context, username string, n int64) {
	quota.RecordDailyUsage(ctx, s.rdb, username, n)
}

func (s *beamServer) DownloadSelective(req *pb.BeamSelectiveReq, stream grpc.ServerStreamingServer[pb.BeamChunk]) error {
	if err := s.requireFilePerm(stream.Context(), canRead, "download"); err != nil {
		return err
	}
	serverUUID := s.extractServerUUID(stream.Context())
	root, baseName, err := s.jailBeam(req.BasePath, serverUUID, "read")
	if err != nil {
		return status.Error(codes.PermissionDenied, err.Error())
	}
	defer root.Close()
	if !req.SelectAll && len(req.Selected) == 0 {
		return status.Error(codes.InvalidArgument, "no paths selected")
	}
	serverDir := s.storageMgr.GetServerDir(serverUUID)
	basePath := filepath.Join(serverDir, filepath.FromSlash(baseName))

	return s.streamZip(stream, zipNameFor(basePath), func(zw *zip.Writer) error {
		if req.SelectAll {
			return addTreeToZip(zw, root, baseName, baseName)
		}
		for _, sel := range req.Selected {
			// Containment per entry: `selected` is client-supplied, so each one
			// is re-anchored under basePath rather than trusted.
			selPath, err := resolveWithinDir(basePath, sel)
			if err != nil {
				continue
			}
			selName, err := rootName(serverDir, selPath)
			if err != nil {
				continue
			}
			// Names stay relative to the base so the archive reproduces the
			// layout the user selected, not an absolute tree.
			if err := addTreeToZip(zw, root, baseName, selName); err != nil {
				return err
			}
		}
		return nil
	})
}

// zipNameFor derives the archive name the client saves under, matching the
// control-plane path in grpc_handler.go so both transports name it the same.
func zipNameFor(path string) string {
	if base := filepath.Base(path); base != "." && base != string(filepath.Separator) && base != "" {
		return base + ".zip"
	}
	return "download.zip"
}

// addTreeToZip writes target (a file or a whole directory, a name inside root)
// into zw with names relative to nameBase. Same walker and entry writer as the
// control-plane zip paths in grpc_handler.go, so both transports archive
// exactly the same thing.
func addTreeToZip(zw *zip.Writer, root *os.Root, nameBase, target string) error {
	return walkRoot(root, target, func(name string, info fs.FileInfo) error {
		rel := relTo(nameBase, name)
		if rel == "." {
			return nil
		}
		return addZipEntry(zw, root, name, rel, info)
	})
}

// streamZip builds an archive with writeEntries and streams it out in chunks.
//
// The zip is produced through an io.Pipe and forwarded as it is written, so a
// multi-gigabyte world folder never lands in memory or in a temp file. TotalSize
// stays 0 because a streamed archive has no known length up front; the client
// treats 0 as "unknown" and reports bytes-loaded only.
func (s *beamServer) streamZip(stream grpc.ServerStreamingServer[pb.BeamChunk], filename string, writeEntries func(*zip.Writer) error) error {
	ctx := stream.Context()
	pr, pw := io.Pipe()

	go func() {
		zw := zip.NewWriter(pw)
		err := writeEntries(zw)
		// Close the zip writer first either way: it flushes the central
		// directory, and skipping that on the error path would leave the reader
		// blocked on a pipe that never ends.
		if cerr := zw.Close(); err == nil {
			err = cerr
		}
		pw.CloseWithError(err)
	}()
	// Unblocks the producer if the client goes away mid-transfer; without it the
	// goroutine would sit in pw.Write until the whole tree had been walked.
	defer pr.Close()

	buf := make([]byte, beamChunkSize)
	var offset int64
	first := true

	for {
		n, readErr := pr.Read(buf)
		if n > 0 {
			if err := s.throttle.WaitN(ctx, DirectionDown, n); err != nil {
				return status.Errorf(codes.Canceled, "throttle: %v", err)
			}
			chunk := &pb.BeamChunk{Data: buf[:n], Offset: offset}
			if first {
				chunk.Filename = filename
				first = false
			}
			if err := stream.Send(chunk); err != nil {
				return err
			}
			offset += int64(n)
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return status.Errorf(codes.Internal, "zip stream: %v", readErr)
		}
	}
}

// ─── Quota ───────────────────────────────────────────────────────────

func (s *beamServer) GetTransferQuota(ctx context.Context, req *pb.BeamQuotaReq) (*pb.BeamQuotaResp, error) {
	// BwLimit on the wire is a single number, so report the lower of the
	// two directions (treating 0 as unlimited). Clients only use this for
	// display hints; the actual enforcement is per-direction now.
	up := s.throttle.UpLimit()
	down := s.throttle.DownLimit()
	limit := up
	if down > 0 && (limit == 0 || down < limit) {
		limit = down
	}

	// Daily upload accounting, when configured and the caller's ticket carried a
	// username. A missing config key, counter, or username reads as 0 (unlimited
	// / none), matching the enforcement path's fail-open behavior.
	var dailyUsed, dailyLimit int64
	if s.rdb != nil {
		if v, err := s.rdb.Get(ctx, quota.DailyUploadBytesKey).Int64(); err == nil {
			dailyLimit = v
		}
		if username := s.extractUsername(ctx); username != "" {
			if v, err := s.rdb.Get(ctx, quota.DailyKey(username, time.Now())).Int64(); err == nil {
				dailyUsed = v
			}
		}
	}

	return &pb.BeamQuotaResp{
		DailyUsed:  dailyUsed,
		DailyLimit: dailyLimit,
		BwLimit:    limit,
	}, nil
}

// ─── Helpers ─────────────────────────────────────────────────────────

// extractServerUUID returns the server-UUID that Authenticate stashed for
// this gRPC peer. One Beam.exe session = one TCP connection from Link to
// the Node = one stable peer address, so this is reliable for the
// lifetime of a session.
//
// Returns "" when the peer hasn't authenticated yet — validateBeamPath
// then refuses with "server_uuid required" which surfaces upstream as a
// PermissionDenied gRPC error.
func (s *beamServer) extractServerUUID(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p == nil || p.Addr == nil {
		return ""
	}
	if !s.sessionLive(ctx, p.Addr.String()) {
		return ""
	}
	v, ok := s.serverUUIDByPeer.Load(p.Addr.String())
	if !ok {
		return ""
	}
	uuid, _ := v.(string)
	return uuid
}

// extractUsername returns the ticket username Authenticate stashed for this
// peer, or "" when none was stored (an empty-username ticket, or not yet
// authenticated). A "" result disables the per-user daily quota rather than
// bucketing distinct users together.
func (s *beamServer) extractUsername(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p == nil || p.Addr == nil {
		return ""
	}
	v, ok := s.usernameByPeer.Load(p.Addr.String())
	if !ok {
		return ""
	}
	name, _ := v.(string)
	return name
}

// copyDir and copyFile are defined in installer.go — reused here.
