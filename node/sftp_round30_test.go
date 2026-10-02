package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dylaris-pkg/fileperms"

	"github.com/alicebob/miniredis/v2"
	"github.com/pkg/sftp"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"
)

func sftpTestServer(t *testing.T) (*SFTPServer, *miniredis.Miniredis) {
	t.Helper()
	// SFTP served: other tests change these globals.
	origExternal := nodeExternal
	r, f, p, cp, io, pids := getModes()
	nodeExternal = false
	setModes(r, "sftp", p, cp, io, pids)
	t.Cleanup(func() { nodeExternal = origExternal; setModes(r, f, p, cp, io, pids) })
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return NewSFTPServer(rdb, nil, "node-1"), mr
}

func setHash(t *testing.T, mr *miniredis.Miniredis, user, password string) string {
	t.Helper()
	h, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	mr.Set(sftpAuthKey("node-1", user), string(h))
	return string(h)
}

// An unknown name answered at once and a wrong password after bcrypt and half a
// second, which told a stranger which accounts exist here; and the lockout was
// per account, so anyone could keep anyone locked out.
func TestSFTPPasswordSignInDoesNotTellNamesApartOrLockOthersOut(t *testing.T) {
	s, mr := sftpTestServer(t)
	setHash(t, mr, "alice", "right")

	_, errUnknown := s.authUser("nobody", "x", "198.51.100.1")
	_, errWrong := s.authUser("alice", "x", "198.51.100.1")
	if errUnknown == nil || errWrong == nil || errUnknown.Error() != errWrong.Error() {
		t.Fatalf("unknown %v, wrong %v; want the same refusal", errUnknown, errWrong)
	}
	for i := 0; i < sftpMaxFailsPerUserAddr; i++ {
		s.authUser("alice", "x", "198.51.100.1")
	}
	if _, err := s.authUser("alice", "right", "198.51.100.1"); err == nil {
		t.Error("the guessing address was not locked out")
	}
	if p, err := s.authUser("alice", "right", "203.0.113.5"); err != nil || p.Extensions["method"] != "password" {
		t.Errorf("alice from her own address was locked out by someone else: %v", err)
	}
}

// An account with a second factor signs in with a key it added in the panel.
func TestSFTPSignsInWithAPublishedKey(t *testing.T) {
	s, mr := sftpTestServer(t)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := ssh.NewPublicKey(pub)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	data, _ := json.Marshal([]string{line})
	mr.Set(sftpKeysKey("node-1", "alice"), string(data))

	p, err := s.authKey("alice", key)
	if err != nil || p.Extensions["method"] != "publickey" || p.Extensions["cred"] != line {
		t.Fatalf("published key refused: %v", err)
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	okey, _ := ssh.NewPublicKey(other)
	if _, err := s.authKey("alice", okey); err == nil {
		t.Error("a key that is not alice's was accepted")
	}
	if _, err := s.authKey("bob", key); err == nil {
		t.Error("alice's key signed in as bob")
	}
}

// A reset password, or a removed key, ended nothing: the session ran on what it
// read at sign-in. Driven through a real SSH connection.
func TestAnOpenSFTPSessionEndsWhenItsCredentialChanges(t *testing.T) {
	prev := sftpRecheckInterval
	sftpRecheckInterval = 50 * time.Millisecond
	t.Cleanup(func() { sftpRecheckInterval = prev })

	s, mr := sftpTestServer(t)
	setHash(t, mr, "alice", "right")
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(hostPriv)
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
		return s.authUser(c.User(), string(p), remoteAddr(c.RemoteAddr()))
	}}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			s.handleConn(conn, cfg)
		}
	}()
	client, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{
		User: "alice", Auth: []ssh.AuthMethod{ssh.Password("right")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("sign-in: %v", err)
	}
	closed := make(chan struct{})
	go func() { client.Wait(); close(closed) }()

	select {
	case <-closed:
		t.Fatal("the session ended while its credential was still valid")
	case <-time.After(200 * time.Millisecond):
	}
	setHash(t, mr, "alice", "a-new-password") // reset in the panel, republished by Core
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("the session outlived the password it signed in with")
	}
}

