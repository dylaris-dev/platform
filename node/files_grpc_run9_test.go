package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"dylaris-pkg/queue"

	"google.golang.org/grpc/peer"
)

// P14: a Beam audit record named the peer as "ip:port", which Core casts to
// INET, so Postgres refused every Beam row. It names the host, like SFTP.
func TestABeamAuditRecordNamesTheHostWithoutItsPort(t *testing.T) {
	_, rdb := gaugeRedis(t)
	sub := rdb.Subscribe(context.Background(), queue.SFTPAuditChannel(nodeID))
	defer sub.Close()
	if _, err := sub.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := &beamServer{rdb: rdb}
	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("10.0.1.7"), Port: 43122}})
	s.auditBeam(ctx, "srv", "write", "plugins/x.yml")
	select {
	case msg := <-sub.Channel():
		var rec queue.SFTPAuditRecord
		if err := json.Unmarshal([]byte(msg.Payload), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.RemoteIP != "10.0.1.7" {
			t.Fatalf("RemoteIP = %q, want the host alone", rec.RemoteIP)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no audit record was published")
	}
}

// P15: a Beam write after the server was deleted recreated its directory.
func TestAWriteForADeletedServerDoesNotRecreateIt(t *testing.T) {
	const uuid = "15151515-1515-1515-1515-151515151515"
	prevDir := nodeSecretDir
	nodeSecretDir = ""
	t.Cleanup(func() {
		nodeSecretDir = prevDir
		deletedServers.Lock()
		delete(deletedServers.at, uuid)
		delete(deletedServers.issued, uuid)
		deletedServers.Unlock()
	})
	markServerDeletedIssued(uuid, 0)
	dir := filepath.Join(t.TempDir(), uuid)
	if root, _, err := openJailedForWrite(dir, "plugins/x.yml"); err == nil {
		root.Close()
		t.Fatal("a write for a deleted server was let through")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the deleted server's directory came back: %v", err)
	}
}

// P13: the sweeper removed a kept Beam temp whose bytes were already booked
// into the gauge, and nothing lowered it until the next measurement.
func TestTheUploadSweeperRemeasuresWhatItFrees(t *testing.T) {
	_, rdb := gaugeRedis(t)
	// A remeasure an earlier test started reads globalStorageMgr; swapping it
	// under that goroutine is a data race under -race.
	waitRemeasuresIdle(t)
	prevSM, prevQ := globalStorageMgr, globalQuotaSet
	t.Cleanup(func() { globalStorageMgr, globalQuotaSet = prevSM, prevQ })
	globalStorageMgr, globalQuotaSet = NewStorageManager(t.TempDir(), rdb), nil
	const uuid = "13131313-1313-1313-1313-131313131313"
	serverDir := globalStorageMgr.GetServerDir(uuid)
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(serverDir, ".beam-upload-kept")
	if err := os.WriteFile(tmp, make([]byte, 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(tmp, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "keep.txt"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	recordDiskLimit(context.Background(), rdb, uuid, 1)
	rdb.Set(context.Background(), diskGaugeKey(uuid), `{"total":900000,"limit":1048576}`, time.Minute)
	t.Cleanup(func() {
		unmeasured.Lock()
		delete(unmeasured.m, uuid)
		delete(unmeasured.last, uuid)
		unmeasured.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sweepStaleUploadTemps(ctx, globalStorageMgr, nil)
	want := dirSize(serverDir)
	deadline := time.Now().Add(3 * time.Second)
	for gaugeTotal(t, rdb, uuid) != want {
		if time.Now().After(deadline) {
			t.Fatalf("gauge after the sweep = %d, want the measured %d", gaugeTotal(t, rdb, uuid), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitRemeasuresIdle(t)
}

// P16: a rename caught between its two renameat calls by a node restart left
// the entry parked under a name no tenant path can reach. The boot sweep gives
// it back under a visible name; a parked link is removed, never exposed; one
// this process is moving right now is left alone.
func TestTheBootSweepRecoversAParkedRename(t *testing.T) {
	base := t.TempDir()
	srv := filepath.Join(base, "srv-1")
	parkedDir := filepath.Join(srv, "survival", "world", renameStagePrefix+"0123456789abcdef")
	if err := os.MkdirAll(filepath.Join(parkedDir, "region"), 0o755); err != nil {
		t.Fatal(err)
	}
	parkedFile := filepath.Join(srv, renameStagePrefix+"fedcba9876543210")
	if err := os.WriteFile(parkedFile, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	ours := filepath.Join(srv, renameStageOwn+"aaaa")
	if err := os.WriteFile(ours, []byte("in flight"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(srv, renameStagePrefix+"1111222233334444")
	linkMade := os.Symlink(".node_config.json", link) == nil

	sweepInterruptedWork([]string{base})

	for _, tc := range []struct{ parked, back string }{
		{parkedDir, filepath.Join(srv, "survival", "world", "recovered-0123456789abcdef", "region")},
		{parkedFile, filepath.Join(srv, "recovered-fedcba9876543210")},
	} {
		if _, err := os.Lstat(tc.parked); !os.IsNotExist(err) {
			t.Errorf("still parked: %s", tc.parked)
		}
		if _, err := os.Stat(tc.back); err != nil {
			t.Errorf("not given back: %s (%v)", tc.back, err)
		}
	}
	if _, err := os.Stat(ours); err != nil {
		t.Errorf("took an entry this process is still moving: %v", err)
	}
	if linkMade {
		if _, err := os.Lstat(link); !os.IsNotExist(err) {
			t.Error("a parked link is still there")
		}
		if _, err := os.Lstat(filepath.Join(srv, "recovered-1111222233334444")); !os.IsNotExist(err) {
			t.Error("a parked link was given a visible name")
		}
	}
}

// waitRemeasuresIdle waits until no background remeasure is running, so a
// test can swap the storage globals without racing one. The atomic counter
// reaching 0 happens after the goroutine's last read of the globals.
func waitRemeasuresIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	remeasureRuns.Range(func(_, v any) bool {
		for v.(*atomic.Int64).Load() != 0 {
			if time.Now().After(deadline) {
				t.Fatal("a background remeasure is still running")
			}
			time.Sleep(5 * time.Millisecond)
		}
		return true
	})
}
