package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func writeSavedConfig(t *testing.T, dir, uuid string, hostPort int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := ServerConfig{UUID: uuid}
	cfg.Docker.HostPort = hostPort
	cfg.Docker.Timezone = "Europe/Berlin"
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, uuid, ".node_config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readSavedConfig(t *testing.T, dir, uuid string) ServerConfig {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, uuid, ".node_config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg ServerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The failure this pins (N1, re-test of the installer on two VMs): a move from
// Compose to Swarm removed the game containers AND restarted Valkey, so the
// ledger was gone in both places, and the reconciler handed out 25600/25601 in
// the order the servers came up - the two servers swapped addresses.
func TestRestorePortLedger_KeepsEachServersPortAfterRedisAndContainersAreGone(t *testing.T) {
	dir := t.TempDir()
	writeSavedConfig(t, dir, "server-a", 25600)
	writeSavedConfig(t, dir, "server-b", 25601)
	writeSavedConfig(t, dir, "legacy-c", 0) // saved before configs carried a port

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	pm := NewPortManager(rdb, "node-1", 25600, 25699, seqMode)
	dm := &DockerManager{storageMgr: NewStorageManager(dir, nil), portMgr: pm}

	// No containers survived: nothing to adopt from Docker.
	pm.AdoptExistingBindings(map[string]int{})
	dm.restorePortLedger()

	// server-b comes up first, as it did on the VM. It must not take 25600.
	if got := pm.GetPort("server-b"); got != 25601 {
		t.Fatalf("server-b holds %d, want its old 25601", got)
	}
	if got := pm.GetPort("server-a"); got != 25600 {
		t.Fatalf("server-a holds %d, want its old 25600", got)
	}
	got, err := pm.AllocatePort("legacy-c")
	if err != nil {
		t.Fatal(err)
	}
	if got == 25600 || got == 25601 {
		t.Fatalf("legacy-c was handed %d, a port another server had before", got)
	}
}

func TestRestorePortLedger_BackfillsTheSavedConfigFromTheLedger(t *testing.T) {
	dir := t.TempDir()
	writeSavedConfig(t, dir, "server-a", 0)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	pm := NewPortManager(rdb, "node-1", 25600, 25699, seqMode)
	dm := &DockerManager{storageMgr: NewStorageManager(dir, nil), portMgr: pm}

	// The running container still holds 25605; its config does not say so.
	pm.AdoptExistingBindings(map[string]int{"server-a": 25605})
	dm.restorePortLedger()

	cfg := readSavedConfig(t, dir, "server-a")
	if cfg.Docker.HostPort != 25605 {
		t.Fatalf("saved hostPort = %d, want 25605", cfg.Docker.HostPort)
	}
	if cfg.Docker.Timezone != "Europe/Berlin" {
		t.Fatalf("other docker fields lost: timezone %q", cfg.Docker.Timezone)
	}
}

func TestRestorePortLedger_ARunningContainerWinsOverAStaleSavedPort(t *testing.T) {
	dir := t.TempDir()
	writeSavedConfig(t, dir, "server-a", 25600) // stale: a holds 25610 now
	writeSavedConfig(t, dir, "server-b", 25600)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	pm := NewPortManager(rdb, "node-1", 25600, 25699, seqMode)
	dm := &DockerManager{storageMgr: NewStorageManager(dir, nil), portMgr: pm}

	pm.AdoptExistingBindings(map[string]int{"server-a": 25610, "server-b": 25600})
	dm.restorePortLedger()

	if got := pm.GetPort("server-a"); got != 25610 {
		t.Fatalf("server-a holds %d, want its container's 25610", got)
	}
	if got := readSavedConfig(t, dir, "server-a").Docker.HostPort; got != 25610 {
		t.Fatalf("server-a saved hostPort = %d, want it corrected to 25610", got)
	}
	if got := pm.GetPort("server-b"); got != 25600 {
		t.Fatalf("server-b holds %d, want 25600", got)
	}
}

func TestSaveNodeConfig_StoresThePortTheNodePicked(t *testing.T) {
	dir := t.TempDir()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	pm := NewPortManager(rdb, "node-1", 25600, 25699, seqMode)
	if _, err := pm.AllocatePort("server-a"); err != nil {
		t.Fatal(err)
	}
	prev := dockerManager
	dockerManager = &DockerManager{portMgr: pm}
	t.Cleanup(func() { dockerManager = prev })

	if err := os.MkdirAll(filepath.Join(dir, "server-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	saveNodeConfig(filepath.Join(dir, "server-a"), ServerConfig{UUID: "server-a"}) // Core sent hostPort 0
	if got := readSavedConfig(t, dir, "server-a").Docker.HostPort; got != 25600 {
		t.Fatalf("saved hostPort = %d, want the allocated 25600", got)
	}

	explicit := ServerConfig{UUID: "server-a"}
	explicit.Docker.HostPort = 25650
	saveNodeConfig(filepath.Join(dir, "server-a"), explicit)
	if got := readSavedConfig(t, dir, "server-a").Docker.HostPort; got != 25650 {
		t.Fatalf("saved hostPort = %d, want Core's explicit 25650", got)
	}
}
