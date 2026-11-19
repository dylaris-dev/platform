package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// R26-01: deleting the ACTIVE sub-server re-pointed .active_server at another
// one, while Core records active "" and pending_setup and never adopts that
// pick. A later setup that failed then read the file, reported "stopped" for
// a server Core holds unset-up, and the panel offered Start on nothing.
func TestDeletingTheActiveSubServerLeavesNoActiveSubServer(t *testing.T) {
	const uuid = "26262626-0101-0101-0101-262626262626"
	for _, tc := range []struct {
		name, deleted string
		wantActive    string // "" means no .active_server
	}{
		{"the active one, another left", "a", ""},
		{"an inactive one", "b", "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, uuid)
			for _, sub := range []string{"a", "b"} {
				if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, ".active_server"), []byte("a"), 0o644); err != nil {
				t.Fatal(err)
			}
			rdb := newMiniRedis(t)
			cmd := NodeCommand{Action: "delete_sub_server"}
			cmd.Config.UUID = uuid
			cmd.Config.ActiveSubServer = tc.deleted
			processCommand(context.Background(), cmd, "", rdb, dockerWithWorkingDir(t, ""), "node-a", nil, &StorageManager{paths: []string{root}})

			if got := readActiveSubServer(dir); got != tc.wantActive {
				t.Errorf(".active_server after deleting %q = %q, want %q", tc.deleted, got, tc.wantActive)
			}
			wantIdle := "stopped"
			if tc.wantActive == "" {
				wantIdle = "pending_setup"
			}
			if got := setupIdleStatus(dir); got != wantIdle {
				t.Errorf("a failed setup after deleting %q would report %q, want %q", tc.deleted, got, wantIdle)
			}
		})
	}
}
