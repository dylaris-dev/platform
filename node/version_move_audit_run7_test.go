package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	paperBuildsPath   = "/v3/projects/paper/versions/1.21.4/builds/latest"
	paperBuildsBody   = `{"id":1,"downloads":{"server:default":{"name":"p.jar","url":"https://fill-data.papermc.io/p.jar"}}}`
	paperBuild130Path = "/v3/projects/paper/versions/1.21.4/builds/130"
	paperBuild130Body = `{"id":130,"downloads":{"server:default":{"name":"p.jar","url":"https://fill-data.papermc.io/p130.jar"}}}`
	fabricLoaderPath  = "/v2/versions/loader/1.21.4"
	forgePromosPath   = "/net/minecraftforge/forge/promotions_slim.json"
	forgeJarPath      = "/net/minecraftforge/forge/1.20.1-47.3.0/forge-1.20.1-47.3.0-installer.jar"
	neoJarPath        = "/releases/net/neoforged/neoforge/21.1.9/neoforge-21.1.9-installer.jar"
)

// M11: every loader a move can target is checked upstream before the stop,
// not only a blank NeoForge build. The resolved build is what the install
// then uses.
func TestResolveVersionMoveTarget(t *testing.T) {
	stubInstallerMeta(t, metaTransport{
		paperBuildsPath:              paperBuildsBody,
		paperBuild130Path:            paperBuild130Body,
		fabricLoaderPath:             `[{"loader":{"version":"0.17.0-beta","stable":false}},{"loader":{"version":"0.16.9","stable":true}}]`,
		"/v2/versions/loader/1.99.0": `[]`,
		forgePromosPath:              `{"promos":{"1.20.1-recommended":"47.3.0"}}`,
		forgeJarPath:                 "jar",
		neoJarPath:                   "jar",
	})
	tests := []struct {
		name      string
		cfg       InstallerConfig
		want      string
		wantError bool
	}{
		{"paper with a build", InstallerConfig{Type: "paper", Version: "1.21.4"}, "", false},
		{"paper with no build yet", InstallerConfig{Type: "paper", Version: "1.21.5"}, "", true},
		// N08: a typed build is checked like a Forge or NeoForge one.
		{"paper typed build that exists", InstallerConfig{Type: "paper", Version: "1.21.4", Loader: "130"}, "130", false},
		{"paper typed build that does not", InstallerConfig{Type: "paper", Version: "1.21.4", Loader: "99999"}, "", true},
		{"fabric blank loader", InstallerConfig{Type: "fabric", Version: "1.21.4"}, "0.16.9", false},
		{"fabric version with no loader", InstallerConfig{Type: "fabric", Version: "1.99.0"}, "", true},
		{"fabric typed loader that exists", InstallerConfig{Type: "fabric", Version: "1.21.4", Loader: "0.17.0-beta"}, "0.17.0-beta", false},
		{"fabric typed loader that does not", InstallerConfig{Type: "fabric", Version: "1.21.4", Loader: "0.1.0"}, "", true},
		{"forge blank build", InstallerConfig{Type: "forge", Version: "1.20.1"}, "47.3.0", false},
		{"forge blank build with no promotion", InstallerConfig{Type: "forge", Version: "1.21.4"}, "", true},
		{"forge typed build that exists", InstallerConfig{Type: "forge", Version: "1.20.1", Loader: "47.3.0"}, "47.3.0", false},
		{"forge typed build that does not", InstallerConfig{Type: "forge", Version: "1.20.1", Loader: "99.9.9"}, "", true},
		{"neoforge typed build that exists", InstallerConfig{Type: "neoforge", Version: "1.21.1", Loader: "21.1.9"}, "21.1.9", false},
		{"neoforge typed build that does not", InstallerConfig{Type: "neoforge", Version: "1.21.1", Loader: "21.1.999"}, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveVersionMoveTarget(tc.cfg)
			if (err != nil) != tc.wantError {
				t.Fatalf("err = %v, wantError %v", err, tc.wantError)
			}
			if err == nil && got != tc.want {
				t.Errorf("loader = %q, want %q", got, tc.want)
			}
		})
	}
}

