package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func intPtr(n int) *int { return &n }

func TestRAMPaddingHelpers(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   *int
		want int
	}{
		{"not sent", nil, 512},
		{"none", intPtr(0), 0},
		{"set", intPtr(1536), 1536},
		{"negative", intPtr(-1), 512},
	} {
		if got := (DockerConfig{RAMPaddingMB: tc.in}).ramPadding(); got != tc.want {
			t.Errorf("ramPadding %s = %d, want %d", tc.name, got, tc.want)
		}
	}
	for _, tc := range []struct {
		labels map[string]string
		want   int
	}{
		{nil, 512}, // container from before the label
		{map[string]string{ramPaddingLabel: "0"}, 0},
		{map[string]string{ramPaddingLabel: "768"}, 768},
		{map[string]string{ramPaddingLabel: "x"}, 512},
	} {
		if got := ramPaddingFromLabels(tc.labels); got != tc.want {
			t.Errorf("ramPaddingFromLabels(%v) = %d, want %d", tc.labels, got, tc.want)
		}
	}
}

// A payload or .node_config.json from before the field decodes to nil, so it
// falls back to the label or 512 rather than to "no padding".
func TestRAMPaddingJSON(t *testing.T) {
	var old ServerConfig
	if err := json.Unmarshal([]byte(`{"uuid":"u","docker":{"ram":4096}}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Docker.RAMPaddingMB != nil {
		t.Errorf("old config decoded padding %d, want nil", *old.Docker.RAMPaddingMB)
	}
	var zero ServerConfig
	json.Unmarshal([]byte(`{"docker":{"ram":4096,"ramPaddingMB":0}}`), &zero)
	if zero.Docker.RAMPaddingMB == nil || *zero.Docker.RAMPaddingMB != 0 {
		t.Error("an explicit 0 padding did not survive decoding")
	}
}

// Without padding in the payload the container's own label is kept: a 1024
// padded container is not recreated for a change that touches nothing.
func TestUpdateResourcesKeepsTheLabelledPadding(t *testing.T) {
	const uuid = "41414141-2222-3333-4444-555555555555"
	dm, calls, _ := paddedContainerDocker(t, 1024, `{"dylaris.ram_padding_mb":"1024"}`)
	dm.portMgr.SetPort(uuid, 25601)
	cfg := ServerConfig{UUID: uuid}
	cfg.Docker.RAM, cfg.Docker.CPULimit = 4096, 1
	got, err := dm.UpdateResources(cfg)
	if err != nil {
		t.Fatalf("UpdateResources: %v", err)
	}
	if calls["stop"].Load() != 0 || calls["create"].Load() != 0 {
		t.Errorf("unchanged resources recreated a 1024-padded container (stop=%d create=%d)", calls["stop"].Load(), calls["create"].Load())
	}
	if got.Docker.RAMPaddingMB == nil || *got.Docker.RAMPaddingMB != 1024 {
		t.Errorf("applied config padding %v, want the label's 1024 saved", got.Docker.RAMPaddingMB)
	}
}

// A different padding in the payload recreates, and the new container carries
// the new limit and label.
func TestUpdateResourcesRecreatesForAPaddingChange(t *testing.T) {
	const uuid = "42424242-2222-3333-4444-555555555555"
	dm, calls, created := paddedContainerDocker(t, 512, "")
	dm.portMgr.SetPort(uuid, 25601)
	cfg := ServerConfig{UUID: uuid}
	cfg.Docker.RAM, cfg.Docker.CPULimit = 4096, 1
	cfg.Docker.RAMPaddingMB = intPtr(2048)
	dm.UpdateResources(cfg) // the recreate itself may fail against the fake
	if calls["stop"].Load() == 0 {
		t.Fatal("a padding change did not recreate the container")
	}
	assertCreated(t, created(), 4096+2048, "2048")
}

// A restart rebuilds the booking from the limit minus the label's padding and
// stamps the same padding again; a container without the label had 512.
func TestRestartContainerKeepsThePadding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		padding int
		labels  string
		label   string
	}{
		{"labelled", 1024, `{"dylaris.ram_padding_mb":"1024"}`, "1024"},
		{"labelled none", 0, `{"dylaris.ram_padding_mb":"0"}`, "0"},
		{"pre-label container", 512, "", "512"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dm, calls, created := paddedContainerDocker(t, tc.padding, tc.labels)
			dm.RestartContainer("43434343-2222-3333-4444-555555555555")
			if calls["create"].Load() == 0 {
				t.Fatal("restart never reached the create")
			}
			assertCreated(t, created(), 4096+tc.padding, tc.label)
		})
	}
}

func assertCreated(t *testing.T, body string, memMB int, label string) {
	t.Helper()
	var req struct {
		Labels     map[string]string
		HostConfig struct{ Memory, MemorySwap int64 }
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("create body %q: %v", body, err)
	}
	if want := int64(memMB) << 20; req.HostConfig.Memory != want || req.HostConfig.MemorySwap != want {
		t.Errorf("created with memory %d/%d MB, want %d", req.HostConfig.Memory>>20, req.HostConfig.MemorySwap>>20, memMB)
	}
	if req.Labels[ramPaddingLabel] != label {
		t.Errorf("created with padding label %q, want %q", req.Labels[ramPaddingLabel], label)
	}
}

// The saved config the next start builds from takes a sent padding and keeps
// its own when none is sent.
func TestMergeIntoSavedConfigPadding(t *testing.T) {
	const uuid = "44444444-2222-3333-4444-555555555555"
	data := t.TempDir()
	dm, _ := noContainerDocker(t, data)
	dir := filepath.Join(data, "servers", uuid)
	os.MkdirAll(dir, 0o755)
	saved := ServerConfig{UUID: uuid}
	saved.Docker.RAMPaddingMB = intPtr(1024)
	b, _ := json.Marshal(saved)
	os.WriteFile(filepath.Join(dir, ".node_config.json"), b, 0o644)

	change := ServerConfig{UUID: uuid}
	change.Docker.RAM = 2048
	if got := dm.mergeIntoSavedConfig(change); got.Docker.RAMPaddingMB == nil || *got.Docker.RAMPaddingMB != 1024 {
		t.Errorf("no padding sent: merged %v, want the saved 1024", got.Docker.RAMPaddingMB)
	}
	change.Docker.RAMPaddingMB = intPtr(0)
	if got := dm.mergeIntoSavedConfig(change); got.Docker.RAMPaddingMB == nil || *got.Docker.RAMPaddingMB != 0 {
		t.Errorf("padding 0 sent: merged %v, want 0", got.Docker.RAMPaddingMB)
	}
}

// Both container builders stamp the label from the config.
func TestBothCreatePathsStampThePadding(t *testing.T) {
	b, err := os.ReadFile("docker_mgr.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if n := strings.Count(s, "ramPaddingLabel: strconv.Itoa(config.Docker.ramPadding()),"); n != 2 {
		t.Errorf("padding label stamped at %d create sites, want 2 (CreateServerPodStopped, startMinecraftContainer)", n)
	}
	if n := strings.Count(s, "oomPadding := int64(config.Docker.ramPadding())"); n != 2 {
		t.Errorf("padding taken from the config at %d create sites, want 2", n)
	}
}
