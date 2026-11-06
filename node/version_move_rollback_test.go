package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTestFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

// A failed install puts the server back as it was; a successful one keeps what
// it installed. Before the stash the old jar was deleted first, and a failed
// download left a server with no jar at all.
func TestJarStashPutsTheOldServerBackOnFailure(t *testing.T) {
	for _, c := range []struct {
		name    string
		success bool
		want    map[string]string // relative to the sub-server
	}{
		{"failed install", false, map[string]string{
			"server.jar":                "old jar",
			"versions/1.21.1/paper.jar": "old paper",
			"mods/sodium-old.jar":       "old mod",
			"mods/sodium-new.jar":       "<missing>",
			"mods/kept.jar":             "kept",
			"world/level.dat":           "world",
		}},
		{"successful install", true, map[string]string{
			"server.jar":                "new jar",
			"versions/1.21.1/paper.jar": "<missing>",
			"mods/sodium-old.jar":       "<missing>",
			"mods/sodium-new.jar":       "new mod",
			"mods/kept.jar":             "kept",
			"world/level.dat":           "world",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			serverPath := t.TempDir()
			sub := filepath.Join(serverPath, "main")
			writeTestFile(t, filepath.Join(sub, "server.jar"), "old jar")
			writeTestFile(t, filepath.Join(sub, "versions", "1.21.1", "paper.jar"), "old paper")
			writeTestFile(t, filepath.Join(sub, "mods", "sodium-old.jar"), "old mod")
			writeTestFile(t, filepath.Join(sub, "mods", "kept.jar"), "kept")
			writeTestFile(t, filepath.Join(sub, "world", "level.dat"), "world")

			stash, err := stashServerJars(serverPath, "main")
			if err != nil {
				t.Fatal(err)
			}
			if got := readTestFile(filepath.Join(sub, "server.jar")); got != "<missing>" {
				t.Fatalf("server.jar still in place after the stash: %q", got)
			}
			if err := stash.takeFile(filepath.Join(sub, "mods", "sodium-old.jar"), "main/mods/sodium-old.jar"); err != nil {
				t.Fatal(err)
			}
			// What the move and the install then write.
			writeTestFile(t, filepath.Join(sub, "mods", "sodium-new.jar"), "new mod")
			stash.add("main/mods/sodium-new.jar")
			writeTestFile(t, filepath.Join(sub, "server.jar"), "new jar")

			if c.success {
				stash.discard()
			} else if err := stash.restore(); err != nil {
				t.Fatal(err)
			}
			for rel, want := range c.want {
				if got := readTestFile(filepath.Join(sub, filepath.FromSlash(rel))); got != want {
					t.Errorf("%s = %q, want %q", rel, got, want)
				}
			}
			if _, err := os.Stat(filepath.Join(serverPath, ".main.oldjars")); !os.IsNotExist(err) {
				t.Errorf("the stash was left behind: %v", err)
			}
		})
	}
}

// A Forge/NeoForge server starts from the unix_args.txt under its launch dir,
// and the loader install clears that dir before the installer runs. A failed
// install has to put it back, or the "restored" server cannot start.
func TestJarStashPutsTheOldLoaderLaunchDirBack(t *testing.T) {
	const (
		oldArgs = "libraries/net/minecraftforge/forge/47.2.0/unix_args.txt"
		newArgs = "libraries/net/minecraftforge/forge/47.3.0/unix_args.txt"
		lib     = "libraries/com/example/lib.jar"
	)
	for _, c := range []struct {
		name     string
		success  bool
		want     map[string]string
		wantArgs string
	}{
		{"failed install", false, map[string]string{oldArgs: "old args", newArgs: "<missing>", lib: "lib"}, oldArgs},
		{"successful install", true, map[string]string{oldArgs: "<missing>", newArgs: "new args", lib: "lib"}, newArgs},
	} {
		t.Run(c.name, func(t *testing.T) {
			serverPath := t.TempDir()
			sub := filepath.Join(serverPath, "main")
			writeTestFile(t, filepath.Join(sub, filepath.FromSlash(oldArgs)), "old args")
			writeTestFile(t, filepath.Join(sub, filepath.FromSlash(lib)), "lib")

			stash, err := stashServerJars(serverPath, "main")
			if err != nil {
				t.Fatal(err)
			}
			// What runLoaderInstaller does, and what the installer writes
			// before it fails or succeeds.
			if err := clearLoaderLaunchDirs(sub); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(sub, filepath.FromSlash(newArgs)), "new args")

			if c.success {
				stash.discard()
			} else if err := stash.restore(); err != nil {
				t.Fatal(err)
			}
			for rel, want := range c.want {
				if got := readTestFile(filepath.Join(sub, filepath.FromSlash(rel))); got != want {
					t.Errorf("%s = %q, want %q", rel, got, want)
				}
			}
			if got := resolveLaunch(sub); got.Mode != launchArgfile || got.ArgsFile != c.wantArgs {
				t.Errorf("resolveLaunch = %+v, want args file %s", got, c.wantArgs)
			}
		})
	}
}

