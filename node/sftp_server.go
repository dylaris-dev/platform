package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dylaris-pkg/beam/quota"
	"dylaris-pkg/fileperms"

	"github.com/pkg/sftp"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"
)

// SFTPServer exposes server directories to users via SSH/SFTP.
// Auth is Redis-based: sftp:auth:{username} = bcrypt hash.
// Server list per user: sftp:node:{nodeID}:user:{username} = [{uuid,name}].
// Failed-auth counter: see sftpFailKey.
type SFTPServer struct {
	rdb        *redis.Client
	storageMgr *StorageManager
	nodeID     string
}

type sftpServerRef struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
	// What this account may do to this server's files, published per (node,
	// user) by Core's SFTP sync from the same resolution the HTTP file API
	// enforces. Flat keys, and ABSENT means false: an entry written by a Core
	// that predates them refuses every operation rather than allowing every
	// one, and the next 60s tick replaces it.
	//
	// Until these existed the node asked one question - is this account allowed
	// an SFTP session at all - and then permitted everything through it. The
	// built-in Builder role is defined as write-but-not-delete, so an account
	// invited as a Builder was refused a delete over HTTP and could remove
	// server.jar here.
	fileperms.Perms
}

func NewSFTPServer(rdb *redis.Client, storageMgr *StorageManager, nodeID string) *SFTPServer {
	return &SFTPServer{rdb: rdb, storageMgr: storageMgr, nodeID: nodeID}
}

func (s *SFTPServer) Start(ctx context.Context, port string) {
	hostKey, err := s.loadOrGenHostKey()
	if err != nil {
		log.Printf("SFTP: failed to load/generate host key: %v", err)
		return
	}

	config := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			return s.authUser(c.User(), string(pass))
		},
	}
	config.AddHostKey(hostKey)

	listener, err := net.Listen("tcp", ":"+strings.TrimPrefix(port, ":"))
	if err != nil {
		log.Printf("SFTP: listen error on port %s: %v", port, err)
		return
	}
	defer listener.Close()

	log.Printf("SFTP server listening on port %s", port)

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				log.Printf("SFTP: accept error: %v", err)
				continue
			}
		}
		go s.handleConn(conn, config)
	}
}

// sftpFailKey is the per-username failed-auth counter, scoped to this node.
//
// It used to be a bare "sftp:fail:<username>", which is outside every pattern
// Core grants this node's Redis ACL user (core/services/redisacl/rules.go:
// sftp:auth:* and sftp:node:<token>:* are granted, that third namespace was
// not). Under mandatory ACL every GET/INCR/DEL on it therefore answered NOPERM
// - and all three call sites discarded the error, so the counter read as 0
// forever and the lockout below could never trigger. The whole guard was inert
// with nothing in any log to say so.
//
// Node-scoped rather than fleet-global on purpose. The alternative fix was a
// global grant on that namespace, which would hand every node in the fleet -
// BYON machines the tenant owns included - write access to a brute-force
// counter it is itself subject to. A node that can zero the counter can also
// erase the guard, so the scope that makes the grant free is the scope worth
// having: each node counts the attempts made against its own SFTP listener.
func sftpFailKey(nodeID, username string) string {
	return fmt.Sprintf("dylaris:node:%s:sftp_fail:%s", nodeID, username)
}