// Two servers of one name: the last one won, so a server someone invited you to
// could stand in for your own and take your uploads.
func TestServersOfOneNameAreBothReachable(t *testing.T) {
	set := newServerSet([]sftpServerRef{
		{UUID: "aaaaaaaa-1", Name: "survival"},
		{UUID: "bbbbbbbb-2", Name: "survival"},
		{UUID: "cccccccc-3", Name: "lobby"},
		{UUID: "dddddddd-4", Name: "a/b"},
	})
	for name, uuid := range map[string]string{
		"survival (aaaaaaaa)": "aaaaaaaa-1", "survival (bbbbbbbb)": "bbbbbbbb-2",
		"lobby": "cccccccc-3", "a_b (dddddddd)": "dddddddd-4",
	} {
		if got := set.nameToRef[name].UUID; got != uuid {
			t.Errorf("%q -> %q, want %q", name, got, uuid)
		}
	}
	if _, ok := set.nameToRef["survival"]; ok {
		t.Error("an ambiguous name still resolves to one of them")
	}
}

// The ceiling was computed per open and booked on close: twenty handles opened
// at once each had the whole remaining allowance.
func TestParallelSFTPUploadsShareOneAllowance(t *testing.T) {
	dir := t.TempDir()
	var pending atomic.Int64
	open := func(name string) *meteredSFTPWriter {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		daily := int64(100)
		return &meteredSFTPWriter{f: f, ceil: -1, dailyCap: &daily, pending: &pending}
	}
	a, b := open("a"), open("b")
	if _, err := a.WriteAt(make([]byte, 60), 0); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	if _, err := b.WriteAt(make([]byte, 60), 0); err == nil {
		t.Fatal("a second handle wrote past the allowance the first had already used")
	}
	a.f.Close()
	b.f.Close()
}

func flagsReq(path string, flags uint32) *sftp.Request {
	return &sftp.Request{Method: "Put", Filepath: path, Flags: flags}
}

// Every open for writing truncated the file, so a resumed upload or an in-place
// edit emptied it first; and a truncate request was answered without happening.
func TestSFTPWritesHonourTheOpenFlagsAndTruncate(t *testing.T) {
	fs, target := permittedFS(t, fileperms.Full())
	const write, creat, trunc = 0x02, 0x08, 0x10
	w, err := fs.Filewrite(flagsReq("myserver/survival/server.jar", write|creat))
	if err != nil {
		t.Fatal(err)
	}
	w.WriteAt([]byte("J"), 0)
	w.(interface{ Close() error }).Close()
	if got, _ := os.ReadFile(target); string(got) != "Jar" {
		t.Fatalf("without TRUNC the file became %q, want the rest kept", got)
	}

	set := &sftp.Request{Method: "Setstat", Filepath: "myserver/survival/server.jar",
		Flags: 0x01, Attrs: binary.BigEndian.AppendUint64(nil, 1)} // size = 1
	if err := fs.Filecmd(set); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "J" {
		t.Fatalf("after a truncate to 1 byte the file is %q", got)
	}

	w, err = fs.Filewrite(flagsReq("myserver/survival/server.jar", write|creat|trunc))
	if err != nil {
		t.Fatal(err)
	}
	w.(interface{ Close() error }).Close()
	if got, _ := os.ReadFile(target); len(got) != 0 {
		t.Fatalf("with TRUNC the file is %q, want empty", got)
	}
}

// SFTP changes reached no audit trail.
func TestSFTPChangesAreCollectedForTheAudit(t *testing.T) {
	fs, _ := permittedFS(t, fileperms.Full())
	fs.audit = newSFTPAudit("alice", "203.0.113.5")
	if err := fs.Filecmd(&sftp.Request{Method: "Remove", Filepath: "myserver/survival/server.jar"}); err != nil {
		t.Fatal(err)
	}
	recs := fs.audit.take()
	if len(recs) != 1 || recs[0].Deletes != 1 || recs[0].Paths[0] != "survival/server.jar" || recs[0].Username != "alice" {
		t.Fatalf("audit = %+v", recs)
	}
}

