package main

import (
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// serveFiles serves path -> body from an httptest server on loopback and lets
// the guarded client dial it for the length of the test.
func serveFiles(t *testing.T, files map[string][]byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	allowDials(t, func(string, string) bool { return true })
	return srv.URL
}

type loaderCall struct{ kind, mc, version string }

// stubLoaders records the loader install instead of reaching Maven/Docker.
func stubLoaders(t *testing.T) *[]loaderCall {
	t.Helper()
	var calls []loaderCall
	pf, pn, pb, pr := technicInstallForge, technicInstallNeoForge, technicInstallFabric, technicRunInstaller
	t.Cleanup(func() {
		technicInstallForge, technicInstallNeoForge, technicInstallFabric, technicRunInstaller = pf, pn, pb, pr
	})
	technicInstallForge = func(dir, mc, build, _, _ string) error {
		calls = append(calls, loaderCall{"forge", mc, build})
		return os.WriteFile(filepath.Join(dir, "forge-"+mc+"-"+build+".jar"), nil, 0o644)
	}
	technicInstallNeoForge = func(dir, v, _, _ string) error {
		calls = append(calls, loaderCall{"neoforge", "", v})
		return nil
	}
	technicInstallFabric = func(dir, mc, v string) error {
		calls = append(calls, loaderCall{"fabric", mc, v})
		return os.WriteFile(filepath.Join(dir, "fabric-server-launch.jar"), nil, 0o644)
	}
	technicRunInstaller = func(_, _, _, jar string, _ ...string) error {
		calls = append(calls, loaderCall{"installer", "", jar})
		return nil
	}
	return &calls
}

// noStageLeft fails when an install left its staging or download dir behind.
func noStageLeft(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".technic-") {
			t.Errorf("left behind: %s", e.Name())
		}
	}
}

func TestInstallTechnicServerPack(t *testing.T) {
	// Tekkit Classic's shape: everything under one folder, the jar under a name
	// of the author's choosing.
	pack := zipBytes(t, map[string]string{
		"Tekkit_Server/Tekkit.jar":        "jar",
		"Tekkit_Server/mods/a.zip":        "mod",
		"Tekkit_Server/config/a.cfg":      "cfg",
		"Tekkit_Server/.dylaris.json":     "planted",
		"Tekkit_Server/server.properties": "motd=x",
	})
	base := serveFiles(t, map[string][]byte{"/pack.zip": pack})
	calls := stubLoaders(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".dylaris.json"), []byte("ours"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := installTechnic(dir, InstallerConfig{Variant: "server", URL: base + "/pack.zip"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"server.jar", "mods/a.zip", "config/a.cfg", "server.properties"} {
		if !exists(filepath.Join(dir, p)) {
			t.Errorf("missing %s", p)
		}
	}
	if exists(filepath.Join(dir, "Tekkit_Server")) {
		t.Error("the single top folder was not lifted")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".dylaris.json")); string(b) != "ours" {
		t.Errorf("a protected file was replaced by the pack: %q", b)
	}
	if len(*calls) != 0 {
		t.Errorf("a server pack ran a loader install: %v", *calls)
	}
	noStageLeft(t, dir)
}

// A jar next to a folder is NOT a single wrapping folder: lifting mods/ into
// the root would scatter the mods.
func TestInstallTechnicServerPackDoesNotLiftBesideFiles(t *testing.T) {
	pack := zipBytes(t, map[string]string{"Tekkit.jar": "jar", "mods/a.zip": "mod"})
	base := serveFiles(t, map[string][]byte{"/pack.zip": pack})
	stubLoaders(t)
	dir := t.TempDir()
	if err := installTechnic(dir, InstallerConfig{Variant: "server", URL: base + "/pack.zip"}); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, "mods", "a.zip")) || exists(filepath.Join(dir, "a.zip")) {
		t.Error("mods/ was lifted into the root")
	}
}

func TestInstallTechnicServerPackRunsItsInstaller(t *testing.T) {
	pack := zipBytes(t, map[string]string{"forge-1.16.5-36.2.34-installer.jar": "i", "mods/a.jar": "m"})
	base := serveFiles(t, map[string][]byte{"/pack.zip": pack})
	calls := stubLoaders(t)
	technicRunInstaller = func(_, _, _, jar string, _ ...string) error {
		*calls = append(*calls, loaderCall{"installer", "", jar})
		return nil
	}
	dir := t.TempDir()
	err := installTechnic(dir, InstallerConfig{Variant: "server", URL: base + "/pack.zip"})
	// The stub installs nothing, so after it the pack is still not startable;
	// what matters is that the pack's installer was the one run.
	if len(*calls) != 1 || (*calls)[0].version != "forge-1.16.5-36.2.34-installer.jar" {
		t.Fatalf("installer calls = %v", *calls)
	}
	if err == nil || !strings.Contains(err.Error(), "no server jar") {
		t.Errorf("err = %v, want the no-server-jar message", err)
	}
}