func (s *SFTPServer) authUser(username, password string) (*ssh.Permissions, error) {
	// Before anything is read or compared: on a platform whose file access is
	// beam-only there is no SFTP, and a password check that can succeed is not
	// "no SFTP". See sftpEnabled in main.go for what was measured.
	if !sftpEnabled() {
		return nil, fmt.Errorf("SFTP is disabled on this platform")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Per-username lockout to blunt brute force: after 10 failures in the
	// sliding 15min window, reject outright (with a small delay).
	failKey := sftpFailKey(s.nodeID, username)
	if n, _ := s.rdb.Get(ctx, failKey).Int(); n >= 10 {
		time.Sleep(time.Second)
		return nil, fmt.Errorf("too many failed attempts, try again later")
	}

	hash, err := s.rdb.Get(ctx, sftpAuthKey(s.nodeID, username)).Result()
	if err != nil {
		s.recordAuthFail(ctx, failKey)
		return nil, fmt.Errorf("user not found")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		s.recordAuthFail(ctx, failKey)
		time.Sleep(500 * time.Millisecond) // slow down credential stuffing
		return nil, fmt.Errorf("invalid password")
	}
	s.rdb.Del(ctx, failKey) // success clears the counter
	return &ssh.Permissions{Extensions: map[string]string{"username": username}}, nil
}

// recordAuthFail bumps the per-username failure counter and (re)arms its
// sliding 15min TTL in one round-trip.
func (s *SFTPServer) recordAuthFail(ctx context.Context, key string) {
	pipe := s.rdb.Pipeline()
	pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, 15*time.Minute)
	if _, err := pipe.Exec(ctx); err != nil {
		// Was discarded, which is how the key being outside this node's ACL grant
		// stayed invisible for as long as it did. A counter that cannot be written
		// is a lockout that cannot fire, so say it rather than fail open quietly.
		log.Printf("SFTP: failed-auth counter %s could not be updated, the lockout is not counting: %v", key, err)
	}
}

func (s *SFTPServer) handleConn(conn net.Conn, config *ssh.ServerConfig) {
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer sshConn.Close()

	username := sshConn.Permissions.Extensions["username"]
	servers := s.getUserServers(username)

	go ssh.DiscardRequests(reqs)

	for ch := range chans {
		if ch.ChannelType() != "session" {
			ch.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}
		channel, requests, err := ch.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(channel, requests, servers, username)
	}
}

func (s *SFTPServer) handleSession(channel ssh.Channel, requests <-chan *ssh.Request, servers []sftpServerRef, username string) {
	defer channel.Close()

	for req := range requests {
		if req.Type == "subsystem" && len(req.Payload) >= 4 {
			name := string(req.Payload[4:])
			if name == "sftp" {
				if req.WantReply {
					req.Reply(true, nil)
				}
				vfs := newVirtualFS(servers, s.storageMgr, s.rdb, username)
				handlers := sftp.Handlers{
					FileGet:  vfs,
					FilePut:  vfs,
					FileCmd:  vfs,
					FileList: vfs,
				}
				sftpSrv := sftp.NewRequestServer(channel, handlers)
				sftpSrv.Serve()
				return
			}
		}
		if req.WantReply {
			req.Reply(false, nil)
		}
	}
}

func (s *SFTPServer) getUserServers(username string) []sftpServerRef {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	key := fmt.Sprintf("sftp:node:%s:user:%s", s.nodeID, username)
	data, err := s.rdb.Get(ctx, key).Result()
	if err != nil {
		return nil
	}
	var servers []sftpServerRef
	json.Unmarshal([]byte(data), &servers)
	return servers
}

// legacySFTPHostKeyPath is the pre-STORAGE_PATHS location, relative to the
// working directory. Inside the container that is /app/data, which is on no
// volume, so a key kept there is regenerated on every recreate and every client
// gets a host-key-changed warning.
const legacySFTPHostKeyPath = "data/sftp_host_key"

// sftpHostKeyPath puts the host key next to the node identity on the first
// storage path, which is the one directory a node is already required to keep
// across recreates. Falls back to the legacy location only if that is unset.
func sftpHostKeyPath() string {
	if nodeSecretDir == "" {
		return filepath.FromSlash(legacySFTPHostKeyPath)
	}
	return filepath.Join(nodeSecretDir, "sftp_host_key")
}

// parseHostKey returns a signer for a PEM-encoded RSA key, or nil if the bytes
// are not one.
func parseHostKey(data []byte) ssh.Signer {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil
	}
	return signer
}

func (s *SFTPServer) loadOrGenHostKey() (ssh.Signer, error) {
	return loadOrGenHostKeyAt(sftpHostKeyPath(), filepath.FromSlash(legacySFTPHostKeyPath))
}

