package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

// runningDocker is a Docker daemon that shows mc_<uuid> running and answers a
// stop with stopStatus. It counts the stops it was sent.
func runningDocker(t *testing.T, uuid string, stopStatus int) (*DockerManager, *atomic.Int32) {
	t.Helper()
	var stops atomic.Int32
	started := time.Now().Format(time.RFC3339Nano) // just started: no 3s save-all
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/mc_"+uuid+"/json"):
			w.Write([]byte(`{"Id":"c","State":{"Running":true,"StartedAt":"` + started + `"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			stops.Add(1)
			w.WriteHeader(stopStatus)
			if stopStatus >= 400 {
				w.Write([]byte(`{"message":"cannot stop"}`))
			}
		default:
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.44"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return &DockerManager{cli: cli, ctx: t.Context(), localDataPath: t.TempDir()}, &stops
}

// L16/L21: a move whose stop fails is abandoned before a single file moves,
// and the server is reported as what Docker says it is. It used to drop the
// stop's error and swap the jars under the running JVM.
func TestVersionMoveAbortsWhenTheStopFails(t *testing.T) {
	prevWait := gracefulStopWait
	gracefulStopWait = 200 * time.Millisecond
	t.Cleanup(func() { gracefulStopWait = prevWait })
	// The move asks Fill for the build before it stops.
	stubInstallerMeta(t, metaTransport{paperBuildsPath: paperBuildsBody})

	root := t.TempDir()
	const uuid = "srv-move-stopfail"
	jar := filepath.Join(root, uuid, "main", "server.jar")
	writeTestFile(t, jar, "paper")
	dm, _ := runningDocker(t, uuid, http.StatusInternalServerError)
	rdb := newMiniRedis(t)
	payload, _ := json.Marshal(map[string]interface{}{"config": map[string]interface{}{
		"uuid": uuid, "activeSubServer": "main", "targetDir": "plugins",
	}})
	runUpdateServerVersion(context.Background(), rdb, dm, &StorageManager{paths: []string{root}},
		NodeCommand{Installer: InstallerConfig{Type: "paper", Version: "1.21.4"}}, string(payload))

	ctx := context.Background()
	if got, _ := rdb.Get(ctx, "dylaris:server:"+uuid+":version_update_failed").Result(); !strings.Contains(got, "could not be stopped") {
		t.Errorf("abandon reason = %q, want the failed stop", got)
	}
	if got, _ := rdb.Get(ctx, "dylaris:server:"+uuid+":status").Result(); got != "online" {
		t.Errorf("status = %q, want online: the server is still running", got)
	}
	if readTestFile(jar) != "paper" {
		t.Error("the server jar was touched although the server could not be stopped")
	}
	if _, err := os.Lstat(filepath.Join(root, uuid, ".main.oldjars")); !os.IsNotExist(err) {
		t.Error("the jars were stashed although the server could not be stopped")
	}
}

// L16: the move stops through gracefulStop and, once it has, clears the
// markers that hold a server down, as reinstall does: a stopped_by_server
// left by an earlier in-game /stop survived the move's own start.
func TestVersionMoveStopsLikeReinstall(t *testing.T) {
	src, err := os.ReadFile("update_server_version.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := cutFunc(string(src), "func runUpdateServerVersion(")
	if !ok {
		t.Fatal("runUpdateServerVersion not found")
	}
	stop := strings.Index(body, "err := gracefulStop(")
	if stop < 0 {
		t.Fatal("the move does not stop through gracefulStop and check its error")
	}
	if strings.Contains(body, `dm.PowerAction(pl.UUID, "stop")`) {
		t.Error("the move still stops with a bare PowerAction")
	}
	if i := strings.Index(body, "clearRunBlockers(ctx, rdb, pl.UUID)"); i < stop {
		t.Error("the move does not clear the run blockers after its stop")
	}
	if i := strings.Index(body, "stashServerJars("); i < stop {
		t.Error("the jars are stashed before the stop")
	}
}

// L20: a move refused before the stop reports the server as what Docker says
// it is, not the busy hold's "installing" (which Core then landed as stopped).
func TestVersionMoveRefusedBeforeTheStopReportsTheRealStatus(t *testing.T) {
	root := t.TempDir()
	const uuid = "srv-move-prestop"
	writeTestFile(t, filepath.Join(root, uuid, "main", "server.jar"), "purpur")
	dm, stops := runningDocker(t, uuid, http.StatusNoContent)
	rdb := newMiniRedis(t)
	payload, _ := json.Marshal(map[string]interface{}{"config": map[string]interface{}{
		"uuid": uuid, "activeSubServer": "main", "targetDir": "plugins",
	}})
	runUpdateServerVersion(context.Background(), rdb, dm, &StorageManager{paths: []string{root}},
		NodeCommand{Installer: InstallerConfig{Type: "purpur"}}, string(payload))

	if got, _ := rdb.Get(context.Background(), "dylaris:server:"+uuid+":status").Result(); got != "online" {
		t.Errorf("status = %q, want online", got)
	}
	if stops.Load() != 0 {
		t.Error("the server was stopped for a refused move")
	}
}

// metaTransport serves installer metadata by URL path, so a resolve can be
// tested without the real Maven.
type metaTransport map[string]string

func (m metaTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body, ok := m[r.URL.Path]
	if !ok {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func stubInstallerMeta(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	prev := installerMetaClient
	installerMetaClient = &http.Client{Transport: rt}
	t.Cleanup(func() { installerMetaClient = prev })
}

const (
	neoforgeMetaPath = "/releases/net/neoforged/neoforge/maven-metadata.xml"
	legacyForgePath  = "/releases/net/neoforged/forge/maven-metadata.xml"
)

// L17: NeoForge for 1.20.1 (47.x) is published as net.neoforged:forge, under
// 1.20.1-<build>. A blank build for 1.20.1 read only the neoforge artifact and
// could never resolve.
func TestResolveNeoForgeVersionFor1201(t *testing.T) {
	stubInstallerMeta(t, metaTransport{
		neoforgeMetaPath: `<metadata><versioning><release>21.1.9</release><versions>` +
			`<version>20.2.3-beta</version><version>21.1.9</version></versions></versioning></metadata>`,
		legacyForgePath: `<metadata><versioning><release>1.20.1-47.1.106</release><versions>` +
			`<version>1.20.1-47.1.7</version><version>1.20.1-47.1.106</version><version>1.20.1-47.1.99</version>` +
			`</versions></versioning></metadata>`,
	})
	for _, c := range []struct{ mc, want string }{
		{"1.20.1", "47.1.106"},
		{"1.21.1", "21.1.9"},
	} {
		got, err := resolveNeoForgeVersion(c.mc)
		if err != nil || got != c.want {
			t.Errorf("resolveNeoForgeVersion(%q) = %q, %v; want %q", c.mc, got, err, c.want)
		}
	}
	if u := neoForgeInstallerURL("47.1.106"); !strings.Contains(u, "/net/neoforged/forge/1.20.1-47.1.106/") {
		t.Errorf("installer URL for the resolved build = %s", u)
	}
}

// L17: a blank NeoForge build is resolved BEFORE the server is stopped, so a
// target upstream does not serve is refused with the server still running.
func TestVersionMoveResolvesTheNeoForgeBuildBeforeTheStop(t *testing.T) {
	stubInstallerMeta(t, metaTransport{})
	root := t.TempDir()
	const uuid = "srv-move-neoresolve"
	jar := filepath.Join(root, uuid, "main", "server.jar")
	writeTestFile(t, jar, "neoforge")
	dm, stops := runningDocker(t, uuid, http.StatusNoContent)
	rdb := newMiniRedis(t)
	payload, _ := json.Marshal(map[string]interface{}{"config": map[string]interface{}{
		"uuid": uuid, "activeSubServer": "main", "targetDir": "mods",
	}})
	runUpdateServerVersion(context.Background(), rdb, dm, &StorageManager{paths: []string{root}},
		NodeCommand{Installer: InstallerConfig{Type: "neoforge", Version: "1.20.1"}}, string(payload))

	if got, _ := rdb.Get(context.Background(), "dylaris:server:"+uuid+":version_update_failed").Result(); got == "" {
		t.Error("Core was not told the move did not happen")
	}
	if stops.Load() != 0 {
		t.Error("the server was stopped before its NeoForge build was known")
	}
	if readTestFile(jar) != "neoforge" {
		t.Error("the server was changed for a move whose build could not be resolved")
	}
}

// L18: a version move that takes the server while the mod downloads makes the
// install give up before the rename, so no old-version jar lands beside the
// move's.
func TestInstallModGivesUpWhenAMoveStartsDuringTheDownload(t *testing.T) {
	sm, uuid, modsDir := installFixture(t)
	rdb := newMiniRedis(t)
	orig := downloadModFile
	downloadModFile = func(_, dest, _ string) error {
		rdb.Set(context.Background(), nodeBusyKey(uuid), "installing", time.Minute)
		return os.WriteFile(dest, []byte("new jar bytes"), 0o644)
	}
	t.Cleanup(func() { downloadModFile = orig })

	runInstallMod(context.Background(), rdb, sm, replacePayload(uuid, ""))

	for _, name := range []string{"spark-1.1.jar", "spark-1.1.jar.part"} {
		if _, err := os.Lstat(filepath.Join(modsDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is in mods/ after the server was taken by a move: %v", name, err)
		}
	}
}

// L19: a removal that arrives while the server is held is carried out once the
// hold ends. It used to be dropped, after Core had already deleted the row, so
// the jar kept loading with nothing in the panel left to remove it with.
func TestRemoveModWaitsOutTheHold(t *testing.T) {
	prevPoll := modsHeldPoll
	modsHeldPoll = 10 * time.Millisecond
	t.Cleanup(func() { modsHeldPoll = prevPoll })

	sm, uuid, modsDir := installFixture(t)
	rdb := newMiniRedis(t)
	ctx := context.Background()
	rdb.Set(ctx, nodeBusyKey(uuid), "installing", time.Minute)
	jar := filepath.Join(modsDir, "spark-1.0.jar")

	runRemoveMod(ctx, rdb, sm, mustModPayload(t, removeModPayload{
		UUID: uuid, ActiveSubServer: "survival", TargetDir: "mods", FileName: "spark-1.0.jar",
	}))
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(jar); err != nil {
		t.Fatalf("the jar was removed while the server was held: %v", err)
	}

	rdb.Del(ctx, nodeBusyKey(uuid))
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(jar); errors.Is(err, os.ErrNotExist) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the removal was dropped: the jar is still there after the hold ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
