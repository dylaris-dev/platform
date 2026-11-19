package main

import (
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R34-08: a server root that does not resolve right now ("") must not turn
// into the relative .dylaris-backups in the node's working directory - not for
// the write, not for the redelivery check and not for the failure cleanup.
func TestNodeLocalBackupRefusesAnUnresolvedServerRoot(t *testing.T) {
	const key = "backups/x/job-1/run-1.tar.gz"
	info := storageInfo{Provider: "node-local", Config: json.RawMessage(`{}`)}
	// An id that is no server id resolves to "", like a root lost mid-run.
	const unresolved = ""

	tests := []struct {
		name  string
		stray bool // a stray archive already sits in the working directory
		run   func(sm *StorageManager) string
	}{
		{"upload", false, func(sm *StorageManager) string {
			if err := uploadBackup(t.Context(), sm, unresolved, info, key, strings.NewReader("archive")); err == nil {
				return "upload into an unresolved server root succeeded"
			}
			return ""
		}},
		{"complete check", true, func(sm *StorageManager) string {
			if _, ok := completeArchive(sm, unresolved, info, key); ok {
				return "a stray archive in the working directory was reported as this run's"
			}
			return ""
		}},
		{"restore download", true, func(sm *StorageManager) string {
			if rc, err := downloadBackup(t.Context(), sm, unresolved, info, key); err == nil {
				rc.Close()
				return "a restore read the working directory's archive"
			}
			return ""
		}},
		{"cleanup", true, func(sm *StorageManager) string {
			deleteBackup(t.Context(), sm, unresolved, info, key)
			return ""
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sm := NewStorageManager(t.TempDir(), nil)
			wd := t.TempDir()
			t.Chdir(wd)
			stray := filepath.Join(wd, backupDirName, nodeLocalArchiveName(key))
			if tc.stray {
				if err := os.MkdirAll(filepath.Dir(stray), 0o755); err != nil {
					t.Fatal(err)
				}
				f, err := os.Create(stray)
				if err != nil {
					t.Fatal(err)
				}
				gz := gzip.NewWriter(f)
				gz.Write([]byte("tar"))
				gz.Close()
				f.Close()
			}

			if msg := tc.run(sm); msg != "" {
				t.Error(msg)
			}

			_, err := os.Lstat(stray)
			if tc.stray && err != nil {
				t.Errorf("touched the working directory's archive: %v", err)
			}
			if !tc.stray {
				if _, err := os.Lstat(filepath.Join(wd, backupDirName)); !os.IsNotExist(err) {
					t.Errorf("created %s in the working directory: %v", backupDirName, err)
				}
			}
		})
	}
}