// loadOrGenHostKeyAt resolves the host key with both locations passed in, so the
// adoption path can be tested without touching the real legacy directory.
//
// The order is what keeps a client's known_hosts entry valid: an existing key at
// the persistent location wins, then a key still in the legacy location is
// ADOPTED and copied across (an upgrading node must not change its fingerprint),
// and only a node that has neither generates one.
func loadOrGenHostKeyAt(keyPath, legacyPath string) (ssh.Signer, error) {
	os.MkdirAll(filepath.Dir(keyPath), 0700)

	if data, err := os.ReadFile(keyPath); err == nil {
		if signer := parseHostKey(data); signer != nil {
			return signer, nil
		}
	}

	// Adopt a key still sitting in the legacy location so upgrading a node does
	// not change its fingerprint, and persist it where it will actually survive.
	if legacyPath != keyPath {
		if data, err := os.ReadFile(legacyPath); err == nil {
			if signer := parseHostKey(data); signer != nil {
				if err := os.WriteFile(keyPath, data, 0600); err != nil {
					log.Printf("SFTP: could not migrate host key to %s: %v", keyPath, err)
				}
				return signer, nil
			}
		}
	}

	// Generate new RSA key
	key, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return nil, err
	}
	pemData := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.WriteFile(keyPath, pemData, 0600); err != nil {
		log.Printf("SFTP: could not persist host key: %v", err)
	}
	return ssh.NewSignerFromKey(key)
}

// ==========================================
// Virtual FS
// ==========================================

// virtualFS maps root-level entries to server directories.
// /{serverName}/... → storageMgr.GetServerDir(uuid)/...
type virtualFS struct {
	servers    []sftpServerRef
	storageMgr *StorageManager
	nameToRef  map[string]sftpServerRef
	// rdb + username let Filewrite meter writes against the upload limits and
	// attribute them to the user's shared daily-quota bucket. The SFTP login
	// username is the account username (sftp_sync.go keys sftp:auth by it), the
	// same identity the beam/HTTP upload paths use, so all three share one bucket.
	rdb      *redis.Client
	username string
}

func newVirtualFS(servers []sftpServerRef, sm *StorageManager, rdb *redis.Client, username string) *virtualFS {
	m := make(map[string]sftpServerRef, len(servers))
	for _, s := range servers {
		m[s.Name] = s
	}
	return &virtualFS{servers: servers, storageMgr: sm, nameToRef: m, rdb: rdb, username: username}
}

// resolve converts a virtual path to a real OS path, and returns that path
// relative to the server directory alongside it.
// Returns ("", "", nil) for root, ("", "", os.ErrNotExist) for invalid paths.
//
// Callers guard on the RELATIVE path. isProtectedFile inspects every component
// precisely so ".dylaris-backups/<archive>.tar.gz" is caught; handing it only
// filepath.Base leaves "<archive>.tar.gz", an ordinary-looking filename, and
// every guard here passed exactly that. Listing already hid the directory, so
// an SFTP client could truncate, delete or move backups it could not see.
// resolve maps a virtual SFTP path to a real one, and returns the caller's
// permissions on the server it lands in.
//
// The permissions come back HERE, rather than being looked up by each handler,
// so that every path that reaches a file also holds the answer to "may they".
// A handler that forgets to ask is then visible as an unused value rather than
// as nothing at all - which is what the previous shape was: resolve returned a
// path, and Remove, Rmdir, Rename and Mkdir did their work with no permission
// in sight.
//
// The virtual root has no server and therefore no permissions; it lists the
// server names the sync published, which is what the account is entitled to see
// by definition.
func (v *virtualFS) resolve(path string) (string, string, sftpServerRef, error) {
	path = filepath.ToSlash(filepath.Clean("/" + path))
	if path == "/" {
		return "", "", sftpServerRef{}, nil // root — virtual
	}

	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
	serverName := parts[0]
	ref, ok := v.nameToRef[serverName]
	if !ok {
		return "", "", sftpServerRef{}, os.ErrNotExist
	}
	uuid := ref.UUID

	base := v.storageMgr.GetServerDir(uuid)
	if len(parts) == 1 {
		return base, ".", ref, nil
	}
	rel := filepath.FromSlash(parts[1])
	full := filepath.Join(base, rel)
	// SFTP builds its own paths rather than going through resolveWithinDir, so
	// it needs the symlink half of that guard restated here: Clean("/"+path)
	// above strips traversal from the STRING, and os.Open/os.OpenFile then
	// follow a planted link straight out of the server directory.
	if !linkStaysWithin(base, full) {
		return "", "", sftpServerRef{}, os.ErrPermission
	}
	return full, rel, ref, nil
}

