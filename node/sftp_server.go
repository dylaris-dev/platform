package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dylaris-pkg/beam/quota"
	"dylaris-pkg/fileperms"
	"dylaris-pkg/queue"

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

	// Open connections, in total and per remote address. Every accepted
	// connection held a goroutine and a descriptor with no deadline and no
	// count, and each handshake costs an RSA-4096 signature - on a machine
	// that runs other tenants' servers.
	connMu  sync.Mutex
	conns   int
	perAddr map[string]int

	// Bytes written through open SFTP handles and not yet booked against the
	// account's quota, per account. Shared by every handle of the account so
	// twenty uploads at once cannot each spend the same remaining allowance.
	pendingMu sync.Mutex
	pending   map[string]*atomic.Int64
}

const (
	sftpMaxConns         = 256
	sftpMaxConnsPerAddr  = 16
	sftpHandshakeTimeout = 30 * time.Second
	// Failed sign-ins before a source address, or one account from one address,
	// is turned away for the 15-minute window.
	sftpMaxFailsPerAddr     = 30
	sftpMaxFailsPerUserAddr = 10
	// Across all addresses: one account guessed from many addresses (an IPv6
	// prefix holds billions) is still bounded. High enough that locking
	// someone out this way takes real effort.
	sftpMaxFailsPerUser = 100
)

// sftpRecheckInterval is how often an open session re-reads its credential and
// its server list. A variable for tests.
var sftpRecheckInterval = 30 * time.Second

// sftpDummyHash is compared against when the account is unknown, so an unknown
// name costs the same bcrypt round as a wrong password.
var sftpDummyHash, _ = bcrypt.GenerateFromPassword([]byte("dylaris-sftp-no-such-account"), bcrypt.DefaultCost)

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
	return &SFTPServer{rdb: rdb, storageMgr: storageMgr, nodeID: nodeID,
		perAddr: map[string]int{}, pending: map[string]*atomic.Int64{}}
}

// admit counts a new connection from addr, or refuses it when the node or that
// address already holds its share.
func (s *SFTPServer) admit(addr string) bool {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.conns >= sftpMaxConns || s.perAddr[addr] >= sftpMaxConnsPerAddr {
		return false
	}
	s.conns++
	s.perAddr[addr]++
	return true
}

func (s *SFTPServer) release(addr string) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	s.conns--
	if s.perAddr[addr]--; s.perAddr[addr] <= 0 {
		delete(s.perAddr, addr)
	}
}

// pendingFor is the account's shared count of written, unbooked bytes.
func (s *SFTPServer) pendingFor(username string) *atomic.Int64 {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	p := s.pending[username]
	if p == nil {
		p = &atomic.Int64{}
		s.pending[username] = p
	}
	return p
}

// remoteAddr is the host part of a connection's peer address, as the unit the
// limits count by: an IPv4 address, or an IPv6 /64 - one subscriber holds a
// whole /64, so counting single IPv6 addresses counts nothing.
func remoteAddr(a net.Addr) string {
	if a == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		host = a.String()
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return host
}

