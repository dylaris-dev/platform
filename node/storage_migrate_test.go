package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// A storage migration MOVES the server directory: it copies to the target and
// then deletes the source. So everything the server needs must arrive, not just
// the files a user is allowed to duplicate.
//
// This is a regression test with a live cause. copyDir was taught to skip
// protected entries (a self-copy had zeroed a server's world AND both of its
// backups in one walk), and MigrateServerPath still called it. With backups
// present the migration then failed the 90% size check; without them it
// succeeded and deleted a source directory holding the only copy of
// .active_server and .node_config.json.
func TestMigrateServerPathKeepsProtectedEntries(t *testing.T) {
	sm, _ := newPlacementManager(t, 2)
	paths := sm.Paths()
	const uuid = "b083ff0c-fc16-47c5-9700-7b16486583e7_xegpnft249"

	srcDir := filepath.Join(paths[0], uuid)
	files := map[string]string{
		".active_server":    "survival",
		".node_config.json": `{"uuid":"x"}`,
		".dylaris.json":     "{}",
		filepath.Join(".dylaris-backups", "old.tar.gz"): "archive-bytes",
		filepath.Join("survival", "server.properties"):  "motd=hi",
	}
	for rel, body := range files {
		full := filepath.Join(srcDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := sm.MigrateServerPath(uuid, paths[1]); err != nil {
		t.Fatalf("MigrateServerPath: %v", err)
	}

	dstDir := filepath.Join(paths[1], uuid)
	for rel, want := range files {
		body, err := os.ReadFile(filepath.Join(dstDir, rel))
		if err != nil {
			t.Errorf("%s did not survive the migration: %v", rel, err)
			continue
		}
		if string(body) != want {
			t.Errorf("%s = %q, want %q", rel, body, want)
		}
	}

	if _, err := os.Stat(srcDir); !os.IsNotExist(err) {
		t.Errorf("source directory still present after the move (stat err = %v)", err)
	}
}

// The user-facing copy keeps its behaviour: the two helpers must not converge
// back into one.
func TestCopyTreeAndCopyDirDisagreeOnProtectedEntries(t *testing.T) {
	src, verbatim, filtered := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(src, ".active_server"), []byte("survival"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyTree(src, verbatim); err != nil {
		t.Fatalf("copyTree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verbatim, ".active_server")); err != nil {
		t.Errorf("copyTree dropped a protected entry: %v", err)
	}

	if err := copyDir(src, filtered); err != nil {
		t.Fatalf("copyDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filtered, ".active_server")); !os.IsNotExist(err) {
		t.Errorf("copyDir copied a protected entry (stat err = %v)", err)
	}
}

// J39/J41: a copy an interrupted move left on the target is not the server.
// Copying over it merged the two, and every file deleted from the live tree
// since came back with the move.
func TestMigrateServerPathStartsFromAnEmptyTarget(t *testing.T) {
	sm, _ := newPlacementManager(t, 2)
	paths := sm.Paths()
	const uuid = "b083ff0c-fc16-47c5-9700-7b16486583e7_xegpnft249"
	srcDir, dstDir := filepath.Join(paths[0], uuid), filepath.Join(paths[1], uuid)
	for dir, rel := range map[string]string{
		srcDir: filepath.Join("survival", "server.properties"),
		dstDir: filepath.Join("plugins", "removed.jar"),
	} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := sm.MigrateServerPath(uuid, paths[1]); err != nil {
		t.Fatalf("MigrateServerPath: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "plugins", "removed.jar")); !os.IsNotExist(err) {
		t.Errorf("a file from the stale copy survived the move (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "survival", "server.properties")); err != nil {
		t.Errorf("the live tree did not arrive: %v", err)
	}
}

// J38: the source is deleted only once the storage key names the target. A
// key that could not be written left the server resolving to a deleted tree.
func TestMigrateServerPathKeepsTheSourceWhenTheKeyCannotMove(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	sm, _ := newPlacementManager(t, 2)
	sm.rdb = rdb
	paths := sm.Paths()
	const uuid = "b083ff0c-fc16-47c5-9700-7b16486583e7_xegpnft249"
	srcDir := filepath.Join(paths[0], uuid)
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "level.dat"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	mr.SetError("READONLY")
	if err := sm.MigrateServerPath(uuid, paths[1]); err == nil {
		t.Fatal("the move reported success without moving the storage key")
	}
	if _, err := os.Stat(filepath.Join(srcDir, "level.dat")); err != nil {
		t.Fatalf("the source was deleted although the key still names it: %v", err)
	}

	mr.SetError("")
	if err := sm.MigrateServerPath(uuid, paths[1]); err != nil {
		t.Fatalf("MigrateServerPath: %v", err)
	}
	if v, _ := rdb.Get(t.Context(), "node:"+nodeID+":server:"+uuid+":storage").Result(); v != paths[1] {
		t.Fatalf("storage key = %q, want %q", v, paths[1])
	}
	if _, err := os.Stat(srcDir); !os.IsNotExist(err) {
		t.Errorf("source still present after the move (stat err = %v)", err)
	}
}