func TestInstallTechnicClientZip(t *testing.T) {
	pack := zipBytes(t, map[string]string{
		"bin/version.json": `{"inheritsFrom":"1.20.1","libraries":[{"name":"net.fabricmc:fabric-loader:0.15.11"}]}`,
		"mods/a.jar":       "m",
		"config/a.toml":    "c",
	})
	base := serveFiles(t, map[string][]byte{"/client.zip": pack})
	calls := stubLoaders(t)
	dir := t.TempDir()
	if err := installTechnic(dir, InstallerConfig{Variant: "client-zip", URL: base + "/client.zip"}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || (*calls)[0] != (loaderCall{"fabric", "1.20.1", "0.15.11"}) {
		t.Errorf("loader calls = %v", *calls)
	}
	for _, p := range []string{"mods/a.jar", "config/a.toml", "fabric-server-launch.jar"} {
		if !exists(filepath.Join(dir, p)) {
			t.Errorf("missing %s", p)
		}
	}
	if exists(filepath.Join(dir, "bin")) {
		t.Error("the client loader in bin/ was copied into the server")
	}
	noStageLeft(t, dir)
}

func md5hex(b []byte) string { s := md5.Sum(b); return hex.EncodeToString(s[:]) }

func TestInstallTechnicSolder(t *testing.T) {
	forgeZip := zipBytes(t, map[string]string{"bin/modpack.jar": string(zipBytes(t, map[string]string{
		"version.json": `{"inheritsFrom":"1.16.5","libraries":[{"name":"net.minecraftforge:forge:1.16.5-36.2.34"}]}`,
	}))})
	modZip := zipBytes(t, map[string]string{"mods/pixelmon.jar": "p"})
	base := serveFiles(t, map[string][]byte{"/forge.zip": forgeZip, "/mod.zip": modZip})
	calls := stubLoaders(t)
	dir := t.TempDir()

	cfg := InstallerConfig{Variant: "client-solder", TechnicMods: []TechnicMod{
		{URL: base + "/forge.zip", MD5: md5hex(forgeZip)},
		{URL: base + "/mod.zip", MD5: strings.ToUpper(md5hex(modZip))},
	}}
	if err := installTechnic(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || (*calls)[0] != (loaderCall{"forge", "1.16.5", "36.2.34"}) {
		t.Errorf("loader calls = %v", *calls)
	}
	if !exists(filepath.Join(dir, "mods", "pixelmon.jar")) {
		t.Error("the Solder mod did not arrive")
	}
	noStageLeft(t, dir)
}

func TestInstallTechnicRefusals(t *testing.T) {
	modZip := zipBytes(t, map[string]string{"mods/a.jar": "a"})
	base := serveFiles(t, map[string][]byte{"/mod.zip": modZip})
	stubLoaders(t)

	cases := []struct {
		name string
		cfg  InstallerConfig
		want string
	}{
		{"checksum", InstallerConfig{Variant: "client-solder", TechnicMods: []TechnicMod{{URL: base + "/mod.zip", MD5: "00000000000000000000000000000000"}}}, "checksum"},
		{"scheme", InstallerConfig{Variant: "server", URL: "file:///etc/passwd"}, "http(s)"},
		{"variant", InstallerConfig{Variant: "mystery", URL: base + "/mod.zip"}, "variant"},
		{"empty solder", InstallerConfig{Variant: "client-solder"}, "no files"},
		{"no loader", InstallerConfig{Variant: "client-zip", URL: base + "/mod.zip"}, "mod loader"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			err := installTechnic(dir, c.cfg)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to mention %q", err, c.want)
			}
			noStageLeft(t, dir)
		})
	}
}

// A pack whose only top-level entry is mods/ is not a wrapped server: lifting
// it would scatter the mods into the root.
func TestInstallTechnicDoesNotLiftALoneModsDir(t *testing.T) {
	pack := zipBytes(t, map[string]string{"mods/a.jar": "m", "mods/b.jar": "m"})
	base := serveFiles(t, map[string][]byte{"/pack.zip": pack})
	stubLoaders(t)
	dir := t.TempDir()
	installTechnic(dir, InstallerConfig{Variant: "server", URL: base + "/pack.zip"})
	if !exists(filepath.Join(dir, "mods", "a.jar")) || exists(filepath.Join(dir, "a.jar")) {
		t.Error("a lone mods/ was lifted into the root")
	}
}

func TestInstallTechnicUnpackBudget(t *testing.T) {
	pack := zipBytes(t, map[string]string{"server.jar": strings.Repeat("x", 4096)})
	base := serveFiles(t, map[string][]byte{"/pack.zip": pack})
	stubLoaders(t)

	budget := int64(4095)
	dir := t.TempDir()
	stage := t.TempDir()
	err := fetchTechnicArchive(base+"/pack.zip", "", filepath.Join(dir, "p.zip"), technicPackMax, stage, &budget)
	if err == nil || !strings.Contains(err.Error(), "unpacks to more") {
		t.Fatalf("err = %v, want the unpack budget refusal", err)
	}
	if exists(filepath.Join(stage, "server.jar")) {
		t.Error("an over-budget archive was extracted")
	}
	budget = 4096
	if err := fetchTechnicArchive(base+"/pack.zip", "", filepath.Join(dir, "p.zip"), technicPackMax, stage, &budget); err != nil || budget != 0 {
		t.Fatalf("exactly at the budget: err = %v, left = %d", err, budget)
	}
}

// A symlink the tenant planted in the server directory must not carry a file
// outside it.
func TestInstallTechnicRefusesPlantedSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows; covered on Linux CI")
	}
	pack := zipBytes(t, map[string]string{"server.jar": "j", "config/evil.cfg": "x"})
	base := serveFiles(t, map[string][]byte{"/pack.zip": pack})
	stubLoaders(t)
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "config")); err != nil {
		t.Fatal(err)
	}
	installTechnic(dir, InstallerConfig{Variant: "server", URL: base + "/pack.zip"})
	if exists(filepath.Join(outside, "evil.cfg")) {
		t.Fatal("the pack wrote through a planted symlink")
	}
}

func TestTechnicStagingIsProtected(t *testing.T) {
	for _, p := range []string{".technic-stage-123/mods/a.jar", "sub/.technic-dl-9/pack.zip", ".technic-stage-x"} {
		if !isProtectedFile(p) {
			t.Errorf("%s is not protected", p)
		}
	}
}