// protectedRel reports whether a resolved SFTP path names a platform-managed
// entry. "." is the server directory itself, which every client stats while
// navigating, so it is not treated as protected here - the operations that
// could damage it (write, remove, rename) all fail on a non-empty directory.
func protectedRel(rel string) bool {
	return rel != "." && isProtectedFile(rel)
}

// --- sftp.ReadWriteAt (FileGet + FilePut) ---

func (v *virtualFS) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	realPath, rel, ref, err := v.resolve(r.Filepath)
	if err != nil || realPath == "" {
		return nil, os.ErrPermission
	}
	if !ref.Read {
		return nil, os.ErrPermission
	}
	if protectedRel(rel) {
		return nil, os.ErrPermission
	}
	return os.Open(realPath)
}

func (v *virtualFS) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	realPath, rel, ref, err := v.resolve(r.Filepath)
	if err != nil || realPath == "" {
		return nil, os.ErrPermission
	}
	if !ref.Write {
		return nil, os.ErrPermission
	}
	if protectedRel(rel) {
		return nil, os.ErrPermission
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	f, err := os.OpenFile(realPath, flags, 0644)
	if err != nil {
		return nil, err
	}
	// The node writes as root and the server runs as uid 1000, so a file
	// uploaded into a RUNNING server would be one the server can read and not
	// modify - which surfaces days later as a plugin that cannot save its own
	// config. The start-time pass repairs this, but only at the next start.
	chownForMC(realPath)
	// Without Redis there is nothing to meter against — behave as before.
	if v.rdb == nil {
		return f, nil
	}
	// SFTP is a streaming protocol with no declared size, so the upload limits
	// are enforced per write against a ceiling computed once here, and the
	// written bytes count toward the user's daily quota on close.
	ceil, reason := sftpWriteCeiling(context.Background(), v.rdb, v.serverUUIDForPath(r.Filepath), v.username)
	return &meteredSFTPWriter{f: f, ceil: ceil, reason: reason, rdb: v.rdb, username: v.username}, nil
}

// serverUUIDForPath returns the server UUID a virtual path targets, or "" for the
// virtual root / an unknown server name.
func (v *virtualFS) serverUUIDForPath(path string) string {
	path = filepath.ToSlash(filepath.Clean("/" + path))
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
	return v.nameToRef[parts[0]].UUID
}

// sftpWriteCeiling returns the largest end offset a write to this server dir may
// reach — the minimum of the enforced upload limits (per-upload size cap,
// remaining server disk headroom, remaining daily quota) — plus a label naming
// the tightest one. ceil < 0 means no limit is configured (writes unrestricted).
func sftpWriteCeiling(ctx context.Context, rdb *redis.Client, serverUUID, username string) (ceil int64, reason string) {
	ceil = -1
	consider := func(enforced bool, remaining int64, label string) {
		if !enforced {
			return
		}
		if remaining < 0 {
			remaining = 0 // already over -> ceiling 0 rejects any non-empty write
		}
		if ceil < 0 || remaining < ceil {
			ceil, reason = remaining, label
		}
	}
	// A nil cap is no cap. A cap of 0 is a real one and yields a ceiling of 0,
	// which rejects any non-empty write - that is the operator saying uploads are
	// not allowed, and it used to be indistinguishable from "no limit".
	if capBytes := quota.MaxUploadCap(ctx, rdb); capBytes != nil {
		consider(true, *capBytes, "per-upload size limit")
	}
	if total, limit := serverDiskGauge(ctx, rdb, serverUUID); limit > 0 {
		consider(true, limit-total, "server disk limit")
	}
	if used, limit := quota.DailyUsage(ctx, rdb, username); limit != nil {
		consider(true, *limit-used, "daily upload quota")
	}
	return ceil, reason
}

