package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// The staging directory sits inside the server root for a sub-server restore,
// and that root is bind-mounted into the tenant's own MC container at /data.
// A predictable name plus os.MkdirAll meant a planted symlink was adopted
// silently, the extraction wrote through it, and the swap renamed the link
// itself into place.
func TestCreateStageDirNeverAdoptsWhatIsAlreadyThere(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "survival")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	// An escape hatch planted under a name from the pattern the code uses.
	outside := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	planted := target + stageDirInfix + "000000000"
	mustSymlink(t, outside, planted)

	seen := map[string]bool{}
	for range 5 {
		dir, err := createStageDir(target)
		if err != nil {
			t.Fatalf("createStageDir: %v", err)
		}
		if dir == planted {
			t.Fatal("createStageDir returned the planted path")
		}
		if seen[dir] {
			t.Fatalf("createStageDir returned %s twice", dir)
		}
		seen[dir] = true

		// Lstat, not Stat: Stat would report the symlink's target as a
		// directory and call the adopted link a success.
		fi, err := os.Lstat(dir)
		if err != nil {
			t.Fatalf("lstat: %v", err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			t.Fatal("the staging path is a symlink")
		}
		if !fi.IsDir() {
			t.Fatal("the staging path is not a directory")
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("the staging dir is not empty: %v %v", entries, err)
		}
	}

	// Nothing was written through the link.
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("the symlink target was touched: %v %v", entries, err)
	}
}

func TestCreateStageDirIsReadableByTheContainer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod bits are not meaningful on Windows")
	}
	root := t.TempDir()
	target := filepath.Join(root, "survival")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := createStageDir(target)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	// MkdirTemp's own 0700 would leave the restored world unreadable to an MC
	// image that does not run as this process's user.
	if perm := fi.Mode().Perm(); perm != 0o755 {
		t.Errorf("staging dir mode = %o, want 755", perm)
	}
}

// The name is produced in backup_restore.go and recognised in
// restore_cleanup.go. Pin that those two still agree, and that neither of the
// two leftover kinds is mistaken for the other.
func TestTheCleanupRecognisesTheNameARestoreActuallyProduces(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "survival")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := createStageDir(target)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(dir)

	if !restoreStage.MatchString(base) {
		t.Errorf("the cleanup does not recognise %q as a staging dir", base)
	}
	if preRestoreStash.MatchString(base) {
		t.Errorf("%q is matched as a pre-restore stash", base)
	}
	const stash = "survival.pre-restore-20260821-120000"
	if restoreStage.MatchString(stash) {
		t.Errorf("%q is matched as a staging dir", stash)
	}
}

// A restore killed mid-extraction leaves a full copy of the world behind. Only
// the stash was ever swept, so the staging tree stayed inside the server
// directory for good, counting against that server's own disk limit.
func TestRestoreCleanupRemovesAbandonedStagingDirs(t *testing.T) {
	sm, _ := newPlacementManager(t, 1)
	root := sm.Paths()[0]
	const uuid = "srv-uuid"

	sub := filepath.Join(root, uuid, "survival")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	age := func(p string, at time.Time) string {
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := time.Now().Add(-48 * time.Hour)

	abandoned, err := createStageDir(sub)
	if err != nil {
		t.Fatal(err)
	}
	age(abandoned, old)

	// A restore that started a moment ago must not be swept out from under
	// itself.
	inFlight, err := createStageDir(sub)
	if err != nil {
		t.Fatal(err)
	}

	// A user directory that merely looks similar. The server directory is the
	// file browser's root, so this is something they can create.
	userDir := filepath.Join(root, uuid, "world.restore-2")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	age(userDir, old)

	runRestoreCleanup(sm)

	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Errorf("abandoned staging dir kept: %v", err)
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Errorf("swept an in-flight staging dir: %v", err)
	}
	if _, err := os.Stat(userDir); err != nil {
		t.Errorf("deleted a user directory that only looks like ours: %v", err)
	}
}

// The producer and the matcher have to agree for EVERY random MkdirTemp can
// pick, not just a typical one.
//
// The matcher used to require six or more digits because MkdirTemp's random was
// described as "a long run". It is a uint32 printed in decimal, so it is one to
// ten digits wide, and a short one produced a staging dir the sweep did not
// recognise - a full copy of a world left in the server directory for good.
// TestTheCleanupRecognisesTheNameARestoreActuallyProduces calls the real
// producer, so it only fails on the runs that happen to draw a short random:
// it took a 1-in-tens-of-thousands CI run to surface this. These cases pin the
// same property without waiting for the dice.
func TestTheStagingNameIsRecognisedAtEveryRandomWidth(t *testing.T) {
	for _, random := range []string{"0", "7", "92106", "999999", "1000000", "4294967295"} {
		name := "survival" + stageDirInfix + random + stageDirSuffix
		if !restoreStage.MatchString(name) {
			t.Errorf("the cleanup does not recognise %q, so a staging dir with that random is never swept", name)
		}
		if preRestoreStash.MatchString(name) {
			t.Errorf("%q is matched as a pre-restore stash", name)
		}
	}
}

// The width rule the suffix replaced was protecting something real: the server
// directory is writable by the tenant over SFTP, so a hand-made "world.restore-2"
// must not be swept 24 hours later without a word. The suffix has to be
// NARROWER than the rule it replaced, not wider.
func TestAHandMadeDirectoryIsNotMistakenForStaging(t *testing.T) {
	for _, name := range []string{
		"world.restore-2",
		"world.restore-20260827",
		"world.restore-",
		"world.stage",
		"world.restore-12.stage.bak",
		"world.restore-abc.stage",
	} {
		if restoreStage.MatchString(name) {
			t.Errorf("%q is matched as a staging dir and would be deleted", name)
		}
	}
}

// No restore survives a restart, so a stage older than this process is dead
// whatever its age. Kept for 24 hours, it shrank the disk budget of the very
// restore the queue redelivered after the restart.
func TestRestoreCleanupRemovesAStageFromBeforeThisProcess(t *testing.T) {
	sm, _ := newPlacementManager(t, 1)
	root := sm.Paths()[0]
	sub := filepath.Join(root, "srv-uuid", "survival")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	dead, err := createStageDir(sub)
	if err != nil {
		t.Fatal(err)
	}
	// As the previous process left it: minutes old, and not this process's.
	ownStages.Delete(filepath.Base(dead))
	at := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(dead, at, at); err != nil {
		t.Fatal(err)
	}
	live, err := createStageDir(sub)
	if err != nil {
		t.Fatal(err)
	}

	runRestoreCleanup(sm)

	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Errorf("stage of the previous process kept: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("swept a stage of this process: %v", err)
	}
}
