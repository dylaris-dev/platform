package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"dylaris-pkg/queue"

	"github.com/docker/docker/client"
)

// A file that grew while it was archived failed the backup with "write too
// long", one that shrank with "missed writing".
func TestCopyExactlyKeepsTheHeadersSize(t *testing.T) {
	for _, tc := range []struct {
		data string
		size int64
		want string
	}{
		{"grown-file", 5, "grown"},
		{"abc", 5, "abc\x00\x00"},
		{"exact", 5, "exact"},
	} {
		var out bytes.Buffer
		if err := copyExactly(&out, strings.NewReader(tc.data), tc.size); err != nil || out.String() != tc.want {
			t.Errorf("copyExactly(%q, %d) = %q, %v; want %q", tc.data, tc.size, out.String(), err, tc.want)
		}
	}
}

// The node's own files came back from the archive: a restore reinstated the
// RAM and CPU of before a downgrade.
func TestARestoreKeepsTheNodesOwnFiles(t *testing.T) {
	stashed, restored := t.TempDir(), t.TempDir()
	for _, f := range []struct{ dir, name, body string }{
		{stashed, ".node_config.json", "current"},
		{restored, ".node_config.json", "from the archive"},
		{restored, "world.dat", "restored"},
	} {
		if err := os.WriteFile(filepath.Join(f.dir, f.name), []byte(f.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := carryArchivesAcrossSwap(stashed, restored); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(restored, ".node_config.json")); string(got) != "current" {
		t.Fatalf(".node_config.json = %q, want the node's current one", got)
	}
	if got, _ := os.ReadFile(filepath.Join(restored, "world.dat")); string(got) != "restored" {
		t.Fatalf("world.dat = %q, want the restored one", got)
	}
}

// restoreArchive writes a local-provider archive of entries and returns the
// command that restores it into a fresh server root.
func restoreArchive(t *testing.T, entries []tar.Header, bodies map[string]string) (root string, cmd BackupRestoreCommand) {
	t.Helper()
	root, base := t.TempDir(), t.TempDir()
	const uuid, key = "srv-r29", "backups/srv/job-1/run.tar.gz"
	if err := os.MkdirAll(filepath.Join(root, uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, uuid, "keep.txt"), []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	gw := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gw)
	for _, h := range entries {
		h := h
		h.Size = int64(len(bodies[h.Name]))
		_ = tw.WriteHeader(&h)
		_, _ = tw.Write([]byte(bodies[h.Name]))
	}
	_ = tw.Close()
	_ = gw.Close()
	full := filepath.Join(base, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, archive.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(localCfg{BasePath: base})
	blob, _ := json.Marshal(storageInfo{ID: 1, Provider: "local", Config: cfg})
	return root, BackupRestoreCommand{RunID: 9, RestoreID: 61, ServerUUID: uuid, StorageKey: key, Storage: blob}
}

// A file archived once and linked under a second name restores under both.
func TestRestoreRecreatesALinkedFile(t *testing.T) {
	root, cmd := restoreArchive(t, []tar.Header{
		{Name: "a.txt", Mode: 0o644, Typeflag: tar.TypeReg},
		{Name: "b.txt", Mode: 0o644, Typeflag: tar.TypeLink, Linkname: "a.txt"},
	}, map[string]string{"a.txt": "same"})
	rdb := newMiniRedis(t)
	report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
		RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
	})
	if report["status"] != "success" {
		t.Fatalf("report = %v", report)
	}
	for _, n := range []string{"a.txt", "b.txt"} {
		if got, err := os.ReadFile(filepath.Join(root, cmd.ServerUUID, n)); err != nil || string(got) != "same" {
			t.Errorf("%s = %q, %v", n, got, err)
		}
	}
}

// The stage sits outside the server's disk limit and the archive may come from
// a bucket the tenant writes: extraction filled the node's disk.
func TestRestoreStopsAtTheDiskBudget(t *testing.T) {
	prev := restoreDiskBudget
	restoreDiskBudget = func(string) int64 { return 4 }
	t.Cleanup(func() { restoreDiskBudget = prev })

	root, cmd := restoreArchive(t, []tar.Header{{Name: "big.dat", Mode: 0o644, Typeflag: tar.TypeReg}},
		map[string]string{"big.dat": "eight by"})
	rdb := newMiniRedis(t)
	report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
		RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
	})
	if report["status"] != "failed" {
		t.Fatalf("report = %v, want failed", report)
	}
	if got, _ := os.ReadFile(filepath.Join(root, cmd.ServerUUID, "keep.txt")); string(got) != "before" {
		t.Fatalf("the server was replaced anyway: keep.txt = %q", got)
	}
}

// dockerRunningOnce answers the first inspect "running" and every later one
// "stopped", and records each request that changes something.
func dockerRunningOnce(t *testing.T, running bool) (*DockerManager, func() []string) {
	t.Helper()
	var (
		mu       sync.Mutex
		changes  []string
		inspects atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json") {
			up := running && inspects.Add(1) == 1
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"Name": "/mc_x", "State": map[string]interface{}{"Running": up},
				"Config": map[string]interface{}{"Image": "img"}, "HostConfig": map[string]interface{}{},
			})
			return
		}
		mu.Lock()
		changes = append(changes, r.Method+" "+r.URL.Path)
		mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.44"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return &DockerManager{cli: cli, ctx: t.Context()}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), changes...)
	}
}

// A restore started a server its owner had stopped, and a restore that failed
// after the stop left a running server down.
func TestARestoreBringsBackOnlyARunningServer(t *testing.T) {
	ok := []tar.Header{{Name: "world.dat", Mode: 0o644, Typeflag: tar.TypeReg}}
	for _, tc := range []struct {
		name      string
		running   bool
		entries   []tar.Header
		wantStart bool
	}{
		{"stopped, restore succeeds", false, ok, false},
		{"running, restore fails", true, nil, true}, // an empty archive fails after the stop
	} {
		t.Run(tc.name, func(t *testing.T) {
			dm, changes := dockerRunningOnce(t, tc.running)
			root, cmd := restoreArchive(t, tc.entries, map[string]string{"world.dat": "w"})
			rdb := newMiniRedis(t)
			terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
				RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, dm, cmd)
			})
			if started := len(changes()) > 0; started != tc.wantStart {
				t.Fatalf("container changed = %v (%v), want %v", started, changes(), tc.wantStart)
			}
		})
	}
}
