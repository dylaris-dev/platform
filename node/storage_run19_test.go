package main

import (
	"context"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Z13: an empty disk missing for one boot kept its older gen, and from then
// on every boot refused it as an unmounted mountpoint, mounted or not.
func TestAnEmptyDiskMissingForOneBootIsTakenBackWhenItReturns(t *testing.T) {
	disk1, disk2 := t.TempDir(), t.TempDir()
	csv := disk1 + "," + disk2
	if sm := NewStorageManager(csv, nil); len(sm.Paths()) != 2 {
		t.Fatalf("boot 1 took %v, want both", sm.Paths())
	}
	// Boot 2: disk2 does not mount; its bare mountpoint is empty.
	away := disk2 + "-disk"
	if err := os.Rename(disk2, away); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(disk2, 0o755); err != nil {
		t.Fatal(err)
	}
	if sm := NewStorageManager(csv, nil); len(sm.Paths()) != 1 {
		t.Fatalf("boot 2 took %v, want disk2 skipped", sm.Paths())
	}
	// Boot 3 and 4: disk2 mounts again, still holding no server.
	if err := os.Remove(disk2); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(away, disk2); err != nil {
		t.Fatal(err)
	}
	for boot := 3; boot <= 4; boot++ {
		if sm := NewStorageManager(csv, nil); len(sm.Paths()) != 2 || sm.skipped != 0 {
			t.Fatalf("boot %d took %v (skipped %d), want the remounted disk back", boot, sm.Paths(), sm.skipped)
		}
	}
}

// Z12: publishDiskUsage held the node-wide unmeasured lock across a SET with
// the caller's deadline-less context, so a hung Redis held it for go-redis's
// whole retry window, and noteDiskWrite on the mesh read loop waited behind it.
func TestPublishDiskUsageDoesNotWaitOutAHungRedis(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Reads everything, answers nothing.
			go func(c net.Conn) { defer c.Close(); io.Copy(io.Discard, c) }(c)
		}
	}()
	rdb := redis.NewClient(&redis.Options{Addr: ln.Addr().String()})
	defer rdb.Close()
	uuids := []string{
		"19191919-1919-1919-1919-191919191901",
		"19191919-1919-1919-1919-191919191902",
		"19191919-1919-1919-1919-191919191903",
		"19191919-1919-1919-1919-191919191904",
	}
	t.Cleanup(func() {
		unmeasured.Lock()
		for _, u := range uuids {
			delete(unmeasured.m, u)
			delete(unmeasured.last, u)
		}
		unmeasured.Unlock()
	})

	// Three servers' disk ticks publish at once, then a mesh upload for a
	// fourth finishes.
	for _, u := range uuids[:3] {
		go publishDiskUsage(context.Background(), rdb, u, &DiskUsagePayload{Total: 1}, time.Now(), time.Minute)
	}
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	noteDiskWrite(context.Background(), rdb, uuids[3], 1)
	if took := time.Since(start); took > 4500*time.Millisecond {
		t.Fatalf("noteDiskWrite waited %v behind other servers' publishes to a hung Redis", took)
	}
	start = time.Now()
	publishDiskUsage(context.Background(), rdb, uuids[3], &DiskUsagePayload{Total: 1}, time.Now(), time.Minute)
	if took := time.Since(start); took > 4500*time.Millisecond {
		t.Fatalf("publishDiskUsage took %v against a hung Redis", took)
	}
}
