package handlers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Deleting a server or a backup schedule takes its backup archives out of
// reach: backup_jobs.server_id and backup_runs.job_id both cascade, and the run
// row is the only record of an archive's storage key. So every such delete has
// to purge first, and this test is here because there are FOUR of them - the
// single-server delete, the admin node force-delete, the customer's own machine
// delete and the schedule delete. Three of four with the purge is the same leak.
//
// It scans the whole package rather than the four files by name, so a fifth
// delete path added in a new file is caught rather than quietly joining the
// leak.
func TestEveryDeleteThatCascadesBackupsPurgesFirst(t *testing.T) {
	// call site -> the purge it must be preceded by.
	needs := map[string]string{
		"Store.DeleteServer(":        "purgeBackupArchivesForServers(",
		"Store.DeleteServersByNode(": "purgeBackupArchivesForServers(",
		"Store.DeleteBackupJob(":     "purgeBackupArchivesForJob(",
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	found := map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		for call, purge := range needs {
			at := strings.Index(src, call)
			if at < 0 {
				continue
			}
			found[call]++
			p := strings.Index(src, purge)
			if p < 0 || p > at {
				t.Errorf("%s calls %s without %s before it: the archives of what it deletes are left in the bucket with no row naming them", f, call, purge)
			}
		}
	}
	// A call site that disappears is as interesting as one that forgets: it
	// means the delete moved somewhere this test no longer looks.
	for call := range needs {
		if found[call] == 0 {
			t.Errorf("no call site of %s found in this package any more - has the delete moved? The purge has to move with it", call)
		}
	}
}
