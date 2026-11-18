package main

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dylaris-pkg/queue"
)

// Z09: a successful restore booked the extracted tree as landed and deleted
// the old copy beside it without crediting that space back, so every write
// budget still running counted the old world as used until it ended. What it
// books is now net of the old copy, never below zero: a restore that replaced
// a bigger world than it wrote books nothing, one onto an empty root books
// all it wrote.
func TestASuccessfulRestoreCreditsTheOldCopyItDeletes(t *testing.T) {
	tests := []struct {
		name     string
		oldBytes int // size of the old world; 0 removes the server root
		wantZero bool
	}{
		{"old world bigger than the restore", 1 << 20, true},
		{"no server root before the restore", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, cmd := restoreArchive(t, []tar.Header{
				{Name: "world/r.0.0.mca", Mode: 0o644, Typeflag: tar.TypeReg},
			}, map[string]string{"world/r.0.0.mca": "region bytes"})
			serverDir := filepath.Join(root, cmd.ServerUUID)
			if tc.oldBytes == 0 {
				if err := os.RemoveAll(serverDir); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(filepath.Join(serverDir, "old.mca"), []byte(strings.Repeat("x", tc.oldBytes)), 0o644); err != nil {
				t.Fatal(err)
			}
			rdb := newMiniRedis(t)
			l0 := writesOn("").landed.Load()
			report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
				RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
			})
			if report["status"] != "success" {
				t.Fatalf("report = %v", report)
			}
			got := writesOn("").landed.Load() - l0
			if tc.wantZero && got != 0 {
				t.Errorf("%d bytes booked as landed after deleting a bigger old copy, want 0", got)
			}
			if !tc.wantZero && got <= 0 {
				t.Errorf("%d bytes booked as landed for a restore onto an empty root, want what it wrote", got)
			}
		})
	}
}
