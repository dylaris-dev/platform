package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// The URL is a real cdn.modrinth.com one because validateModrinthURL still runs
// - only the fetch is replaced. See downloadModFile.
const testModURL = "https://cdn.modrinth.com/data/AABBCC/versions/1/spark-1.1.jar"

// stubDownload replaces the fetch for one test: it either writes bytes to the
// destination the way a real download does, or fails without touching it.
func stubDownload(t *testing.T, fail error) {
	t.Helper()
	orig := downloadModFile
	downloadModFile = func(_, dest, _ string) error {
		if fail != nil {
			return fail
		}
		return os.WriteFile(dest, []byte("new jar bytes"), 0o644)
	}
	t.Cleanup(func() { downloadModFile = orig })
}

// installFixture builds a server tree that already carries spark-1.0.jar.
func installFixture(t *testing.T) (*StorageManager, string, string) {
	t.Helper()
	sm, _ := newPlacementManager(t, 1)
	const uuid = "srv-uuid"
	modsDir := filepath.Join(sm.Paths()[0], uuid, "survival", "mods")
	if err := os.MkdirAll(modsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modsDir, "spark-1.0.jar"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	return sm, uuid, modsDir
}

func replacePayload(uuid, previous string) string {
	b, _ := json.Marshal(map[string]interface{}{"config": map[string]interface{}{
		"uuid":             uuid,
		"activeSubServer":  "survival",
		"targetDir":        "mods",
		"fileName":         "spark-1.1.jar",
		"downloadUrl":      testModURL,
		"sha512":           "",
		"previousFileName": previous,
		"installId":        "attempt-1",
		"serverId":         7,
		"projectId":        "spark",
	}})
	return string(b)
}

// Updating a mod wrote the new jar BESIDE the old one. The server then loaded
// two builds of the same mod, and only the newer one had a row naming it - the
// upsert is keyed on the project and had already overwritten the old file name
// - so the leftover could not be removed from the panel at all.
func TestInstallReplacesThePreviousJar(t *testing.T) {
	stubDownload(t, nil)
	sm, uuid, modsDir := installFixture(t)

	runInstallMod(context.Background(), nil, sm, replacePayload(uuid, "spark-1.0.jar"))

	if _, err := os.Stat(filepath.Join(modsDir, "spark-1.1.jar")); err != nil {
		t.Fatalf("the new jar is not there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(modsDir, "spark-1.0.jar")); !os.IsNotExist(err) {
		t.Error("the previous jar is still in mods/, so the server loads both builds of the mod")
	}
}

// The ORDER, as its own case, because the obvious reading of "replace the jar"
// is to delete first - and that is worse than the bug. A download that 404s or
// fails its hash would leave the server with NEITHER build: a broken update
// turned into a broken server.
func TestAFailedDownloadLeavesThePreviousJarAlone(t *testing.T) {
	stubDownload(t, fmt.Errorf("404 from the cdn"))
	sm, uuid, modsDir := installFixture(t)

	runInstallMod(context.Background(), nil, sm, replacePayload(uuid, "spark-1.0.jar"))

	if _, err := os.Stat(filepath.Join(modsDir, "spark-1.0.jar")); err != nil {
		t.Fatalf("the previous jar was deleted for an install that never landed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(modsDir, "spark-1.1.jar")); !os.IsNotExist(err) {
		t.Error("a failed download left the destination file behind")
	}
}

func TestNothingIsDeletedWithNoPreviousOrTheSameName(t *testing.T) {
	t.Run("a first install has nothing to replace", func(t *testing.T) {
		stubDownload(t, nil)
		sm, uuid, modsDir := installFixture(t)
		runInstallMod(context.Background(), nil, sm, replacePayload(uuid, ""))
		if _, err := os.Stat(filepath.Join(modsDir, "spark-1.0.jar")); err != nil {
			t.Error("an unrelated jar was removed by an install that replaces nothing")
		}
	})

	// A build that keeps its file name has already been replaced by the rename.
	// Deleting afterwards would delete the jar just installed.
	t.Run("the same file name is already replaced", func(t *testing.T) {
		stubDownload(t, nil)
		sm, uuid, modsDir := installFixture(t)
		runInstallMod(context.Background(), nil, sm, replacePayload(uuid, "spark-1.1.jar"))
		if _, err := os.Stat(filepath.Join(modsDir, "spark-1.1.jar")); err != nil {
			t.Error("the install deleted the very file it had just written")
		}
	})
}

// The name arrives over a queue. Core validates it the same way, but the node is
// the last thing between a queued payload and the filesystem, and this one is
// joined onto a directory in order to delete.
func TestAPreviousNameIsNeverJoinedBlindly(t *testing.T) {
	stubDownload(t, nil)
	sm, uuid, modsDir := installFixture(t)
	outside := filepath.Join(filepath.Dir(modsDir), "server.properties")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	runInstallMod(context.Background(), nil, sm, replacePayload(uuid, "../server.properties"))

	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("a traversing previousFileName deleted a file outside mods/: %v", err)
	}
}

// A loader change moves the target directory (Paper plugins/ -> Fabric mods/),
// and the row still names the jar where it was installed. Deleting the previous
// name inside the NEW directory found nothing, so the old jar stayed loaded and
// no row could name it any more. An old Core sends no previousTargetDir; the
// node then keeps the old behaviour and looks in targetDir.
func TestThePreviousJarIsDeletedFromItsOwnDirectory(t *testing.T) {
	cases := []struct {
		name, prevDir, prevFile string
		wantGone                string // relative to the sub-server dir
	}{
		{"from plugins/ into mods/", "plugins", "Chunky-Bukkit-1.4.jar", "plugins/Chunky-Bukkit-1.4.jar"},
		{"same name, other directory", "plugins", "spark-1.1.jar", "plugins/spark-1.1.jar"},
		{"old core sends no directory", "", "spark-1.0.jar", "mods/spark-1.0.jar"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stubDownload(t, nil)
			sm, uuid, modsDir := installFixture(t)
			sub := filepath.Dir(modsDir)
			if c.prevDir != "" {
				if err := os.MkdirAll(filepath.Join(sub, c.prevDir), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(sub, c.prevDir, c.prevFile), []byte("old"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var m map[string]map[string]interface{}
			_ = json.Unmarshal([]byte(replacePayload(uuid, c.prevFile)), &m)
			m["config"]["previousTargetDir"] = c.prevDir
			b, _ := json.Marshal(m)

			runInstallMod(context.Background(), nil, sm, string(b))

			if _, err := os.Stat(filepath.Join(modsDir, "spark-1.1.jar")); err != nil {
				t.Fatalf("the new jar is not there: %v", err)
			}
			if _, err := os.Stat(filepath.Join(sub, filepath.FromSlash(c.wantGone))); !os.IsNotExist(err) {
				t.Errorf("%s survived the update", c.wantGone)
			}
		})
	}
}

// A previousTargetDir is joined onto the server to delete in, so it gets the
// same check as targetDir.
func TestAnInvalidPreviousDirectoryDeletesNothing(t *testing.T) {
	stubDownload(t, nil)
	sm, uuid, modsDir := installFixture(t)
	outside := filepath.Join(filepath.Dir(modsDir), "server.properties")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var m map[string]map[string]interface{}
	_ = json.Unmarshal([]byte(replacePayload(uuid, "server.properties")), &m)
	m["config"]["previousTargetDir"] = "."
	b, _ := json.Marshal(m)

	runInstallMod(context.Background(), nil, sm, string(b))

	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("an invalid previousTargetDir deleted a file outside mods/plugins: %v", err)
	}
	if _, err := os.Stat(filepath.Join(modsDir, "spark-1.1.jar")); err != nil {
		t.Fatalf("the install itself should still land: %v", err)
	}
}

// Two attempts at the same jar ran at once and shared one "<name>.part": the
// second attempt's Remove unlinked the first one's open file, and the first
// rename then moved the second's half-written download into place. Each
// attempt now writes its own temp file.
func TestConcurrentInstallsOfOneJarDoNotShareATempFile(t *testing.T) {
	sm, uuid, modsDir := installFixture(t)
	orig := downloadModFile
	t.Cleanup(func() { downloadModFile = orig })
	var mu sync.Mutex
	var dests []string
	both := make(chan struct{})
	downloadModFile = func(_, dest, _ string) error {
		mu.Lock()
		dests = append(dests, dest)
		if len(dests) == 2 {
			close(both)
		}
		mu.Unlock()
		<-both // both attempts are mid-download together
		return os.WriteFile(dest, []byte("new jar bytes"), 0o644)
	}

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runInstallMod(context.Background(), nil, sm, replacePayload(uuid, ""))
		}()
	}
	wg.Wait()

	if dests[0] == dests[1] {
		t.Fatalf("both attempts downloaded into %s, so one rename can move the other's partial file into place", dests[0])
	}
	if b, err := os.ReadFile(filepath.Join(modsDir, "spark-1.1.jar")); err != nil || string(b) != "new jar bytes" {
		t.Fatalf("jar after two installs: %q, %v", b, err)
	}
	for _, d := range dests {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("temp file %s left behind", d)
		}
	}
}
