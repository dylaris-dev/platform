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

// tar stops at its end-of-archive blocks and never reads the gzip trailer, so
// an archive whose bytes rotted at rest restored as "success" and the good
// world beside it was deleted.
func TestRestoreRefusesAnArchiveFailingItsChecksum(t *testing.T) {
	tests := []struct {
		name    string
		corrupt bool
		want    string
	}{
		{"intact", false, "success"},
		{"checksum mismatch", true, "failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, cmd := restoreArchive(t, []tar.Header{
				{Name: "world/r.0.0.mca", Mode: 0o644, Typeflag: tar.TypeReg},
			}, map[string]string{"world/r.0.0.mca": "region bytes"})
			if tt.corrupt {
				var si storageInfo
				var lc localCfg
				_ = json.Unmarshal(cmd.Storage, &si)
				_ = json.Unmarshal(si.Config, &lc)
				full := filepath.Join(lc.BasePath, filepath.FromSlash(cmd.StorageKey))
				b, err := os.ReadFile(full)
				if err != nil {
					t.Fatal(err)
				}
				b[len(b)-8] ^= 0xff // first byte of the CRC32 trailer
				if err := os.WriteFile(full, b, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			rdb := newMiniRedis(t)
			report := terminalReport(t, rdb, queue.BackupRestoresChannel(nodeID), func() {
				RunRestore(context.Background(), rdb, &StorageManager{paths: []string{root}}, nil, cmd)
			})
			if report["status"] != tt.want {
				t.Fatalf("report = %v, want %s", report, tt.want)
			}
			if tt.corrupt {
				if got, err := os.ReadFile(filepath.Join(root, cmd.ServerUUID, "keep.txt")); err != nil || string(got) != "before" {
					t.Fatalf("live world was replaced: keep.txt = %q, %v", got, err)
				}
			}
		})
	}
}
