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
		mk("srv-1/survival/world/datapacks/pack/.upload-1.tmp", false, true), // P06: uploads land at any depth
		mk("srv-1/.dylaris-backups/job-1.tar.gz.partial", false, true),
		mk("srv-2.migration-in.zip", false, true),
		mk(".migration-pull-123", false, true),
	}
	kept := []string{
		mk("srv-1/.technic-stage-new", true, false),             // a job since boot
		mk(".migration-pull-456", false, false),                 // a download since boot
		mk("srv-1/survival/world/.technic-stage-x", true, true), // not where installs stage
		mk("srv-1/.dylaris-backups/job-0.tar.gz", false, true),  // a finished archive
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
	for _, want := range []string{"partial := full + partialBackupSuffix", "root.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL", "root.Rename(partial, full)"} {
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

// R37-09: the sweep walks by directory handle and stops at a depth cap, so a
// tenant's endlessly nested tree cannot make every boot crawl or crash. What
// the walk reaches is still judged by its parent's name.
func TestSweepServerDirStopsAtDepthCap(t *testing.T) {
	prev := maxLeftoverSweepDepth
	maxLeftoverSweepDepth = 3
	t.Cleanup(func() { maxLeftoverSweepDepth = prev })
	srv := filepath.Join(t.TempDir(), "srv-1")
	old := time.Now().Add(-7 * time.Hour)
	cases := []struct {
		rel  string
		gone bool
	}{
		{"a/a/.upload-1.tmp", true},                     // depth 3: within the cap
		{"a/.dylaris-backups/job.tar.gz.partial", true}, // parent name still known
		{"a/a/a/.upload-2.tmp", false},                  // depth 4: not entered
	}
	for _, tc := range cases {
		p := filepath.Join(srv, filepath.FromSlash(tc.rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	sweepServerDir(srv, time.Now().Add(-leftoverMinAge))
	for _, tc := range cases {
		_, err := os.Lstat(filepath.Join(srv, filepath.FromSlash(tc.rel)))
		if gone := os.IsNotExist(err); gone != tc.gone {
			t.Errorf("%s: gone=%v, want %v", tc.rel, gone, tc.gone)
		}
	}
}
