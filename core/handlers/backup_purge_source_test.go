package handlers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dylaris-core/handlers/handlertest"
)

// Deleting a server or a backup schedule takes its backup archives out of
// reach: backup_jobs.server_id and backup_runs.job_id both cascade, and the run
// row is the only record of an archive's storage key. So every such delete has
// to purge first, and this test is here because there are FOUR of them - the
// single-server delete, the admin node force-delete, the customer's own machine
// delete and the schedule delete. Three of four with the purge is the same leak.
//
// It scans the whole package, and the handlers/<domain> subpackages split out
// of it, rather than the four files by name, so a fifth delete path added in a
// new file is caught rather than quietly joining the leak. A subpackage calls
// the purge by its exported name (handlers.PurgeBackupArchivesForJob).
func TestEveryDeleteThatCascadesBackupsPurgesFirst(t *testing.T) {
	// call site -> the purge it must be preceded by. The first letter is left
	// off so the package's purgeX( and a subpackage's handlers.PurgeX( match.
	needs := map[string]string{
		"Store.DeleteServer(":        "urgeBackupArchivesForServers(",
		"Store.DeleteServersByNode(": "urgeBackupArchivesForServers(",
		"Store.DeleteBackupJob(":     "urgeBackupArchivesForJob(",
	}

	// Every package under handlers/ at any depth, and services/: a delete
	// that moved behind a service still cascades.
	root := handlertest.HandlersRoot(t)
	files := handlertest.SourceFiles(t, root)
	files = append(files, handlertest.SourceFiles(t, filepath.Join(root, "..", "services"))...)
	found := map[string]int{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		for call, purge := range needs {
			// Every call site, each against its own enclosing func: a purge
			// earlier in the file, in another function, does not cover it.
			for off := 0; ; {
				i := strings.Index(src[off:], call)
				if i < 0 {
					break
				}
				at := off + i
				off = at + len(call)
				found[call]++
				fnStart := strings.LastIndex(src[:at], "\nfunc ")
				if fnStart < 0 || !strings.Contains(src[fnStart:at], purge) {
					t.Errorf("%s calls %s without %s before it: the archives of what it deletes are left in the bucket with no row naming them", f, call, purge)
				}
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
	// Floors, measured: losing SOME call sites (one of two moved where this
	// walk does not look) fails here instead of passing until the last one is
	// gone. A delete path really added or removed updates its number.
	floor := map[string]int{
		"Store.DeleteServer(":        2,
		"Store.DeleteServersByNode(": 2,
		"Store.DeleteBackupJob(":     1,
	}
	for call, want := range floor {
		if found[call] != want {
			t.Errorf("%d call sites of %s, the pin expects %d: if one moved out of reach the purge has to move with it; if one was really added or removed, update the floor", found[call], call, want)
		}
	}
}
