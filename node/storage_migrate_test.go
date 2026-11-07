package main

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	sm, _ := newPlacementManager(t, 2)
	sm.rdb = rdb
	paths := sm.Paths()
	const uuid = "b083ff0c-fc16-47c5-9700-7b16486583e7_xegpnft249"
	// The key names the source: that is what makes the target a leftover.
	if err := sm.setRedisPath(uuid, paths[0]); err != nil {
		t.Fatal(err)
	}
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
	if err := sm.setRedisPath(uuid, paths[0]); err != nil {
		t.Fatal(err)
	}

	mr.SetError("READONLY")
	if err := sm.MigrateServerPath(uuid, paths[1]); !errors.Is(err, errStorageKeyUnsure) {
		// The handler starts a server again after a failed move only when
		// this is not the error: the key may name the target already.
		t.Fatalf("MigrateServerPath = %v, want errStorageKeyUnsure", err)
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

// K35: with no storage key to say which copy is live, a tree on the target is
// not provably a leftover - it may be the live server and the source the
// stale copy. The move is refused rather than deleting it.
func TestMigrateServerPathRefusesATargetTreeItCannotProveIsALeftover(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	sm, _ := newPlacementManager(t, 2)
	sm.rdb = rdb
	paths := sm.Paths()
	const uuid = "b083ff0c-fc16-47c5-9700-7b16486583e7_xegpnft249"
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Join(p, uuid), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	live := filepath.Join(paths[1], uuid, "level.dat")
	if err := os.WriteFile(live, []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		key  string
	}{
		{"no storage key", ""},
		{"key names a path that is not active", t.TempDir()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := "node:" + nodeID + ":server:" + uuid + ":storage"
			mr.Del(key)
			if tc.key != "" {
				mr.Set(key, tc.key)
			}
			if err := sm.MigrateServerPath(uuid, paths[1]); err == nil {
				t.Fatal("the move cleared a target tree no storage key named as the leftover")
			}
			if _, err := os.Stat(live); err != nil {
				t.Fatalf("the tree on the target was deleted: %v", err)
			}
		})
	}
}

// K35: a scan is a guess. Written back, one transient miss of the live path
// made a stale leftover the server's storage for good.
func TestGetServerPathPersistsOnlyAnUnambiguousScan(t *testing.T) {
	const uuid = "b083ff0c-fc16-47c5-9700-7b16486583e7_xegpnft249"
	for _, tc := range []struct {
		name    string
		key     bool // the key names a path that is not active
		both    bool // both paths hold the server
		persist bool
	}{
		{"no key one copy", false, false, true},
		{"no key two copies", false, true, false},
		{"key names a missing path", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })
			sm, _ := newPlacementManager(t, 2)
			sm.rdb = rdb
			paths := sm.Paths()
			key := "node:" + nodeID + ":server:" + uuid + ":storage"
			missing := t.TempDir()
			if tc.key {
				mr.Set(key, missing)
			}
			dirs := paths[:1]
			if tc.both {
				dirs = paths
			}
			for _, p := range dirs {
				if err := os.MkdirAll(filepath.Join(p, uuid), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if got := sm.GetServerPath(uuid); got != paths[0] {
				t.Fatalf("resolved %q, want %q", got, paths[0])
			}
			v, _ := mr.Get(key)
			want := ""
			switch {
			case tc.persist:
				want = paths[0]
			case tc.key:
				want = missing
			}
			if v != want {
				t.Fatalf("storage key = %q, want %q", v, want)
			}
		})
	}
}

// R13: a move walked a Root at the STORAGE PATH, so a tenant link that stays
// inside it (../.node_secret, ../<other server>/...) was followed and its
// target copied into the moved server as a plain file the container can read.
func TestMigrateServerPathDoesNotFollowLinksOutOfTheServer(t *testing.T) {
	const uuid = "b083ff0c-fc16-47c5-9700-7b16486583e7_xegpnft249"
	const other = "c9a1d1a0-0000-4000-8000-000000000000_other00001"
	for _, tc := range []struct {
		name, target, outside string
	}{
		{"node secret", filepath.Join("..", ".node_secret"), ".node_secret"},
		{"other tenant", filepath.Join("..", other, "world", "level.dat"), filepath.Join(other, "world", "level.dat")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm, _ := newPlacementManager(t, 2)
			paths := sm.Paths()
			srcDir := filepath.Join(paths[0], uuid)
			outside := filepath.Join(paths[0], tc.outside)
			for full, body := range map[string]string{
				outside: "SECRET",
				// Large enough that the move's 90% size check is not decided
				// by the few bytes of the link.
				filepath.Join(srcDir, "survival", "server.properties"): strings.Repeat("m", 64<<10),
			} {
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(tc.target, filepath.Join(srcDir, "x")); err != nil {
				t.Skipf("cannot create symlinks here: %v", err)
			}

			if err := sm.MigrateServerPath(uuid, paths[1]); err != nil {
				t.Fatalf("MigrateServerPath: %v", err)
			}
			dstDir := filepath.Join(paths[1], uuid)
			if body, err := os.ReadFile(filepath.Join(dstDir, "x")); err == nil {
				t.Errorf("the move copied a file from outside the server through a link: %q", body)
			}
			if _, err := os.Stat(filepath.Join(dstDir, "survival", "server.properties")); err != nil {
				t.Errorf("the server's own tree did not arrive: %v", err)
			}
		})
	}
}

// R14: CheckMigration reads the writers once, then the stop takes up to a
// minute, and an SFTP handle or an upload opened in that window made the
// move's fence refuse after the server was down. Nothing started it again.
// A failed move restarts a server that was running, except when the key write
// failed and may already name the target.
func TestMigrateStorageRestartsAServerTheFailedMoveStopped(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)case "migrate_storage":(.*?)\n\tcase "`).FindSubmatch(src)
	if m == nil {
		t.Fatal(`case "migrate_storage" not found in the command switch`)
	}
	body := string(m[1])
	run, stop := strings.Index(body, "wasRunning := containerRunning(dm, cmd.Config.UUID)"), strings.Index(body, "err := gracefulStop(")
	if run < 0 || stop < 0 || run > stop {
		t.Error("migrate_storage does not record whether the server ran before it stops it")
	}
	fail := regexp.MustCompile(`(?s)err := storage\.MigrateServerPath\(.*?\n\t\t\}\n`).FindString(body)
	for _, want := range []string{"if wasRunning && !errors.Is(err, errStorageKeyUnsure)", "dm.RestartContainer(cmd.Config.UUID)"} {
		if !strings.Contains(fail, want) {
			t.Errorf("the failed-move branch lacks %q", want)
		}
	}
}
