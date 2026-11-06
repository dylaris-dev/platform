package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dylaris-pkg/protocol"
	pb "dylaris-proto/beam"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	// Side-effect import registers the gzip compressor so call sites can
	// opt in with grpc.UseCompressor("gzip").
	_ "google.golang.org/grpc/encoding/gzip"
)

// alreadyCompressed lists file extensions whose payloads gain nothing from
// gRPC-level gzip. Saves CPU on both ends — and a stalled gzip stream is
// worse than the uncompressed baseline. Lowercased lookup.
var alreadyCompressed = map[string]bool{
	".jar":  true,
	".zip":  true,
	".7z":   true,
	".rar":  true,
	".gz":   true,
	".tgz":  true,
	".bz2":  true,
	".xz":   true,
	".png":  true,
	".jpg":  true,
	".jpeg": true,
	".webp": true,
	".mp4":  true,
	".webm": true,
	".mp3":  true,
	".ogg":  true,
	// Minecraft's own: region files hold zlib chunks, NBT and schematics are
	// gzip.
	".mca":       true,
	".mcr":       true,
	".dat":       true,
	".nbt":       true,
	".schem":     true,
	".schematic": true,
	".litematic": true,
}

// downloadCallOpts picks the compression for one download: none for a folder
// (the node zips it), none on the LAN fast path, else by extension.
func downloadCallOpts(name string, isDir, lan bool) []grpc.CallOption {
	if isDir || lan {
		return nil
	}
	return compressorForPath(name)
}

// compressorForPath returns the gRPC call options to use for downloading
// `name`. Text-like content gets gzip; pre-compressed binaries pass through
// raw to avoid spending CPU on negative-return compression.
func compressorForPath(name string) []grpc.CallOption {
	ext := strings.ToLower(filepath.Ext(name))
	if alreadyCompressed[ext] {
		return nil
	}
	return []grpc.CallOption{grpc.UseCompressor("gzip")}
}

// BeamNodeClient holds an authenticated gRPC connection to a Node's BeamNodeService
// routed through the BeamRelay tunnel.
type BeamNodeClient struct {
	conn       *grpc.ClientConn
	svc        pb.BeamNodeServiceClient
	ServerUUID string
	// lan is set for the LAN fast path. There bandwidth is not the limit and
	// gzip is: one core compressing on the node capped a 10 Gbit LAN at about
	// 40 MB/s, so downloads go uncompressed.
	lan bool
}

// ConnectBeamNode dials the relay, authenticates with the JWT ticket, and
// returns a ready BeamNodeClient. If relayAddr has no port, :25550 is used.
// relayTLSSkipVerify reports whether to skip relay TLS verification. Defaults
// to false (verify the relay certificate). Set BEAM_TLS_SKIP_VERIFY=true for
// dev relays that present a self-signed certificate.
//
// It says so out loud when it is on. This is the only place in the beam path
// where transport authentication can be turned OFF, and the hop it disables is
// the one carrying the beam ticket. Its two siblings are both louder about it:
// ConnectBeamNodeDirect below refuses to dial at all without a pin, and Core
// logs a WARNING for its own GRPC_TLS_ENABLED=false. This one was silent, on a
// desktop app where the variable can be inherited from a shell, a shortcut or a
// user service and then persist invisibly for the life of the install.
var skipVerifyWarnOnce sync.Once

func relayTLSSkipVerify() bool {
	v := os.Getenv("BEAM_TLS_SKIP_VERIFY")
	skip := v == "true" || v == "1"
	if skip {
		skipVerifyWarnOnce.Do(func() {
			log.Println("WARNING: BEAM_TLS_SKIP_VERIFY is set; the relay's TLS certificate is NOT verified. " +
				"Anyone able to intercept the connection can read and alter this session, including the beam ticket. " +
				"Unset it unless you are deliberately talking to a dev relay with a self-signed certificate.")
		})
	}
	return skip
}

// beamCallLimits lifts gRPC's 4 MiB receive default: the node returns a file of
// up to 10 MiB in one ReadFileContent reply, and ListFiles is not paged, so a
// large folder's listing is one message too.
var beamCallLimits = grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64 << 20))

