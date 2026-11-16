package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Core enforces the per-server sub-server limit from this map, and the du path
// - the one every install without xfs/ext4 project quotas takes - used to omit
// it entirely. len(nil) is never >= a positive limit, so the setting silently
// did nothing there. These are the distinctions the map has to get right.
func TestScanSubServerSizes(t *testing.T) {
	root := t.TempDir()

	mustDir := func(name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}
	mustDir("survival")
	mustDir("creative")
	mustDir(".dylaris-backups")                 // not a sub-server
	mustDir(filepath.Join("survival", "world")) // a child, not a sibling

	if err := os.WriteFile(filepath.Join(root, ".active_server"), []byte("survival"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "survival", "server.jar"), make([]byte, 2048), 0o644); err != nil {
		t.Fatalf("write jar: %v", err)
	}

	got := scanSubServerSizes(root)

	if len(got) != 2 {
		t.Fatalf("got %v, want exactly survival and creative", got)
	}
	for _, want := range []string{"survival", "creative"} {
		if _, ok := got[want]; !ok {
			t.Errorf("%q is missing from %v", want, got)
		}
	}
	if _, ok := got[".dylaris-backups"]; ok {
		t.Error("a dot-directory was counted as a sub-server: it would inflate the limit Core enforces")
	}
	if _, ok := got["world"]; ok {
		t.Error("a directory inside a sub-server was counted as one of its own")
	}
	if got["survival"] < 2048 {
		t.Errorf("survival measured %d bytes, want at least the 2048 written into it", got["survival"])
	}
}

// A server directory that is not there yet is a real state (a server created
// but never set up). It must read as an empty map rather than a nil one, so a
// caller can tell "the node looked and found nothing" from "the node never
// reported".
func TestScanSubServerSizesOnAMissingDirectory(t *testing.T) {
	got := scanSubServerSizes(filepath.Join(t.TempDir(), "does-not-exist"))
	if got == nil {
		t.Fatal("a missing directory returned nil, which JSON-encodes as null - exactly the value that made the limit unenforceable")
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

// The server root is the tenant's: anyone with files.write can mkdir there
// over SFTP or the file manager, and every directory used to cost one du
// fork and one entry in the shared stats:disk payload. The walk is bounded
// like its sibling shipperGuardSubServers: valid names only, the active one
// first, at most subServerSizesMax in all.
func TestScanSubServerSizesIsBounded(t *testing.T) {
	tests := []struct {
		name     string
		dirs     []string
		active   string
		wantLen  int
		wantKeys []string
		notKeys  []string
	}{
		{"invalid names are not sub-servers", []string{"survival", "has space", "a.b"}, "", 1, []string{"survival"}, []string{"has space", "a.b"}},
		{"the cap holds", manyDirs(subServerSizesMax + 50), "", subServerSizesMax, nil, nil},
		// "zzz" sorts after every dNNNN, so without active-first it is cut.
		{"the active one survives the cap", append(manyDirs(subServerSizesMax+50), "zzz"), "zzz", subServerSizesMax, []string{"zzz"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for _, d := range tt.dirs {
				if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tt.active != "" {
				if err := os.WriteFile(filepath.Join(root, ".active_server"), []byte(tt.active), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := scanSubServerSizes(root)
			if len(got) != tt.wantLen {
				t.Fatalf("got %d entries, want %d", len(got), tt.wantLen)
			}
			for _, k := range tt.wantKeys {
				if _, ok := got[k]; !ok {
					t.Errorf("%q missing", k)
				}
			}
			for _, k := range tt.notKeys {
				if _, ok := got[k]; ok {
					t.Errorf("%q reported", k)
				}
			}
		})
	}
}

func manyDirs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("d%04d", i)
	}
	return out
}

// Concurrent breakdowns (the collector, the stopped sweep and disk_guard on
// every write all reach it) must not fork du without bound.
func TestScanSubServerSizesBoundsConcurrentDu(t *testing.T) {
	var inFlight, peak atomic.Int32
	orig := subServerDirSize
	subServerDirSize = func(string) int64 {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		return 1
	}
	t.Cleanup(func() { subServerDirSize = orig })

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		root := t.TempDir()
		for _, d := range manyDirs(4) {
			if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		wg.Add(1)
		go func() { defer wg.Done(); scanSubServerSizes(root) }()
	}
	wg.Wait()
	if peak.Load() == 0 {
		t.Fatal("the breakdown did not measure through subServerDirSize")
	}
	if p := peak.Load(); p > int32(cap(subServerDuSlots)) {
		t.Fatalf("%d du ran at once, want at most %d", p, cap(subServerDuSlots))
	}
}

func TestForgetSubServerSizes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "11111111-2222-3333-4444-555555555555")
	if err := os.MkdirAll(filepath.Join(dir, "survival"), 0o755); err != nil {
		t.Fatal(err)
	}
	subServerSizesFresh(dir)
	forgetSubServerSizes(filepath.Base(dir))
	subSizeCache.mu.Lock()
	_, ok := subSizeCache.m[dir]
	subSizeCache.mu.Unlock()
	if ok {
		t.Fatal("a forgotten server's breakdown stayed in memory")
	}
}