// meteredSFTPWriter wraps the destination file so streaming SFTP writes obey the
// upload limits (enforced per write, since there is no declared size) and count
// toward the user's shared daily quota when the transfer closes.
type meteredSFTPWriter struct {
	f        *os.File
	ceil     int64  // max allowed end offset; < 0 = unlimited
	reason   string // which limit set the ceiling, for the error
	maxEnd   int64  // largest end offset written = resulting file size
	rdb      *redis.Client
	username string
}

func (m *meteredSFTPWriter) WriteAt(p []byte, off int64) (int, error) {
	if m.ceil >= 0 && off+int64(len(p)) > m.ceil {
		return 0, fmt.Errorf("SFTP write refused: %s exceeded", m.reason)
	}
	n, err := m.f.WriteAt(p, off)
	if end := off + int64(n); end > m.maxEnd {
		m.maxEnd = end
	}
	return n, err
}

func (m *meteredSFTPWriter) Close() error {
	quota.RecordDailyUsage(context.Background(), m.rdb, m.username, m.maxEnd)
	return m.f.Close()
}

// --- sftp.FileCmder ---

func (v *virtualFS) Filecmd(r *sftp.Request) error {
	switch r.Method {
	case "Mkdir":
		realPath, rel, ref, err := v.resolve(r.Filepath)
		if err != nil || realPath == "" {
			return os.ErrPermission
		}
		// Creating a directory is a write, the same verb the HTTP API's create
		// endpoint asks for.
		if !ref.Write {
			return os.ErrPermission
		}
		if protectedRel(rel) {
			return os.ErrPermission
		}
		return os.Mkdir(realPath, 0755)
	// pkg/sftp reports RMDIR as its own method, and it was not handled at all:
	// it fell through to the "unsupported operation" default, so a client could
	// create a directory over SFTP and then never delete it. os.Remove is the
	// right call for both - it deletes a file, deletes an EMPTY directory, and
	// refuses a non-empty one, which is exactly the RMDIR contract. The guards
	// are the same either way, so the two share a branch rather than drift.
	case "Remove", "Rmdir":
		realPath, rel, ref, err := v.resolve(r.Filepath)
		if err != nil || realPath == "" {
			return os.ErrPermission
		}
		// files.delete, exactly as the HTTP delete endpoint asks. This is the
		// one the built-in Builder role does NOT hold, and until this check
		// existed a Builder was refused a delete over HTTP and allowed it here.
		if !ref.Delete {
			return os.ErrPermission
		}
		if protectedRel(rel) {
			return os.ErrPermission
		}
		return os.Remove(realPath)
	case "Rename":
		src, srcRel, srcRef, err := v.resolve(r.Filepath)
		if err != nil || src == "" {
			return os.ErrPermission
		}
		// files.write, matching the HTTP rename endpoint. Checked on BOTH ends
		// below, because the two paths can name different servers: this virtual
		// filesystem presents every server the account may reach as a
		// directory, so a rename across them is expressible.
		if !srcRef.Write {
			return os.ErrPermission
		}
		if protectedRel(srcRel) {
			return os.ErrPermission
		}
		// The destination was never checked, so a rename ONTO a protected name
		// overwrote it - the one hole the gRPC copy handler had always closed.
		dst, dstRel, dstRef, err := v.resolve(r.Target)
		if err != nil || dst == "" {
			return os.ErrPermission
		}
		if !dstRef.Write {
			return os.ErrPermission
		}
		if protectedRel(dstRel) {
			return os.ErrPermission
		}
		return os.Rename(src, dst)
	case "Setstat":
		return nil // ignore chmod/chown
	}
	return fmt.Errorf("unsupported operation: %s", r.Method)
}

// --- sftp.ListerAt (FileList) ---

type listerAt []os.FileInfo