// A node restart mid-install leaves the old software in the stash and the
// sub-server without it. The redelivered attempt used to delete that stash
// first, so a second failure left no jar at all; what the stash holds goes
// back before the new stash is taken. It wins over what is in place under the
// same name, which only the attempt that died can have written.
func TestJarStashRecoversALeftOverStash(t *testing.T) {
	serverPath := t.TempDir()
	sub := filepath.Join(serverPath, "main")
	left := filepath.Join(serverPath, ".main.oldjars", "main")
	writeTestFile(t, filepath.Join(left, "server.jar"), "old jar")
	writeTestFile(t, filepath.Join(left, "versions", "1.21.1", "paper.jar"), "old paper")
	writeTestFile(t, filepath.Join(left, "mods", "sodium-old.jar"), "old mod")
	writeTestFile(t, filepath.Join(left, "eula-helper.jar"), "stale")
	writeTestFile(t, filepath.Join(sub, "eula-helper.jar"), "current")
	writeTestFile(t, filepath.Join(sub, "world", "level.dat"), "world")

	stash, err := stashServerJars(serverPath, "main")
	if err != nil {
		t.Fatal(err)
	}
	// The redelivered install fails as well.
	if err := stash.restore(); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"server.jar":                "old jar",
		"versions/1.21.1/paper.jar": "old paper",
		"mods/sodium-old.jar":       "old mod",
		"eula-helper.jar":           "stale",
		"world/level.dat":           "world",
	} {
		if got := readTestFile(filepath.Join(sub, filepath.FromSlash(rel))); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
}