func ConnectBeamNode(relayAddr, ticket string) (*BeamNodeClient, error) {
	if _, _, err := net.SplitHostPort(relayAddr); err != nil {
		relayAddr = net.JoinHostPort(relayAddr, "25550")
	}

	conn, err := grpc.NewClient("passthrough:///beam-node",
		beamCallLimits,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			// Bound the dial: without a timeout a dropped SYN (firewall
			// DROP rather than REJECT) hangs until the RPC deadline,
			// surfacing as an opaque "DeadlineExceeded".
			d := &net.Dialer{Timeout: 10 * time.Second}
			tlsConn, err := tls.DialWithDialer(d, "tcp", relayAddr, &tls.Config{
				InsecureSkipVerify: relayTLSSkipVerify(),
			})
			if err != nil {
				return nil, fmt.Errorf("relay dial: %w", err)
			}
			if err := protocol.WriteBeamHeader(tlsConn, ticket); err != nil {
				tlsConn.Close()
				return nil, fmt.Errorf("beam header: %w", err)
			}
			return tlsConn, nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc: %w", err)
	}

	svc := pb.NewBeamNodeServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := svc.Authenticate(ctx, &pb.BeamAuthReq{Ticket: ticket})
	if err != nil {
		conn.Close()
		// Raw gRPC dial failures embed the OS socket error, which on
		// Windows is localized (German, etc.). The clean message keyed
		// off the gRPC status code keeps the user-visible string
		// English; the underlying err is appended verbatim so admins
		// (with Developer Mode on) can read what actually failed —
		// DNS, RST, TLS handshake, etc. — instead of guessing.
		//   Unavailable      → port refused / relay down / dial fail
		//   DeadlineExceeded → packets dropped (firewall) or TLS stalled
		switch status.Code(err) {
		case codes.Unavailable, codes.DeadlineExceeded:
			return nil, fmt.Errorf("could not reach the Beam relay at %s — it may be offline, or its port is closed or blocked by a firewall (underlying: %v)", relayAddr, err)
		default:
			return nil, fmt.Errorf("relay authentication failed: %v", err)
		}
	}
	if !resp.Ok {
		conn.Close()
		return nil, fmt.Errorf("auth rejected: %s", resp.Message)
	}

	return &BeamNodeClient{conn: conn, svc: svc, ServerUUID: resp.ServerUuid}, nil
}

// ConnectBeamNodeDirect dials a Node's BeamNodeService DIRECTLY over the LAN and
// authenticates with the same JWT ticket. This is the LAN fast-path. The hop is
// TLS-encrypted and the node's self-signed certificate is PINNED by the
// fingerprint Core derived from the shared beam secret + node ID — so a passive
// sniffer learns nothing and an active MITM (no matching cert) is rejected. The
// JWT ticket still gates auth and is node+server bound, so this grants no more
// than the relay path.
func ConnectBeamNodeDirect(addr, ticket, fingerprint string) (*BeamNodeClient, error) {
	if fingerprint == "" {
		return nil, fmt.Errorf("missing LAN cert fingerprint; refusing unpinned direct dial")
	}
	want, err := hex.DecodeString(fingerprint)
	if err != nil {
		return nil, fmt.Errorf("bad fingerprint: %w", err)
	}

	// Pin by leaf-cert SHA-256. We skip the normal chain/hostname checks (the
	// node serves a deterministic self-signed cert, not a CA-issued one) and
	// instead require the presented leaf to match the expected fingerprint
	// exactly — equivalent to certificate pinning.
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS13, // node serves an ed25519 leaf (TLS 1.3)
		InsecureSkipVerify: true,             // chain/name checks replaced by the pin below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("no certificate presented")
			}
			sum := sha256.Sum256(rawCerts[0])
			if subtle.ConstantTimeCompare(sum[:], want) != 1 {
				return fmt.Errorf("LAN certificate fingerprint mismatch (possible MITM); refusing")
			}
			return nil
		},
	}

	conn, err := grpc.NewClient("passthrough:///beam-node-lan",
		beamCallLimits,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, "tcp", addr)
		}),
		// A fixed 16 MiB receive window instead of gRPC's BDP-estimated one,
		// which starts at 64 KiB. A download is one stream on one connection,
		// so the window is what paces it; over TLS on loopback this took a
		// 2 GiB stream from ~0.95 to ~1.2 GB/s. 16 MiB is also the ceiling the
		// estimator grows to, so the memory a stream may pin is unchanged.
		grpc.WithInitialWindowSize(16<<20),
		grpc.WithInitialConnWindowSize(16<<20),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc: %w", err)
	}

	svc := pb.NewBeamNodeServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := svc.Authenticate(ctx, &pb.BeamAuthReq{Ticket: ticket})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("lan auth: %w", err)
	}
	if !resp.Ok {
		conn.Close()
		return nil, fmt.Errorf("auth rejected: %s", resp.Message)
	}
	return &BeamNodeClient{conn: conn, svc: svc, ServerUUID: resp.ServerUuid}, nil
}