// M11: a Paper move to a version Paper has no build for is refused with the
// server still running. It used to be found out after the stop.
func TestVersionMoveChecksThePaperBuildBeforeTheStop(t *testing.T) {
	stubInstallerMeta(t, metaTransport{})
	root := t.TempDir()
	const uuid = "srv-move-paper404"
	jar := filepath.Join(root, uuid, "main", "server.jar")
	writeTestFile(t, jar, "paper")
	dm, stops := runningDocker(t, uuid, http.StatusNoContent)
	rdb := newMiniRedis(t)
	payload, _ := json.Marshal(map[string]interface{}{"config": map[string]interface{}{
		"uuid": uuid, "activeSubServer": "main", "targetDir": "plugins",
	}})
	runUpdateServerVersion(context.Background(), rdb, dm, &StorageManager{paths: []string{root}},
		NodeCommand{Installer: InstallerConfig{Type: "paper", Version: "1.21.5"}}, string(payload))

	if got, _ := rdb.Get(context.Background(), "dylaris:server:"+uuid+":version_update_failed").Result(); got == "" {
		t.Error("Core was not told the move did not happen")
	}
	if stops.Load() != 0 {
		t.Error("the server was stopped for a version Paper has no build for")
	}
	if readTestFile(jar) != "paper" {
		t.Error("the server was changed")
	}
}

// M13: the move saves what it rebuilt the container from, as setup does. The
// reconciler recreates a missing container from .node_config.json, and the
// setup-time image and start command crash-looped the moved server.
func TestVersionMoveSavesTheNodeConfig(t *testing.T) {
	src, err := os.ReadFile("update_server_version.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := cutFunc(string(src), "func runUpdateServerVersion(")
	if !ok {
		t.Fatal("runUpdateServerVersion not found")
	}
	cmdSet := strings.Index(body, "cmd.Config.Docker.Command = startCmd")
	for _, want := range []string{"saveNodeConfig(serverPath, cmd.Config)", "refreshServerMetadata(serverPath, pl.UUID,"} {
		if i := strings.Index(body, want); i < 0 || i < cmdSet {
			t.Errorf("%s missing or before the start command is set", want)
		}
	}
	mainSrc, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(mainSrc)
	i := strings.Index(s, "reinstallRecreate(cmd.Config)")
	j := strings.Index(s, `case "migrate_storage":`)
	if i < 0 || j < i || !strings.Contains(s[i:j], "saveNodeConfig(serverPath, cmd.Config)") {
		t.Error("reinstall does not save the config it recreated the container from")
	}
}

// M14: a staged jar that cannot be renamed into place fails the move instead
// of starting the server without that mod.
func TestPlaceStagedJarsReportsAFailedRename(t *testing.T) {
	serverPath := t.TempDir()
	modsDir := filepath.Join(serverPath, "main", "mods")
	writeTestFile(t, filepath.Join(serverPath, "main", "server.jar"), "old")
	writeTestFile(t, filepath.Join(modsDir, "a.jar.part"), "a")
	staged := []stagedJar{
		{tmp: filepath.Join(modsDir, "a.jar.part"), final: filepath.Join(modsDir, "a.jar")},
		{tmp: filepath.Join(modsDir, "b.jar.part"), final: filepath.Join(modsDir, "b.jar")}, // its .part vanished
	}
	stash, err := stashServerJars(serverPath, "main")
	if err != nil {
		t.Fatal(err)
	}
	name, err := placeStagedJars(stash, "main/mods", staged)
	if err == nil || name != "b.jar" {
		t.Fatalf("placeStagedJars = %q, %v; want b.jar and an error", name, err)
	}
	if err := stash.restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(modsDir, "a.jar")); !os.IsNotExist(err) {
		t.Errorf("a.jar the move put in place is still there after the restore: %v", err)
	}
	if readTestFile(filepath.Join(serverPath, "main", "server.jar")) != "old" {
		t.Error("the old server jar was not put back")
	}
}

// N08: the build a Paper move names is the one installed. installPaper took
// only the version and installed the latest, while Core recorded the typed one.
func TestInstallPaperUsesTheTypedBuild(t *testing.T) {
	rt := metaTransport{
		paperBuildsPath:   paperBuildsBody,
		paperBuild130Path: paperBuild130Body,
		"/p.jar":          "latest",
		"/p130.jar":       "build130",
	}
	stubInstallerMeta(t, rt)
	prev := installerDownloadClient
	installerDownloadClient = &http.Client{Transport: rt}
	t.Cleanup(func() { installerDownloadClient = prev })
	for _, tc := range []struct{ build, want string }{{"", "latest"}, {"130", "build130"}} {
		dir := t.TempDir()
		if err := installPaper(dir, "1.21.4", tc.build); err != nil {
			t.Fatalf("build %q: %v", tc.build, err)
		}
		if got := readTestFile(filepath.Join(dir, "server.jar")); got != tc.want {
			t.Errorf("build %q installed %q, want %q", tc.build, got, tc.want)
		}
	}
	if err := installPaper(t.TempDir(), "1.21.4", "../latest"); err == nil {
		t.Error("a build that is not a number was sent to the API")
	}
}