// The node dies mid-move after the new server jar and the new mods are in
// place, and the redelivered move fails too. The old software comes back and
// the dead attempt's mods go: it used to keep the new jar (what was in place
// won), delete the only old copy, and leave both versions of every mod, which
// Fabric and Forge refuse to start on.
func TestJarStashRecoversWhatADeadAttemptWrote(t *testing.T) {
	serverPath := t.TempDir()
	sub := filepath.Join(serverPath, "main")
	writeTestFile(t, filepath.Join(sub, "server.jar"), "old jar")
	writeTestFile(t, filepath.Join(sub, "mods", "sodium-old.jar"), "old mod")

	// The attempt that dies: stash, swap the mods, install, no restore.
	dead, err := stashServerJars(serverPath, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := dead.takeFile(filepath.Join(sub, "mods", "sodium-old.jar"), "main/mods/sodium-old.jar"); err != nil {
		t.Fatal(err)
	}
	dead.add("main/mods/sodium-new.jar")
	writeTestFile(t, filepath.Join(sub, "mods", "sodium-new.jar"), "new mod")
	writeTestFile(t, filepath.Join(sub, "server.jar"), "new jar")
	dead.root.Close()

	// The redelivery stashes again, finds the new mod already in place (so
	// cannot claim it) and fails.
	again, err := stashServerJars(serverPath, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := again.takeFile(filepath.Join(sub, "mods", "sodium-old.jar"), "main/mods/sodium-old.jar"); err != nil {
		t.Fatal(err)
	}
	if err := again.restore(); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"server.jar":          "old jar",
		"mods/sodium-old.jar": "old mod",
		"mods/sodium-new.jar": "<missing>",
	} {
		if got := readTestFile(filepath.Join(sub, filepath.FromSlash(rel))); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
}

// A stash that stopped part-way puts back only what it took. Its restore used
// to clear every root jar as well, and the ones it had not reached yet - the
// server jar among them - had no copy anywhere.
func TestJarStashPartialRestoreKeepsWhatItNeverTook(t *testing.T) {
	serverPath := t.TempDir()
	sub := filepath.Join(serverPath, "main")
	writeTestFile(t, filepath.Join(sub, "server.jar"), "old jar")
	writeTestFile(t, filepath.Join(sub, "versions", "1.21.1", "paper.jar"), "old paper")
	root, err := os.OpenRoot(serverPath)
	if err != nil {
		t.Fatal(err)
	}
	// What stashServerJars holds after taking versions/ and failing before
	// the root jars.
	s := &jarStash{root: root, dir: ".main.oldjars", sub: "main"}
	if err := root.Mkdir(s.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.take("main/versions"); err != nil {
		t.Fatal(err)
	}
	if err := s.restore(); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"server.jar":                "old jar",
		"versions/1.21.1/paper.jar": "old paper",
	} {
		if got := readTestFile(filepath.Join(sub, filepath.FromSlash(rel))); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
}

// A loader Core accepts as a Modrinth tag but the node cannot install is
// refused before anything is stopped or changed, and Core is told to put its
// writes back.
func TestVersionMoveRefusesALoaderItCannotInstall(t *testing.T) {
	for _, typ := range []string{"purpur", "spigot", "bukkit", "quilt", "", "velocity", "waterfall", "bungeecord"} {
		t.Run(typ, func(t *testing.T) {
			root := t.TempDir()
			const uuid = "srv-move-purpur"
			jar := filepath.Join(root, uuid, "main", "server.jar")
			mod := filepath.Join(root, uuid, "main", "plugins", "a.jar")
			writeTestFile(t, jar, "purpur")
			writeTestFile(t, mod, "plugin")
			rdb := newMiniRedis(t)
			payload, _ := json.Marshal(map[string]interface{}{"config": map[string]interface{}{
				"uuid": uuid, "activeSubServer": "main", "targetDir": "plugins", "remove": []string{"a.jar"},
			}})
			// dm is nil: reaching the stop would panic, which is the point.
			runUpdateServerVersion(context.Background(), rdb, nil, &StorageManager{paths: []string{root}},
				NodeCommand{Installer: InstallerConfig{Type: typ}}, string(payload))

			if got, _ := rdb.Get(context.Background(), "dylaris:server:"+uuid+":version_update_failed").Result(); got == "" {
				t.Error("Core was not told the move did not happen")
			}
			if readTestFile(jar) != "purpur" || readTestFile(mod) != "plugin" {
				t.Error("the server was changed before the move was refused")
			}
		})
	}
}

// The tail of a version move takes the same steps setup and reinstall take
// before they start a server, and deletes nothing it cannot put back.
func TestVersionMoveTakesTheSiblingSteps(t *testing.T) {
	src, err := os.ReadFile("update_server_version.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := cutFunc(string(src), "func runUpdateServerVersion(")
	if !ok {
		t.Fatal("runUpdateServerVersion not found")
	}
	install := strings.Index(body, "InstallServer(")
	for _, c := range []struct{ want, why string }{
		{"discardIfDeleted(pl.UUID, serverPath)", "a server deleted during the move got its directory back"},
		{"dm.RecreateStopped", "a server whose owner was cut off during the move was started"},
		{"provisionFloodgateKey(", "the Floodgate key was not provisioned before the start"},
		{`fail("the install", err)`, "a failed install left the server with no jar and Core with the new version"},
	} {
		if i := strings.Index(body, c.want); i < 0 || i < install {
			t.Errorf("%s missing or before the install: %s", c.want, c.why)
		}
	}
	if strings.Contains(body, "CleanServerJars(") {
		t.Error("the move deletes the server jars before the new ones are installed")
	}
	main, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	re, ok := cutFunc(string(main), `case "reinstall":`)
	if ok {
		if i := strings.Index(re, `case "migrate_storage":`); i > 0 {
			re = re[:i]
		}
	}
	if !ok || strings.Contains(re, "CleanServerJars(") || !strings.Contains(re, "stashServerJars(") {
		t.Error("reinstall deletes the server jars before the new ones are installed")
	}
	// Core wrote the new type and version before it sent the reinstall; a
	// rollback that tells nothing left the DB naming software that never ran.
	if !strings.Contains(re, "reportSetupFailed(ctx, rdb, cmd.Config.UUID, subName, cause)") || strings.Contains(re, "putBack()") {
		t.Error("a rolled-back reinstall does not tell Core why on every path")
	}
}

// A copy that will not appear tells Core, so the rows it wrote for the target
// when it queued the copy can go; a successful one says nothing.
func TestCopySubServerReportsAFailure(t *testing.T) {
	for _, c := range []struct {
		name       string
		makeSource bool
		wantReport bool
	}{
		{"source missing", false, true},
		{"copied", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			const uuid = "srv-copy"
			if c.makeSource {
				writeTestFile(t, filepath.Join(root, uuid, "main", "world", "level.dat"), "world")
			} else if err := os.MkdirAll(filepath.Join(root, uuid), 0o755); err != nil {
				t.Fatal(err)
			}
			rdb := newMiniRedis(t)
			payload, _ := json.Marshal(map[string]interface{}{"config": map[string]interface{}{
				"uuid": uuid, "sourceSubServer": "main", "targetSubServer": "copy",
			}})
			runCopySubServer(context.Background(), rdb, &StorageManager{paths: []string{root}}, string(payload))

			raw, _ := rdb.Get(context.Background(), "dylaris:server:"+uuid+":copy_sub_failed").Result()
			if (raw != "") != c.wantReport {
				t.Fatalf("failure report = %q, want reported=%v", raw, c.wantReport)
			}
			if c.wantReport {
				var rep struct{ Target, Reason string }
				if json.Unmarshal([]byte(raw), &rep) != nil || rep.Target != "copy" || rep.Reason == "" {
					t.Errorf("report %q does not name the target and a reason", raw)
				}
			} else if readTestFile(filepath.Join(root, uuid, "copy", "world", "level.dat")) != "world" {
				t.Error("the copy did not land")
			}
		})
	}
}

// A copy to another target while one runs is refused, and Core is told so it
// can remove the rows it wrote for that target; a redelivery of the running
// copy is dropped quietly, or Core would remove the rows of the copy that runs.
func TestCopySubServerBesideARunningCopy(t *testing.T) {
	for _, c := range []struct {
		name       string
		running    string // the target of the copy already running
		wantReport bool
	}{
		{"another target", "other", true},
		{"same target", "copy", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			const uuid = "srv-copy-busy"
			writeTestFile(t, filepath.Join(root, uuid, "main", "world", "level.dat"), "world")
			// What the running copy holds.
			for _, k := range []string{"copy_sub:" + uuid, "copy_sub:" + uuid + "/" + c.running} {
				commandsInFlight.enter(k)
				defer commandsInFlight.leave(k)
			}
			rdb := newMiniRedis(t)
			payload, _ := json.Marshal(map[string]interface{}{"config": map[string]interface{}{
				"uuid": uuid, "sourceSubServer": "main", "targetSubServer": "copy",
			}})
			runCopySubServer(context.Background(), rdb, &StorageManager{paths: []string{root}}, string(payload))

			raw, _ := rdb.Get(context.Background(), "dylaris:server:"+uuid+":copy_sub_failed").Result()
			if (raw != "") != c.wantReport {
				t.Fatalf("failure report = %q, want reported=%v", raw, c.wantReport)
			}
			if readTestFile(filepath.Join(root, uuid, "copy", "world", "level.dat")) != "<missing>" {
				t.Error("a copy ran beside the running one")
			}
		})
	}
}

// A download that fails leaves the file it would have replaced as it was. The
// destination used to be truncated before the request went out, so a 404 for
// a Vanilla or Fabric jar left an empty one in place of the working jar.
func TestAFailedDownloadKeepsTheFileItWouldReplace(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		want   string
	}{
		{"404", http.StatusNotFound, "working jar"},
		{"500", http.StatusInternalServerError, "working jar"},
		{"200", http.StatusOK, "new jar"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				w.Write([]byte("new jar"))
			}))
			defer srv.Close()
			dir := t.TempDir()
			dest := filepath.Join(dir, "server.jar")
			writeTestFile(t, dest, "working jar")

			err := downloadWith(http.DefaultClient, srv.URL, dest, 5*time.Second, 0)
			if (err == nil) != (c.status == http.StatusOK) {
				t.Fatalf("err = %v for HTTP %d", err, c.status)
			}
			if got := readTestFile(dest); got != c.want {
				t.Errorf("server.jar = %q, want %q", got, c.want)
			}
			if _, err := os.Lstat(dest + ".dl"); !os.IsNotExist(err) {
				t.Errorf("the staging file was left behind: %v", err)
			}
		})
	}
}

