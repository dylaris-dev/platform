package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// install_mod and remove_mod write into <server>/<sub-server>/<mods|plugins>,
// and that directory is the tenant's own: the sub-server folder is bind-mounted
// into their Minecraft container as /data. A plugin in there can replace "mods"
// with a symlink, which every string-level check in install_mod.go passes
// unseen - they validate the payload, not the filesystem.
func TestRemoveModRefusesASymlinkedModsDirectory(t *testing.T) {
	root := t.TempDir()
	sm := NewStorageManager(root, nil)
	const uuid = "srv-remove-mod-symlink"

	subDir := filepath.Join(root, uuid, "survival")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	victimDir := t.TempDir()
	victim := filepath.Join(victimDir, "level.dat")
	if err := os.WriteFile(victim, []byte("ANOTHER TENANT"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, victimDir, filepath.Join(subDir, "mods"))

	runRemoveMod(context.Background(), nil, sm, mustModPayload(t, removeModPayload{
		UUID:            uuid,
		ActiveSubServer: "survival",
		TargetDir:       "mods",
		FileName:        "level.dat",
	}))

	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("remove_mod deleted a file outside the server root: %v", err)
	}
}

// A real mods directory must still work, or the guard above is just a broken
// feature.
func TestRemoveModStillRemovesAnOrdinaryMod(t *testing.T) {
	root := t.TempDir()
	sm := NewStorageManager(root, nil)
	const uuid = "srv-remove-mod-ok"

	modsDir := filepath.Join(root, uuid, "survival", "mods")
	if err := os.MkdirAll(modsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jar := filepath.Join(modsDir, "sodium.jar")
	if err := os.WriteFile(jar, []byte("JAR"), 0o644); err != nil {
		t.Fatal(err)
	}

	runRemoveMod(context.Background(), nil, sm, mustModPayload(t, removeModPayload{
		UUID:            uuid,
		ActiveSubServer: "survival",
		TargetDir:       "mods",
		FileName:        "sodium.jar",
	}))

	if _, err := os.Stat(jar); !os.IsNotExist(err) {
		t.Fatalf("an ordinary mod was not removed: %v", err)
	}
}

// The write side. downloadAndVerify used os.Create, which FOLLOWS a link: the
// downloaded file - content the tenant chooses, by publishing it on Modrinth -
// landed on the link's target. The .part name is predictable because the tenant
// picks which mod to install.
func TestDownloadAndVerifyRefusesToWriteThroughAPlantedSymlink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("MOD-BYTES"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "node_secret")
	if err := os.WriteFile(victim, []byte("THE-NODE-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "sodium.jar.part")
	mustSymlink(t, victim, dest)

	if err := downloadAndVerify(srv.URL, dest, ""); err != nil {
		t.Fatalf("download: %v", err)
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "THE-NODE-SECRET" {
		t.Fatalf("the download was written through the link: victim = %q (%v)", got, err)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != "MOD-BYTES" {
		t.Fatalf("the download did not land at its own path: %q (%v)", got, err)
	}
}

func mustModPayload(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(struct {
		Config any `json:"config"`
	}{Config: v})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