func (l listerAt) ListAt(ls []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(ls, l[offset:])
	if n < len(ls) {
		return n, io.EOF
	}
	return n, nil
}

func (v *virtualFS) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	switch r.Method {
	case "List":
		realPath, rel, ref, err := v.resolve(r.Filepath)
		if err != nil {
			return nil, err
		}
		// files.read, matching the HTTP listing endpoint. The virtual ROOT is
		// exempt: it lists the server names the sync published for this account,
		// which is by definition what they are entitled to see, and refusing it
		// would make a session that may write but not read look empty rather
		// than restricted.
		if realPath != "" && !ref.Read {
			return nil, os.ErrPermission
		}
		// The virtual root is resolved BEFORE the protected check, and must be:
		// resolve returns rel "" for it, filepath.Clean("") is ".", and
		// isProtectedFile treats "." as the protected server root - so the
		// listing every SFTP client issues on connect came back ErrNotExist and
		// the whole session looked empty. Every other caller of protectedRel
		// already refuses the root on realPath == "" before reaching it; this
		// branch is the one that must serve it instead.
		if realPath == "" {
			// Root: list virtual server entries
			entries := make([]os.FileInfo, 0, len(v.servers))
			for _, s := range v.servers {
				base := v.storageMgr.GetServerDir(s.UUID)
				fi, err := os.Stat(base)
				if err != nil {
					fi = &virtualDirInfo{name: s.Name}
				} else {
					fi = &namedFileInfo{FileInfo: fi, name: s.Name}
				}
				entries = append(entries, fi)
			}
			return listerAt(entries), nil
		}
		// The per-entry filter below only hides a protected directory from its
		// PARENT's listing. Listing it by name still enumerated everything
		// inside, which for .dylaris-backups is every archive the server has.
		if protectedRel(rel) {
			return nil, os.ErrNotExist
		}
		entries, err := os.ReadDir(realPath)
		if err != nil {
			return nil, err
		}
		infos := make([]os.FileInfo, 0, len(entries))
		for _, e := range entries {
			if isProtectedFile(e.Name()) {
				continue
			}
			fi, err := e.Info()
			if err == nil {
				infos = append(infos, fi)
			}
		}
		return listerAt(infos), nil

	case "Stat", "Lstat":
		realPath, rel, ref, err := v.resolve(r.Filepath)
		if err != nil {
			return nil, err
		}
		if realPath == "" {
			return listerAt([]os.FileInfo{&virtualDirInfo{name: "/"}}), nil
		}
		// Not ErrPermission, for the same reason the protected check below is
		// not: a stat that answers differently for "exists" and "may not see"
		// is how a directory nobody may list is mapped anyway.
		if !ref.Read {
			return nil, os.ErrNotExist
		}
		// Not ErrPermission: the listing filter already hides these entries, so
		// confirming one exists would be the only way to learn it is there.
		if protectedRel(rel) {
			return nil, os.ErrNotExist
		}
		fi, err := os.Lstat(realPath)
		if err != nil {
			return nil, err
		}
		// If it's a server root (top-level), use display name
		path := filepath.ToSlash(filepath.Clean("/" + r.Filepath))
		if strings.Count(path, "/") == 1 {
			serverName := strings.TrimPrefix(path, "/")
			fi = &namedFileInfo{FileInfo: fi, name: serverName}
		}
		return listerAt([]os.FileInfo{fi}), nil
	}
	return nil, fmt.Errorf("unsupported list method: %s", r.Method)
}

// --- helper types ---

type virtualDirInfo struct{ name string }

func (d *virtualDirInfo) Name() string       { return d.name }
func (d *virtualDirInfo) Size() int64        { return 0 }
func (d *virtualDirInfo) Mode() os.FileMode  { return os.ModeDir | 0755 }
func (d *virtualDirInfo) ModTime() time.Time { return time.Time{} }
func (d *virtualDirInfo) IsDir() bool        { return true }
func (d *virtualDirInfo) Sys() interface{}   { return nil }

type namedFileInfo struct {
	os.FileInfo
	name string
}

func (n *namedFileInfo) Name() string { return n.name }
