package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"dylaris-pkg/fileperms"

	"github.com/alicebob/miniredis/v2"
	"github.com/pkg/sftp"
	"github.com/redis/go-redis/v9"
)

// Each SFTP handle measured the server's disk headroom on its own, so twenty
// opened at once - by one member or by several - wrote twenty times past the
// limit. They now share one count with each other and with copies.
func TestSFTPHandlesShareTheServersDiskHeadroom(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	sm, _ := newPlacementManager(t, 1)
	const uuid = "91919191-9191-9191-9191-919191919191"
	if err := os.MkdirAll(filepath.Join(sm.Paths()[0], uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	rdb.Set(context.Background(), "dylaris:server:"+uuid+":stats:disk", `{"total":0,"limit":100}`, 0)
	refs := []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}
	open := func(user, name string) io.WriterAt {
		w, err := newVirtualFS(refs, sm, rdb, user).Filewrite(&sftp.Request{Method: "Put", Filepath: "s/" + name, Flags: 0x02 | 0x08 | 0x10})
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	closeW := func(w io.WriterAt) { w.(io.Closer).Close() }

	a, b := open("alice", "a"), open("bob", "b")
	if _, err := a.WriteAt(make([]byte, 60), 0); err != nil {
		t.Fatalf("first handle: %v", err)
	}
	if _, err := b.WriteAt(make([]byte, 60), 0); err == nil {
		t.Error("a second handle wrote past the headroom the first had already used")
	}
	if _, err := b.WriteAt(make([]byte, 40), 0); err != nil {
		t.Errorf("what is left was refused: %v", err)
	}

	// A copy measures the disk live, which already holds what the handles
	// wrote: counting them again refused copies while an upload ran.
	prev := serverDiskHeadroom
	t.Cleanup(func() { serverDiskHeadroom = prev })
	serverDiskHeadroom = func(string) (string, int64, bool) { return uuid, 100, true }
	withDiskBudget(t, 1<<30, 1000)
	if b := newWriteBudget("x"); b.left != 100 {
		t.Errorf("a copy lost %d bytes to the open SFTP handles", 100-b.left)
	}
	closeW(a)
	closeW(b)
	d := open("dave", "d")
	if _, err := d.WriteAt(make([]byte, 100), 0); err != nil {
		t.Errorf("closed handles still held the headroom: %v", err)
	}
	closeW(d)
	if n := sftpServerInflight(uuid).Load(); n != 0 {
		t.Errorf("%d bytes left counted after every handle closed", n)
	}
}
