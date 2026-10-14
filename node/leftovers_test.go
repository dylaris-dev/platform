package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// What a job killed by a node restart leaves is removed at boot - only these
// names, only once old, and nothing else in the tenant's tree.
func TestSweepInterruptedWork(t *testing.T) {
	base := t.TempDir()
	srv := filepath.Join(base, "srv-1")
	old := time.Now().Add(-7 * time.Hour)
	mk := func(rel string, dir, aged bool) string {
		p := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if dir {
			if err := os.MkdirAll(filepath.Join(p, "inner"), 0o755); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if aged {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	gone := []string{
		mk("srv-1/.technic-stage-123", true, true),
		mk("srv-1/survival/.technic-dl-9", true, true),
		mk("srv-1/.creative.copying", true, true),
		mk("srv-1/survival/mods/.upload-77.tmp", false, true),
		mk("srv-1/.dylaris-backups/job-1.tar.gz.partial", false, true),
		mk("srv-2.migration-in.zip", false, true),
	}
	kept := []string{
		mk("srv-1/.technic-stage-new", true, false),                       // a job since boot
		mk("srv-1/survival/world/region/deep/.upload-1.tmp", false, true), // deeper than uploads land
		mk("srv-1/survival/world/.technic-stage-x", true, true),           // not where installs stage
		mk("srv-1/.dylaris-backups/job-0.tar.gz", false, true),            // a finished archive
		mk("srv-1/survival/server.properties", false, true),
		mk("srv-1/survival/.copying", true, true), // not at the server level
	}
	sweepInterruptedWork([]string{base})
	for _, p := range gone {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("left behind: %s", p)
		}
	}
	for _, p := range kept {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("removed what is not a leftover: %s", p)
		}
	}
	if _, err := os.Stat(srv); err != nil {
		t.Fatal("the server directory itself went")
	}
}

// A node-local archive is written under .partial and renamed when complete,
// and the list Core reads never names a .partial: listed, the orphan scan
// would see a key no run names and delete it while it is written.
func TestPartialArchivesAreNeitherFinalNorListed(t *testing.T) {
	read := func(f string) string {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	w := read("backup_worker.go")
	for _, want := range []string{"partial := full + partialBackupSuffix", "os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL", "os.Rename(partial, full)"} {
		if !strings.Contains(w, want) {
			t.Errorf("backup_worker.go no longer has %q", want)
		}
	}
	if !strings.Contains(read("grpc_backup.go"), "if strings.HasSuffix(e.Name(), partialBackupSuffix) {\n\t\t\tcontinue") {
		t.Error("the backup list names archives still being written")
	}
}

// Storage another node also uses is left alone: one of its jobs can run for
// longer than any age this sweep could pick.
func TestSweepSkipsStorageAnotherNodeUses(t *testing.T) {
	base := t.TempDir()
	p := filepath.Join(base, "srv-1", ".technic-stage-1")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-7 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if err := writeBeacon(base, nodeBeacon{NodeID: "other-node", UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	sweepInterruptedWork([]string{base})
	if _, err := os.Stat(p); err != nil {
		t.Fatal("swept a job on storage another node uses")
	}
}