func (s *SFTPServer) Start(ctx context.Context, port string) {
	hostKey, err := s.loadOrGenHostKey()
	if err != nil {
		log.Printf("SFTP: failed to load/generate host key: %v", err)
		return
	}

	config := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			return s.authUser(c.User(), string(pass), remoteAddr(c.RemoteAddr()))
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			return s.authKey(c.User(), key)
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
		addr := remoteAddr(conn.RemoteAddr())
		if !s.admit(addr) {
			conn.Close()
			continue
		}
		go func() {
			defer s.release(addr)
			s.handleConn(conn, config)
		}()
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

// sftpFailAddrKey counts failed sign-ins from one source address.
func sftpFailAddrKey(nodeID, addr string) string {
	return fmt.Sprintf("dylaris:node:%s:sftp_fail_addr:%s", nodeID, addr)
}

// authUser checks a password sign-in.
//
// The lockout used to count per account alone: anyone could keep any account
// locked out with ten wrong passwords every fifteen minutes, while spraying one
// password across every name went unthrottled, and an unknown name answered at
// once where a known one cost bcrypt and half a second - which told a stranger
// which accounts have something on this node. It now counts per address and per
// account-from-that-address, and an unknown name takes the same path as a wrong
// password.
func (s *SFTPServer) authUser(username, password, addr string) (*ssh.Permissions, error) {
	// Before anything is read or compared: on a platform whose file access is
	// beam-only there is no SFTP, and a password check that can succeed is not
	// "no SFTP". See sftpEnabled in main.go for what was measured.
	if !sftpEnabled() {
		return nil, fmt.Errorf("SFTP is disabled on this platform")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	failKey := sftpFailKey(s.nodeID, username+"@"+addr)
	addrKey := sftpFailAddrKey(s.nodeID, addr)
	userKey := sftpFailKey(s.nodeID, username)
	n, _ := s.rdb.Get(ctx, failKey).Int()
	na, _ := s.rdb.Get(ctx, addrKey).Int()
	nu, _ := s.rdb.Get(ctx, userKey).Int()
	if n >= sftpMaxFailsPerUserAddr || na >= sftpMaxFailsPerAddr || nu >= sftpMaxFailsPerUser {
		time.Sleep(time.Second)
		return nil, fmt.Errorf("too many failed attempts, try again later")
	}

	hash, err := s.rdb.Get(ctx, sftpAuthKey(s.nodeID, username)).Result()
	known := err == nil
	if !known {
		hash = string(sftpDummyHash)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil || !known {
		s.recordAuthFail(ctx, failKey)
		s.recordAuthFail(ctx, addrKey)
		s.recordAuthFail(ctx, userKey)
		time.Sleep(500 * time.Millisecond) // slow down credential stuffing
		return nil, fmt.Errorf("invalid credentials")
	}
	s.rdb.Del(ctx, failKey) // success clears this account's counter for this address
	return &ssh.Permissions{Extensions: map[string]string{"username": username, "method": "password", "cred": hash}}, nil
}

// sftpKeysKey holds the authorized_keys lines an account may sign in with,
// published per node by Core's SFTP sync.
func sftpKeysKey(nodeID, username string) string {
	return fmt.Sprintf("sftp:node:%s:keys:%s", nodeID, username)
}

// sftpAccountKeys reads an account's published keys. A read error is returned
// as such, so a caller can tell "no keys" from "could not ask".
func (s *SFTPServer) sftpAccountKeys(ctx context.Context, username string) ([]string, error) {
	data, err := s.rdb.Get(ctx, sftpKeysKey(s.nodeID, username)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lines []string
	if err := json.Unmarshal([]byte(data), &lines); err != nil {
		return nil, err
	}
	return lines, nil
}

// authKey checks a public-key sign-in: the key must be one the account added in
// the panel. An offered key that is not one of them is not a failed guess -
// clients offer every key they hold before falling back to a password - so it
// is not counted toward the lockout.
func (s *SFTPServer) authKey(username string, key ssh.PublicKey) (*ssh.Permissions, error) {
	if !sftpEnabled() {
		return nil, fmt.Errorf("SFTP is disabled on this platform")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lines, err := s.sftpAccountKeys(ctx, username)
	if err != nil {
		return nil, fmt.Errorf("invalid credentials")
	}
	if line, ok := matchAuthorizedKey(lines, key); ok {
		return &ssh.Permissions{Extensions: map[string]string{"username": username, "method": "publickey", "cred": line}}, nil
	}
	return nil, fmt.Errorf("invalid credentials")
}

// matchAuthorizedKey returns the line among lines that is key.
func matchAuthorizedKey(lines []string, key ssh.PublicKey) (string, bool) {
	want := key.Marshal()
	for _, l := range lines {
		pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(l))
		if err == nil && bytes.Equal(pk.Marshal(), want) {
			return l, true
		}
	}
	return "", false
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
	// A handshake that never finishes held its goroutine and descriptor forever.
	conn.SetDeadline(time.Now().Add(sftpHandshakeTimeout))
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		conn.Close()
		return
	}
	conn.SetDeadline(time.Time{})
	defer sshConn.Close()

	username := sshConn.Permissions.Extensions["username"]
	servers, _ := s.getUserServers(username)
	set := &atomic.Pointer[sftpServerSet]{}
	set.Store(newServerSet(servers))
	audit := newSFTPAudit(username, remoteAddr(conn.RemoteAddr()))

	// The server list and the credential were read once, at sign-in, and the
	// session then ran on that snapshot for as long as the client stayed - a
	// revoked grant, a reset password, a removed key or a deleted account kept
	// working until the client hung up. Re-read on an interval; a credential
	// that is gone or changed ends the connection.
	done := make(chan struct{})
	defer close(done)
	go s.watchSession(sshConn, set, audit, done)

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
		go s.handleSession(channel, requests, set, audit, username)
	}
}

// watchSession re-checks an open connection every sftpRecheckInterval until
// done, and flushes its audit record as it goes and at the end.
func (s *SFTPServer) watchSession(conn *ssh.ServerConn, set *atomic.Pointer[sftpServerSet], audit *sftpAudit, done <-chan struct{}) {
	t := time.NewTicker(sftpRecheckInterval)
	defer t.Stop()
	defer s.publishAudit(audit)
	username := conn.Permissions.Extensions["username"]
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		if !s.credentialStillValid(conn.Permissions) {
			log.Printf("SFTP: closing %s's session: the sign-in it was opened with is no longer valid", username)
			conn.Close()
			return
		}
		if servers, ok := s.getUserServers(username); ok {
			set.Store(newServerSet(servers))
		}
		s.publishAudit(audit)
	}
}

// credentialStillValid reports whether the credential a session signed in with
// is still the one Core publishes. A Redis error answers true: the session is
// not ended for a fault this node cannot attribute, and Core's own 5-minute TTL
// still bounds it.
func (s *SFTPServer) credentialStillValid(p *ssh.Permissions) bool {
	if p == nil || !sftpEnabled() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	username, cred := p.Extensions["username"], p.Extensions["cred"]
	switch p.Extensions["method"] {
	case "password":
		hash, err := s.rdb.Get(ctx, sftpAuthKey(s.nodeID, username)).Result()
		if errors.Is(err, redis.Nil) {
			return false
		}
		return err != nil || hash == cred
	case "publickey":
		lines, err := s.sftpAccountKeys(ctx, username)
		if err != nil {
			return true
		}
		for _, l := range lines {
			if l == cred {
				return true
			}
		}
		return false
	}
	return false
}

func (s *SFTPServer) handleSession(channel ssh.Channel, requests <-chan *ssh.Request, set *atomic.Pointer[sftpServerSet], audit *sftpAudit, username string) {
	defer channel.Close()

	for req := range requests {
		if req.Type == "subsystem" && len(req.Payload) >= 4 {
			name := string(req.Payload[4:])
			if name == "sftp" {
				if req.WantReply {
					req.Reply(true, nil)
				}
				vfs := &virtualFS{set: set, storageMgr: s.storageMgr, rdb: s.rdb, username: username,
					audit: audit, pending: s.pendingFor(username)}
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

// getUserServers reads the account's server list. ok is false when Redis could
// not be asked, which is not the same as "no servers": a missing list is an
// answer (access withdrawn), a failed read is not.
func (s *SFTPServer) getUserServers(username string) (servers []sftpServerRef, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	key := fmt.Sprintf("sftp:node:%s:user:%s", s.nodeID, username)
	data, err := s.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return nil, true
	}
	if err != nil {
		return nil, false
	}
	json.Unmarshal([]byte(data), &servers)
	return servers, true
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
	// set is the session's current server list, replaced by watchSession as
	// Core republishes it.
	set        *atomic.Pointer[sftpServerSet]
	storageMgr *StorageManager
	// rdb + username let Filewrite meter writes against the upload limits and
	// attribute them to the user's shared daily-quota bucket. The SFTP login
	// username is the account username (sftp_sync.go keys sftp:auth by it), the
	// same identity the beam/HTTP upload paths use, so all three share one bucket.
	rdb      *redis.Client
	username string
	// audit collects what this connection changed; pending is the account's
	// shared count of unbooked written bytes. Either may be nil.
	audit   *sftpAudit
	pending *atomic.Int64
}

// sftpServerSet is a server list with the names a session addresses them by.
type sftpServerSet struct {
	servers   []sftpServerRef
	nameToRef map[string]sftpServerRef
}

// newServerSet names each server for the virtual root. Names are not unique -
// two servers can share one, and an owner renames theirs freely - and the last
// one used to win, so a server someone invited you to could stand in for your
// own of the same name and take your uploads. A shared name, or one with a
// slash in it, is shown with the start of the server's id instead.
func newServerSet(servers []sftpServerRef) *sftpServerSet {
	count := map[string]int{}
	for _, s := range servers {
		count[s.Name]++
	}
	out := &sftpServerSet{nameToRef: make(map[string]sftpServerRef, len(servers))}
	for _, s := range servers {
		name := s.Name
		if count[name] > 1 || name == "" || strings.ContainsAny(name, "/\\") {
			short := s.UUID
			if len(short) > 8 {
				short = short[:8]
			}
			name = strings.NewReplacer("/", "_", "\\", "_").Replace(name) + " (" + short + ")"
		}
		s.Name = name
		out.servers = append(out.servers, s)
		out.nameToRef[name] = s
	}
	return out
}

func newVirtualFS(servers []sftpServerRef, sm *StorageManager, rdb *redis.Client, username string) *virtualFS {
	set := &atomic.Pointer[sftpServerSet]{}
	set.Store(newServerSet(servers))
	return &virtualFS{set: set, storageMgr: sm, rdb: rdb, username: username}
}

// current is the server list as of now.
func (v *virtualFS) current() *sftpServerSet { return v.set.Load() }

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
	ref, ok := v.current().nameToRef[serverName]
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

// openRoot opens the server directory resolve landed in as an os.Root. The
// operation then runs through it, so it stays inside the directory even if a
// name changes between resolve's check and the operation (see rootfs.go).
// Files opened through the Root stay valid after it is closed.
func (v *virtualFS) openRoot(ref sftpServerRef) (*os.Root, error) {
	return os.OpenRoot(v.storageMgr.GetServerDir(ref.UUID))
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
	root, err := v.openRoot(ref)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(filepath.ToSlash(rel))
	if err != nil {
		return nil, err
	}
	return &guardedReader{f: f, allowed: v.still(ref.UUID, func(r sftpServerRef) bool { return r.Read })}, nil
}

// still reports, at the moment it is called, whether the session's current
// server list holds uuid with the permission need checks. A handle opened
// before a grant was withdrawn kept reading or writing through its open file
// for as long as the client held it; each operation now asks this first.
func (v *virtualFS) still(uuid string, need func(sftpServerRef) bool) func() bool {
	return func() bool {
		for _, r := range v.current().servers {
			if r.UUID == uuid {
				return need(r)
			}
		}
		return false
	}
}

// guardedReader is a read handle that stops when its server leaves the list.
type guardedReader struct {
	f       *os.File
	allowed func() bool
}

func (g *guardedReader) ReadAt(p []byte, off int64) (int, error) {
	if !g.allowed() {
		return 0, os.ErrPermission
	}
	return g.f.ReadAt(p, off)
}

func (g *guardedReader) Close() error { return g.f.Close() }

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
	root, leaf, err := writeScope(v.storageMgr.GetServerDir(ref.UUID), filepath.ToSlash(rel), false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// The client's flags decide. Every open used to truncate, so a resumed
	// upload, an in-place edit over sshfs or a read-write open emptied the file
	// before the first byte was written.
	pf := r.Pflags()
	flags := os.O_WRONLY | os.O_CREATE
	if pf.Trunc {
		flags |= os.O_TRUNC
	}
	if pf.Excl {
		flags |= os.O_EXCL
	}
	f, err := root.OpenFile(leaf, flags, 0644)
	if err != nil {
		return nil, err
	}
	v.audit.note(ref.UUID, "write", rel)
	// The node writes as root and the server runs as uid 1000, so a file
	// uploaded into a RUNNING server would be one the server can read and not
	// modify - which surfaces days later as a plugin that cannot save its own
	// config. The start-time pass repairs this, but only at the next start.
	// On the open file, not by name.
	if mcUser() != 0 {
		if err := f.Chown(mcUser(), mcUser()); err != nil {
			log.Printf("mc-user: cannot hand %s to uid %d: %v", rel, mcUser(), err)
		}
	}
	// SFTP is a streaming protocol with no declared size, so the upload limits
	// are enforced per write against a ceiling computed once here, and the
	// written bytes count toward the user's daily quota on close.
	// What is already in the file is not new: a resumed upload or an in-place
	// edit is metered from the size the file had when it was opened.
	var baseline int64
	if !pf.Trunc {
		if st, err := f.Stat(); err == nil {
			baseline = st.Size()
		}
	}
	lim := sftpWriteLimits(context.Background(), v.rdb, v.serverUUIDForPath(r.Filepath), v.username)
	return &meteredSFTPWriter{f: f, ceil: lim.disk, reason: "server disk limit", fileCap: lim.file, dailyCap: lim.daily,
		baseline: baseline, maxEnd: baseline, rdb: v.rdb, username: v.username, pending: v.pending,
		allowed: v.still(ref.UUID, func(r sftpServerRef) bool { return r.Write })}, nil
}

// serverUUIDForPath returns the server UUID a virtual path targets, or "" for the
// virtual root / an unknown server name.
func (v *virtualFS) serverUUIDForPath(path string) string {
	path = filepath.ToSlash(filepath.Clean("/" + path))
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
	return v.current().nameToRef[parts[0]].UUID
}

// sftpLimits are the three upload limits apart, because they count different
// things: the size cap is per FILE, the disk headroom per SERVER, the daily
// quota per ACCOUNT. Folded into one number, the account-wide count of open
// handles was held against a per-file cap. -1 = no limit (file and daily are
// nil instead, as meteredSFTPWriter reads them).
type sftpLimits struct {
	file  *int64
	disk  int64
	daily *int64
}

func sftpWriteLimits(ctx context.Context, rdb *redis.Client, serverUUID, username string) sftpLimits {
	lim := sftpLimits{disk: -1}
	if rdb == nil {
		return lim
	}
	clamp := func(v int64) int64 {
		if v < 0 {
			return 0
		}
		return v
	}
	if capBytes := quota.MaxUploadCap(ctx, rdb); capBytes != nil {
		v := clamp(*capBytes)
		lim.file = &v
	}
	if total, limit := serverDiskGauge(ctx, rdb, serverUUID); limit > 0 {
		lim.disk = clamp(limit - total)
	}
	if used, limit := quota.DailyUsage(ctx, rdb, username); limit != nil {
		v := clamp(*limit - used)
		lim.daily = &v
	}
	return lim
}

// meteredSFTPWriter wraps the destination file so streaming SFTP writes obey the
// upload limits (enforced per write, since there is no declared size) and count
// toward the user's shared daily quota when the transfer closes.
type meteredSFTPWriter struct {
	f *os.File
	// ceil bounds the bytes this handle ADDS (end offset past baseline); < 0 =
	// unlimited. reason names it in the error.
	ceil   int64
	reason string
	// fileCap bounds the resulting file size; dailyCap the bytes the account
	// adds today, across every open handle (see pending). nil = none.
	fileCap, dailyCap *int64
	// baseline is the file's size when opened (0 on a truncating open); maxEnd
	// the largest end offset since. What the handle added is maxEnd-baseline.
	baseline, maxEnd int64
	rdb              *redis.Client
	username         string
	// pending is the account's added-but-unbooked bytes across every open
	// handle. Usage is booked on close, so twenty handles opened at once each
	// had the whole remaining daily quota. nil = this handle alone.
	pending *atomic.Int64
	// allowed reports whether the session may still write to this server.
	allowed func() bool
	mu      sync.Mutex
}

func (m *meteredSFTPWriter) added() int64 {
	if d := m.maxEnd - m.baseline; d > 0 {
		return d
	}
	return 0
}

func (m *meteredSFTPWriter) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.allowed != nil && !m.allowed() {
		return 0, os.ErrPermission
	}
	end := off + int64(len(p))
	before := m.added()
	after := before
	if d := end - m.baseline; d > after {
		after = d
	}
	if m.fileCap != nil && end > *m.fileCap {
		return 0, fmt.Errorf("SFTP write refused: per-upload size limit exceeded")
	}
	if m.ceil >= 0 && after > m.ceil {
		return 0, fmt.Errorf("SFTP write refused: %s exceeded", m.reason)
	}
	if m.dailyCap != nil {
		others := int64(0)
		if m.pending != nil {
			others = m.pending.Load() - before
		}
		if others+after > *m.dailyCap {
			return 0, fmt.Errorf("SFTP write refused: daily upload quota exceeded")
		}
	}
	n, err := m.f.WriteAt(p, off)
	if e := off + int64(n); e > m.maxEnd {
		m.maxEnd = e
		if m.pending != nil {
			m.pending.Add(m.added() - before)
		}
	}
	return n, err
}

func (m *meteredSFTPWriter) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	quota.RecordDailyUsage(context.Background(), m.rdb, m.username, m.added())
	if m.pending != nil {
		m.pending.Add(-m.added())
	}
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
		root, leaf, err := writeScope(v.storageMgr.GetServerDir(ref.UUID), filepath.ToSlash(rel), false)
		if err != nil {
			return err
		}
		defer root.Close()
		if err := root.Mkdir(leaf, 0755); err != nil {
			return err
		}
		// Files written over SFTP are handed over; a directory was not, and the
		// server could create nothing in it.
		chownForMCIn(root, leaf)
		v.audit.note(ref.UUID, "mkdir", rel)
		return nil
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
		root, leaf, err := writeScope(v.storageMgr.GetServerDir(ref.UUID), filepath.ToSlash(rel), false)
		if err != nil {
			return err
		}
		defer root.Close()
		if err := root.Remove(leaf); err != nil {
			return err
		}
		v.audit.note(ref.UUID, "delete", rel)
		return nil
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
		// A rename runs inside ONE Root, so it cannot span two servers. It used
		// to, as a plain path rename; moving files between servers is still
		// possible as a download and an upload.
		if srcRef.UUID != dstRef.UUID {
			return os.ErrPermission
		}
		if err := renameNoFollow(v.storageMgr.GetServerDir(srcRef.UUID), filepath.ToSlash(srcRel), filepath.ToSlash(dstRel)); err != nil {
			return err
		}
		v.audit.note(srcRef.UUID, "rename", srcRel+" -> "+dstRel)
		return nil
	case "Setstat":
		// A size is a truncate, and it was answered "done" without happening.
		// Mode, owner and times are still ignored: the node owns those.
		if !r.AttrFlags().Size {
			return nil
		}
		realPath, rel, ref, err := v.resolve(r.Filepath)
		if err != nil || realPath == "" || !ref.Write || protectedRel(rel) {
			return os.ErrPermission
		}
		root, leaf, err := writeScope(v.storageMgr.GetServerDir(ref.UUID), filepath.ToSlash(rel), false)
		if err != nil {
			return err
		}
		defer root.Close()
		f, err := root.OpenFile(leaf, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		// Shrinking only. Growing a file this way passes no upload limit at
		// all - "truncate -s 1T" made a terabyte the server then counted.
		st, err := f.Stat()
		if err != nil {
			return err
		}
		size := int64(r.Attributes().Size)
		if size > st.Size() {
			return os.ErrPermission
		}
		if err := f.Truncate(size); err != nil {
			return err
		}
		v.audit.note(ref.UUID, "write", rel)
		return nil
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
			cur := v.current()
			entries := make([]os.FileInfo, 0, len(cur.servers))
			for _, s := range cur.servers {
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
		root, err := v.openRoot(ref)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		entries, err := fs.ReadDir(root.FS(), filepath.ToSlash(rel))
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
		root, err := v.openRoot(ref)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		fi, err := root.Lstat(filepath.ToSlash(rel))
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

// sftpAudit collects what one connection changed, per server, for the server
// audit trail. The panel's file operations were recorded there; the same ones
// over SFTP were not.
type sftpAudit struct {
	username, addr string
	via            string // "" = sftp
	mu             sync.Mutex
	byServer       map[string]*queue.SFTPAuditRecord
}

// sftpAuditMaxPaths bounds the paths one record carries.
const sftpAuditMaxPaths = 50

func newSFTPAudit(username, addr string) *sftpAudit {
	return &sftpAudit{username: username, addr: addr, byServer: map[string]*queue.SFTPAuditRecord{}}
}

// note records one change. A nil receiver records nothing.
func (a *sftpAudit) note(serverUUID, kind, rel string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	rec := a.byServer[serverUUID]
	if rec == nil {
		rec = &queue.SFTPAuditRecord{ServerUUID: serverUUID, Username: a.username, RemoteIP: a.addr, Via: a.via}
		a.byServer[serverUUID] = rec
	}
	switch kind {
	case "write":
		rec.Writes++
	case "delete":
		rec.Deletes++
	case "rename":
		rec.Renames++
	case "mkdir":
		rec.Mkdirs++
	}
	if len(rec.Paths) < sftpAuditMaxPaths {
		rec.Paths = append(rec.Paths, filepath.ToSlash(rel))
	} else {
		rec.Truncated = true
	}
}

// take hands over what was collected and starts again.
func (a *sftpAudit) take() []*queue.SFTPAuditRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*queue.SFTPAuditRecord, 0, len(a.byServer))
	for _, r := range a.byServer {
		out = append(out, r)
	}
	a.byServer = map[string]*queue.SFTPAuditRecord{}
	return out
}

// publishAudit sends what the connection changed since the last flush to Core.
func (s *SFTPServer) publishAudit(a *sftpAudit) { publishSFTPAudit(s.rdb, a) }

// publishSFTPAudit sends what a has collected to Core, on this node's channel.
func publishSFTPAudit(rdb *redis.Client, a *sftpAudit) {
	if a == nil || rdb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, rec := range a.take() {
		data, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		if err := rdb.Publish(ctx, queue.SFTPAuditChannel(nodeID), data).Err(); err != nil {
			log.Printf("SFTP: audit record for %s could not be sent: %v", rec.ServerUUID, err)
		}
	}
}