// A version move never runs beside a copy of the same server: the copy is
// what makes the move reversible, and copyDir walking the tree while the move
// swaps its jars captured a mix of both. Refused before anything is stopped.
func TestVersionMoveRefusesBesideARunningCopy(t *testing.T) {
	root := t.TempDir()
	const uuid = "srv-move-copying"
	jar := filepath.Join(root, uuid, "main", "server.jar")
	writeTestFile(t, jar, "paper")
	commandsInFlight.enter("copy_sub:" + uuid)
	defer commandsInFlight.leave("copy_sub:" + uuid)
	rdb := newMiniRedis(t)
	payload, _ := json.Marshal(map[string]interface{}{"config": map[string]interface{}{
		"uuid": uuid, "activeSubServer": "main", "targetDir": "plugins",
	}})
	// dm is nil: reaching the stop would panic, which is the point.
	runUpdateServerVersion(context.Background(), rdb, nil, &StorageManager{paths: []string{root}},
		NodeCommand{Installer: InstallerConfig{Type: "paper"}}, string(payload))

	if got, _ := rdb.Get(context.Background(), "dylaris:server:"+uuid+":version_update_failed").Result(); got == "" {
		t.Error("Core was not told the move did not happen")
	}
	if readTestFile(jar) != "paper" {
		t.Error("the server was changed beside the copy")
	}
}