// isPrivateLANIP reports whether ip is a private/link-local LAN address the beam
// client may direct-dial: RFC1918 IPv4 (10/8, 172.16/12, 192.168/16) and IPv6
// ULA (fc00::/7) via IsPrivate, plus IPv4 link-local (169.254/16, RFC3927) and
// IPv6 link-local (fe80::/10) via IsLinkLocalUnicast. Public, unspecified, and
// multicast addresses are rejected so a poisoned ticket can never point the
// client at an off-LAN (deanonymizing) host.
func isPrivateLANIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// filterPrivateLANIPs keeps only the entries of ips that parse as a
// private/link-local LAN address (see isPrivateLANIP), dropping and logging the
// rest. Core already hard-filters LAN hints to RFC1918 before signing the
// ticket, but this is a client-side backstop: a poisoned or MITM'd ticket must
// never make the app dial an arbitrary public target.
func filterPrivateLANIPs(ips []string) []string {
	out := make([]string, 0, len(ips))
	for _, raw := range ips {
		if ip := net.ParseIP(strings.TrimSpace(raw)); ip != nil && isPrivateLANIP(ip) {
			out = append(out, raw)
			continue
		}
		fmt.Fprintf(os.Stderr, "beam-app: dropping non-private LAN hint %q\n", raw)
	}
	return out
}

// probeBeamLAN returns the first ip:port that accepts a TCP connection within a short
// budget, or "" if none do. A cheap reachability check run before the gRPC dial so an
// unreachable LAN hint costs at most one budget before we fall back to the relay.
//
// The dials run CONCURRENTLY so the total probe cost stays within one budget instead
// of len(ips) x budget: the preference chain now probes LAN on EVERY connect (even
// when a relay is present), so a serial probe of several unreachable hints would
// delay the relay fallback unacceptably.
func probeBeamLAN(ips []string, port string) string {
	// The sole caller (dialLANFastpath) always supplies the pinned-TLS beam port
	// (BEAM_LAN_PORT, default 25523). An empty port fails safe - no LAN match, so the
	// connect chain falls back to the relay - rather than probing a hard-coded port.
	if port == "" || len(ips) == 0 {
		return ""
	}
	const budget = 700 * time.Millisecond
	found := make(chan string, len(ips)) // buffered so late successes never block
	var wg sync.WaitGroup
	for _, ip := range ips {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			conn, err := net.DialTimeout("tcp", addr, budget)
			if err == nil {
				conn.Close()
				// found is buffered to len(ips) and each goroutine sends at most once,
				// so this send never blocks.
				found <- addr
			}
		}(net.JoinHostPort(ip, port))
	}
	// Close found once every dial has settled so a range/receive terminates when
	// nothing is reachable. Each dial is capped at `budget`, so this resolves in
	// ~budget regardless of how many IPs are unreachable.
	go func() {
		wg.Wait()
		close(found)
	}()
	addr, ok := <-found
	if !ok {
		return "" // channel closed with no success
	}
	return addr
}

func (c *BeamNodeClient) Close() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

// Ping does a lightweight RPC roundtrip to verify the tunnel is alive.
// Used by the App's background health-check goroutine to detect relay
// drops and reconnect to a fresh one before the user notices. Returns
// the gRPC error verbatim so callers can decide how to react.
func (c *BeamNodeClient) Ping(ctx context.Context) error {
	_, err := c.svc.GetTransferQuota(ctx, &pb.BeamQuotaReq{})
	return err
}

// ─── File Operations ──────────────────────────────────────────────────

func (c *BeamNodeClient) ListFiles(path string) (*FileListResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.svc.ListFiles(ctx, &pb.BeamFileListReq{Path: path})
	if err != nil {
		return nil, err
	}

	files := make([]FileEntry, len(resp.Files))
	for i, f := range resp.Files {
		files[i] = FileEntry{Name: f.Name, IsDir: f.IsDir, Size: f.Size}
	}
	return &FileListResult{Success: true, Files: files}, nil
}

