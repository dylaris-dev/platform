package main

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dylaris-pkg/migration"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func writeMoveZip(t *testing.T, files map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "in.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return p
}

// The source node wrote the archive, and a customer's node writes what it
// likes. The reconciler started a moved server from the archived
// .node_config.json: its image, memory, CPU and command, on this host.
func TestAMovedServerArrivesWithoutTheSourcesNodeFiles(t *testing.T) {
	zipPath := writeMoveZip(t, map[string]string{
		".node_config.json":  `{"docker":{"image":"evil.example/x","ram":999999}}`,
		".dylaris.json":      `{}`,
		".active_server":     "survival",
		"survival/eula.txt":  "eula=true",
		"survival/world/lvl": "w",
	})
	storage := t.TempDir()
	dir := filepath.Join(storage, "srv")
	if err := extractMovedServer(zipPath, storage, dir); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{".node_config.json", ".dylaris.json"} {
		if _, err := os.Lstat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s from the archive is still there", gone)
		}
	}
	for _, kept := range []string{".active_server", "survival/eula.txt", "survival/world/lvl"} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("%s did not arrive: %v", kept, err)
		}
	}
}

// The compressed size is bounded by the pull; what it unpacks to was not.
func TestAMovedServerIsUnpackedWithinTheDiskBudget(t *testing.T) {
	old := restoreDiskBudget
	restoreDiskBudget = func(string) int64 { return 8 }
	t.Cleanup(func() { restoreDiskBudget = old })

	storage := t.TempDir()
	zipPath := writeMoveZip(t, map[string]string{"survival/a": "12345", "survival/b": "6789"})
	if err := extractMovedServer(zipPath, storage, filepath.Join(storage, "srv")); !errors.Is(err, migration.ErrExtractBudget) {
		t.Fatalf("err = %v, want the budget refusal", err)
	}
	zipPath = writeMoveZip(t, map[string]string{"survival/a": "1234", "survival/b": "5678"})
	if err := extractMovedServer(zipPath, storage, filepath.Join(storage, "srv2")); err != nil {
		t.Fatalf("an archive that fits exactly was refused: %v", err)
	}
}

// A moved server holds no config until Core's reconfigure saves one. When the
// node refuses that reconfigure, Core showed the server "starting" for good.
func TestARefusedReconfigureOfAMovedServerReadsStopped(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	sm := NewStorageManager(t.TempDir(), nil)
	const moved, settled = "66666666-6666-6666-6666-666666666666", "77777777-7777-7777-7777-777777777777"
	for _, u := range []string{moved, settled} {
		os.MkdirAll(sm.GetServerDir(u), 0o755)
	}
	os.WriteFile(filepath.Join(sm.GetServerDir(settled), ".node_config.json"), []byte("{}"), 0o644)
	ctx := context.Background()
	reportUnstartable(ctx, rdb, sm, moved)
	reportUnstartable(ctx, rdb, sm, settled)
	if got, _ := rdb.Get(ctx, "dylaris:server:"+moved+":status").Result(); got != "stopped" {
		t.Errorf("moved server status %q, want stopped", got)
	}
	if n, _ := rdb.Exists(ctx, "dylaris:server:"+settled+":status").Result(); n != 0 {
		t.Error("a server with a saved config was reported stopped; its container may be running")
	}
}

// Both refusals in the reconfigure handler report it.
func TestReconfigureReportsBothRefusals(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i, j := strings.Index(s, "\tcase \"reconfigure\":"), strings.Index(s, "\tcase \"switch_server\":")
	if i < 0 || j < i {
		t.Fatal("the reconfigure case moved; move this assertion with it")
	}
	if n := strings.Count(s[i:j], "reportUnstartable("); n != 2 {
		t.Errorf("reconfigure reports %d of its 2 refusals", n)
	}
}