// The limits were taken on the end offset, which was right only while every
// open truncated: a resume at 1.9 GB of a 2 GB file was refused, and an
// in-place edit of a large file booked its whole size.
func TestAResumedUploadIsMeteredOnWhatItAdds(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "big"))
	if err != nil {
		t.Fatal(err)
	}
	daily := int64(100)
	m := &meteredSFTPWriter{f: f, ceil: 50, reason: "server disk limit", dailyCap: &daily, baseline: 1000, maxEnd: 1000}
	if _, err := m.WriteAt(make([]byte, 40), 1000); err != nil {
		t.Fatalf("a resume that adds 40 bytes was refused: %v", err)
	}
	if _, err := m.WriteAt(make([]byte, 10), 0); err != nil {
		t.Fatalf("an in-place write that adds nothing was refused: %v", err)
	}
	if _, err := m.WriteAt(make([]byte, 20), 1040); err == nil {
		t.Fatal("adding 60 bytes passed a 50-byte disk headroom")
	}
	if m.added() != 40 {
		t.Fatalf("added = %d, want 40", m.added())
	}
	f.Close()
}

// A truncate request could GROW a file past every upload limit.
func TestSetstatCannotGrowAFile(t *testing.T) {
	fs, target := permittedFS(t, fileperms.Full())
	grow := &sftp.Request{Method: "Setstat", Filepath: "myserver/survival/server.jar",
		Flags: 0x01, Attrs: binary.BigEndian.AppendUint64(nil, 1<<40)}
	if err := fs.Filecmd(grow); err == nil {
		t.Fatal("a truncate grew the file")
	}
	if st, _ := os.Stat(target); st.Size() != 3 {
		t.Fatalf("size = %d, want 3", st.Size())
	}
}

// A handle opened before the grant was withdrawn kept writing.
func TestAnOpenHandleStopsWhenItsServerLeavesTheList(t *testing.T) {
	fs, _ := permittedFS(t, fileperms.Full())
	w, err := fs.Filewrite(&sftp.Request{Method: "Put", Filepath: "myserver/survival/new.txt", Flags: 0x02 | 0x08 | 0x10})
	if err != nil {
		t.Fatal(err)
	}
	defer w.(interface{ Close() error }).Close()
	r, err := fs.Fileread(&sftp.Request{Method: "Get", Filepath: "myserver/survival/server.jar"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.(interface{ Close() error }).Close()
	fs.set.Store(newServerSet(nil)) // Core withdrew the grant; watchSession republished
	if _, err := w.WriteAt([]byte("x"), 0); err == nil {
		t.Error("a write handle outlived the grant")
	}
	if _, err := r.ReadAt(make([]byte, 1), 0); err == nil {
		t.Error("a read handle outlived the grant")
	}
}

// One account guessed from many addresses had no ceiling, and an IPv6 address
// is one of billions in its holder's /64.
func TestSFTPLockoutCountsTheAccountAndIPv6Prefixes(t *testing.T) {
	if got := remoteAddr(&net.TCPAddr{IP: net.ParseIP("2001:db8:1:2:aaaa::1")}); got != remoteAddr(&net.TCPAddr{IP: net.ParseIP("2001:db8:1:2:bbbb::9")}) {
		t.Errorf("two addresses of one /64 count apart: %s", got)
	}
	s, mr := sftpTestServer(t)
	setHash(t, mr, "alice", "right")
	mr.Set(sftpFailKey("node-1", "alice"), "100")
	if _, err := s.authUser("alice", "right", "203.0.113.77"); err == nil {
		t.Error("an account guessed at from many addresses was not locked")
	}
}