func (c *BeamNodeClient) GetFileContent(path string) (*FileContentResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.svc.ReadFileContent(ctx, &pb.BeamFileReadReq{Path: path})
	if err != nil {
		return nil, err
	}
	return &FileContentResult{Success: resp.Success, Content: resp.Content, Message: resp.Message}, nil
}

func (c *BeamNodeClient) SaveFile(path, content string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.svc.SaveFileContent(ctx, &pb.BeamFileSaveReq{Path: path, Content: content})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

func (c *BeamNodeClient) CreateFile(path string, isDir bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := c.svc.CreateFile(ctx, &pb.BeamFileCreateReq{Path: path, IsDir: isDir})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

func (c *BeamNodeClient) DeleteFile(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := c.svc.DeleteFile(ctx, &pb.BeamFileDeleteReq{Path: path})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

func (c *BeamNodeClient) RenameFile(oldPath, newName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := c.svc.RenameFile(ctx, &pb.BeamFileRenameReq{OldPath: oldPath, NewName: newName})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

func (c *BeamNodeClient) CopyFile(srcPath, dstPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.svc.CopyFile(ctx, &pb.BeamFileCopyReq{SrcPath: srcPath, DstPath: dstPath})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// ─── Streaming Uploads ───────────────────────────────────────────────

// UploadSession is a single in-flight chunked upload. Created by
// UploadStart, fed with WriteChunk, completed (or aborted) with Finish
// or Cancel. The gRPC stream is held open between calls so the Node
// can stream chunks straight into its temp file — no buffering, no
// HTTP body limit on the Core to worry about.
type UploadSession struct {
	stream pb.BeamNodeService_UploadFileClient
	cancel context.CancelFunc
}

// UploadStart opens a chunked upload stream and sends the BeamUploadStart
// header. Returns a session the caller drives via WriteChunk + Finish.
// No timeout on the underlying context — uploads can be arbitrarily large
// and the Node enforces its own bandwidth throttle.
//
// uploadID is attached as gRPC metadata (header "x-beam-upload-id") so
// the Node can keep the temp file across stream drops keyed by id. An
// empty uploadID falls back to legacy non-resumable behaviour (random
// temp suffix, drop on disconnect). Resuming an upload is just calling
// UploadStart again with the SAME uploadID — the Node reattaches to the
// existing temp file at its current size, and subsequent WriteChunk
// calls with explicit offsets fill in / overwrite as needed.
func (c *BeamNodeClient) UploadStart(uploadID, path, filename, strategy string, totalSize int64) (*UploadSession, error) {
	ctx, cancel := context.WithCancel(context.Background())
	if uploadID != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-beam-upload-id", uploadID)
	}
	stream, err := c.svc.UploadFile(ctx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open upload stream: %w", err)
	}
	if err := stream.Send(&pb.BeamUploadMsg{
		Payload: &pb.BeamUploadMsg_Start{
			Start: &pb.BeamUploadStart{
				Path:      path,
				Filename:  filename,
				TotalSize: totalSize,
				Strategy:  strategy,
			},
		},
	}); err != nil {
		cancel()
		return nil, fmt.Errorf("send start: %w", err)
	}
	return &UploadSession{stream: stream, cancel: cancel}, nil
}

func (s *UploadSession) WriteChunk(data []byte, offset int64) error {
	err := s.stream.Send(&pb.BeamUploadMsg{
		Payload: &pb.BeamUploadMsg_Chunk{
			Chunk: &pb.BeamUploadChunkData{Data: data, Offset: offset},
		},
	})
	// gRPC client-streaming reports a server-side early termination as a bare
	// io.EOF on Send — the real terminating status (e.g. ResourceExhausted
	// "daily upload quota reached ...", or a transient Unavailable) is only
	// retrievable via CloseAndRecv. Fetch it so the caller sees the actual
	// reason instead of an EOF that reads like a network blip and gets retried.
	if err == io.EOF {
		if _, recvErr := s.stream.CloseAndRecv(); recvErr != nil {
			return recvErr
		}
	}
	return err
}

func (s *UploadSession) Finish() error {
	resp, err := s.stream.CloseAndRecv()
	s.cancel()
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

func (s *UploadSession) Cancel() {
	s.cancel()
}

// ─── Streaming Downloads ──────────────────────────────────────────────

// beamDownloadIdleTimeout bounds how long a download may go with NO bytes
// before the client gives up. Activity resets it, so only a transfer that has
// stopped making progress is torn down.
//
// It replaces a fixed 10-minute deadline on the whole transfer. Nothing else on
// this path caps a transfer by the clock: UploadStart deliberately builds a
// plain context.WithCancel because "uploads can be arbitrarily large", and the
// Core REST download fallback deliberately uses a client with no timeout
// because "large files can take a while". Only the beam path had the cap - the
// path built FOR multi-GB transfers, and the one an admin can bandwidth-cap via
// the relay throttle, where 600 seconds times the cap is the largest file that
// can ever arrive. Past that the download died at the same byte every attempt,
// and writeChunksToFile restarts from zero, so retrying could not get through
// it. Mirrors the relay's own beamSpliceIdleTimeout, which bounds the same
// session from the other end on the same principle.
const beamDownloadIdleTimeout = 5 * time.Minute

// errDownloadStalled is what writeChunksToFile returns when its idle watchdog
// ended the call, so the retry loop can tell it from a refusal.
var errDownloadStalled = errors.New("download stalled")

func (c *BeamNodeClient) DownloadFile(ctx context.Context, path string, isDir bool, savePath string, onProgress func(loaded, total int64)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Directory downloads are server-zipped — already compressed by the time
	// they hit the wire — so skip gzip there. Single files get the
	// extension-based heuristic.
	stream, err := c.svc.DownloadFile(ctx, &pb.BeamDownloadReq{Path: path, ZipIfDir: isDir}, downloadCallOpts(path, isDir, c.lan)...)
	if err != nil {
		return err
	}
	return writeChunksToFile(cancel, beamDownloadIdleTimeout, stream, savePath, onProgress)
}

func (c *BeamNodeClient) SelectiveDownload(ctx context.Context, basePath string, selected []string, selectAll bool, savePath string, onProgress func(loaded, total int64)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Selective downloads always produce a zip on the server side — no point
	// asking gRPC to re-compress.
	stream, err := c.svc.DownloadSelective(ctx, &pb.BeamSelectiveReq{
		BasePath:  basePath,
		Selected:  selected,
		SelectAll: selectAll,
	})
	if err != nil {
		return err
	}
	return writeChunksToFile(cancel, beamDownloadIdleTimeout, stream, savePath, onProgress)
}

// chunkStream is satisfied by both DownloadFile and DownloadSelective streams.
type chunkStream interface {
	Recv() (*pb.BeamChunk, error)
}

// writeChunksToFile drains a download stream to savePath, cancelling the call
// via cancel when no chunk has arrived for idle. The caller owns the context
// and passes its cancel func here so the watchdog can end the RPC; see
// beamDownloadIdleTimeout for why the bound is on idleness and not on the
// transfer's total duration.
func writeChunksToFile(cancel context.CancelFunc, idle time.Duration, stream chunkStream, savePath string, onProgress func(loaded, total int64)) error {
	// Ensure target directory exists
	if err := os.MkdirAll(filepath.Dir(savePath), 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	f, err := os.Create(savePath)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	defer f.Close()

	// Idle watchdog. stalled distinguishes "we gave up" from a real stream
	// error, because the cancel surfaces on Recv as an opaque context error
	// that reads like a network fault.
	var lastChunk atomic.Int64
	var stalled atomic.Bool
	lastChunk.Store(time.Now().UnixNano())
	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go func() {
		t := time.NewTicker(idle / 2)
		defer t.Stop()
		for {
			select {
			case <-watchdogDone:
				return
			case <-t.C:
				if time.Since(time.Unix(0, lastChunk.Load())) > idle {
					stalled.Store(true)
					cancel()
					return
				}
			}
		}
	}()

	var totalSize int64
	var loaded int64

	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			if stalled.Load() {
				return fmt.Errorf("%w: no data received for %s", errDownloadStalled, idle)
			}
			return err
		}
		lastChunk.Store(time.Now().UnixNano())
		if len(chunk.Data) > 0 {
			if _, err := f.Write(chunk.Data); err != nil {
				return err
			}
			if chunk.TotalSize > 0 && totalSize == 0 {
				totalSize = chunk.TotalSize
			}
			loaded += int64(len(chunk.Data))
			if onProgress != nil {
				onProgress(loaded, totalSize)
			}
		}
	}
	return nil
}