// A move redelivered after the node died mid-move finds the old software in
// the stash and no jar at the root. If that attempt aborts while staging, it
// says the server was left as it was, so the old software is back first: the
// stash used to be recovered only by the next stashServerJars, which an
// aborted move never reaches, and every start crash-looped.
func TestVersionMoveRecoversAStrandedStashBeforeItCanAbort(t *testing.T) {
	root := t.TempDir()
	const uuid = "srv-move-stranded"
	sub := filepath.Join(root, uuid, "main")
	writeTestFile(t, filepath.Join(root, uuid, ".main.oldjars", "main", "server.jar"), "old jar")
	writeTestFile(t, filepath.Join(root, uuid, ".main.oldjars", "main", "versions", "1.21.1", "paper.jar"), "old paper")
	writeTestFile(t, filepath.Join(sub, "world", "level.dat"), "world")
	rdb := newMiniRedis(t)
	payload, _ := json.Marshal(map[string]interface{}{"config": map[string]interface{}{
		"uuid": uuid, "activeSubServer": "main", "targetDir": "plugins",
		"install": []map[string]string{{"fileName": "../escape.jar", "downloadUrl": "https://cdn.modrinth.com/x.jar"}},
	}})
	// dm is nil: reaching the stop would panic, which is the point.
	runUpdateServerVersion(context.Background(), rdb, nil, &StorageManager{paths: []string{root}},
		NodeCommand{Installer: InstallerConfig{Type: "paper"}}, string(payload))

	if got, _ := rdb.Get(context.Background(), "dylaris:server:"+uuid+":version_update_failed").Result(); got == "" {
		t.Error("Core was not told the move did not happen")
	}
	for rel, want := range map[string]string{
		"server.jar":                "old jar",
		"versions/1.21.1/paper.jar": "old paper",
		"world/level.dat":           "world",
	} {
		if got := readTestFile(filepath.Join(sub, filepath.FromSlash(rel))); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, uuid, ".main.oldjars")); !os.IsNotExist(err) {
		t.Errorf("the stash is still there: %v", err)
	}
}
