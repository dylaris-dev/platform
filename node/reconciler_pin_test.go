package main

import (
	"os"
	"strings"
	"testing"
)

// The saved config sits in a directory the node writes for the tenant: a
// copy through a planted link once overwrote it. Its identity is taken from
// where it was found, never from what it says.
func TestPinSavedConfig(t *testing.T) {
	const dirUUID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	for _, c := range []struct {
		sub string
		ok  bool
	}{
		{"survival", true},
		{"Carlo__Ben", true},
		{"legacy name with spaces", true},
		{"../bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb/survival", false},
		{"survival/../../x", false},
		{`..\x`, false},
		{"..", false},
		{".", false},
	} {
		cfg := ServerConfig{UUID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", ActiveSubServer: c.sub}
		if got := pinSavedConfig(&cfg, dirUUID); got != c.ok {
			t.Errorf("sub-server %q: ok = %v, want %v", c.sub, got, c.ok)
		}
		if cfg.UUID != dirUUID {
			t.Errorf("sub-server %q: the config kept the uuid it named (%s)", c.sub, cfg.UUID)
		}
	}
}

// The deleted-container pass is the reader; it must go through the pin.
func TestTheDeletedPassPinsTheConfig(t *testing.T) {
	raw, err := os.ReadFile("reconciler.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := cutFunc(string(raw), "func reconcileDeletedContainers(ctx context.Context, rdb *redis.Client, dm *DockerManager, storage *StorageManager) {")
	if !ok {
		t.Fatal("reconcileDeletedContainers is gone; move this assertion with it")
	}
	pin := strings.Index(body, "pinSavedConfig(&config, uuid)")
	use := strings.Index(body, "dm.RecreateWithCommand(config)")
	if pin < 0 || use < 0 || pin > use {
		t.Error("the saved config reaches RecreateWithCommand without being pinned to its directory")
	}
}
