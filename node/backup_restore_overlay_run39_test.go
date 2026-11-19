package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"dylaris-pkg/queue"
)

// A selective backup holds only what its patterns matched. Restored wholesale it
// replaced the server with that part and deleted everything else - the jar, the
// plugins, every config - with no copy kept (R39-03). An overlay restore lets
// the archive overwrite only what it contains.
func TestRestoreOverlayKeepsWhatTheArchiveDoesNotHold(t *testing.T) {
	tests := []struct {
		name      string
		subServer string
		overlay   bool
		wantKept  bool
	}{
		{"whole server, overlay", "", true, true},
		{"whole server, full archive", "", false, false},
		{"sub-server, overlay", "survival", true, true},
		{"sub-server, full archive", "survival", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, cmd := restoreArchive(t, []tar.Header{
				{Name: "world/r.0.0.mca", Mode: 0o644, Typeflag: tar.TypeReg},
				{Name: "plugins/x.yml", Mode: 0o644, Typeflag: tar.TypeReg},
			}, map[string]string{"world/r.0.0.mca": "restored region", "plugins/x.yml": "restored"})
			cmd.SubServer, cmd.Overlay = tt.subServer, tt.overlay
			target := filepath.Join(root, cmd.ServerUUID, tt.subServer)
			live := map[string]string{
				"server.jar":      "jar",
				"plugins/x.yml":   "live",
				"plugins/Big.jar": "plugin jar",
				"world/old.mca":   "old region",
				"world/r.0.0.mca": "live region",
				"config/a/b.toml": "deep config",
			}
			for n, body := range live {
				p := filepath.Join(target, filepath.FromSlash(n))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			rdb := newMiniRedis(t)
			report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
				RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
			})
			if report["status"] != "success" {
				t.Fatalf("report = %v", report)
			}
			read := func(n string) (string, bool) {
				b, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(n)))
				return string(b), err == nil
			}
			// What the archive holds wins either way.
			for n, want := range map[string]string{"world/r.0.0.mca": "restored region", "plugins/x.yml": "restored"} {
				if got, _ := read(n); got != want {
					t.Errorf("%s = %q, want %q", n, got, want)
				}
			}
			for _, n := range []string{"server.jar", "plugins/Big.jar", "world/old.mca", "config/a/b.toml"} {
				got, ok := read(n)
				if ok != tt.wantKept {
					t.Errorf("%s present = %v, want %v", n, ok, tt.wantKept)
				}
				if ok && got != live[n] {
					t.Errorf("%s = %q, want the live %q", n, got, live[n])
				}
			}
		})
	}
}

// Core sends an overlay restore only to a node that advertises it; without the
// field it refuses the restore of a selective run instead.
func TestTheHeartbeatAdvertisesOverlayRestores(t *testing.T) {
	rdb := newMiniRedis(t)
	const id = "node-overlay-test"
	sendHeartbeat(context.Background(), rdb, id, "dev", "eu-central", nil, nil)
	raw, err := rdb.Get(context.Background(), "dylaris:discovery:"+id).Result()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if got["restoreOverlay"] != true {
		t.Fatalf("restoreOverlay = %v, want true", got["restoreOverlay"])
	}
}
