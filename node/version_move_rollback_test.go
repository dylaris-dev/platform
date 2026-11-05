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

// A loader Core accepts as a Modrinth tag but the node cannot install is
// refused before anything is stopped or changed, and Core is told to put its
// writes back.
func TestVersionMoveRefusesALoaderItCannotInstall(t *testing.T) {
	for _, typ := range []string{"purpur", "spigot", "bukkit", "quilt", ""} {
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
