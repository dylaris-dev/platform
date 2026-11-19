package main

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// preRestoreTTL is how long we keep the "moved aside" sub-server contents
// after a successful restore. Long enough that an admin can intervene if
// the restore caused damage, short enough that disk doesn't fill up.
const preRestoreTTL = 24 * time.Hour

// pendingDeleteTTL is the grace window for .pending-delete-* tombstones
// produced by delete_sub_server. The inline async retry already tries
// for ~78s; this catches the rare case where every retry hit busy-FS.
// 10 minutes is plenty -- the tombstone is hidden from the user the
// whole time, this is just disk-hygiene.
const pendingDeleteTTL = 10 * time.Minute

// restoreCleanupInterval is how often we walk the storage paths looking
// for stale .pre-restore-* directories.
const restoreCleanupInterval = time.Hour

// ownStages holds the base names of the staging dirs of the restores this
// process is running now (createStageDir adds one, releaseStageDir drops it).
// Those are never swept: a stage's mtime moves only when its top level gains an
// entry, and restoreDeadline lets a big archive extract for a whole day, so an
// age said nothing about a running one. No restore survives a restart, so any
// other one is dead whatever its age. Names, not mtimes: a clock stepped back at
// boot would make a fresh stage look old.
var ownStages sync.Map

// preRestoreStash matches the names RunRestore gives a stash:
// "<target>.pre-restore-<YYYYMMDD-HHMMSS>". The sub-server name comes first,
// so this cannot be a prefix check the way ".pending-delete-" is - but a bare
// strings.Contains was too loose in the other direction. The substring is
// reserved nowhere on the way in, so a directory called
// "notes.pre-restore-old" is something a user may legitimately create, and
// this walk would have deleted it 24 hours later without a word.
var preRestoreStash = regexp.MustCompile(`\.pre-restore-\d{8}-\d{6}$`)

// stageDirInfix is what RunRestore hands os.MkdirTemp as the fixed part of the
// staging directory's name. It lives here rather than in backup_restore.go so
// the thing that CREATES the name and the thing that has to recognise it again
// cannot drift apart.
const stageDirInfix = ".restore-"

// stageDirSuffix is appended AFTER MkdirTemp's random run of digits, and it is
// what makes the name recognisable without counting them.
//
// The matcher used to require six or more digits, on the reasoning that
// MkdirTemp's suffix is "a long run". It is not: the random is a uint32
// printed in decimal, so it is anywhere from one to ten digits, and a run that
// came out short produced a staging dir the sweep did not recognise. CI caught
// exactly that with "survival.restore-92106". The consequence is the one this
// sweep exists to prevent: a full copy of the world left inside the server
// directory for good, counting against the server's disk limit.
//
// The width requirement was there for a real reason - a user may plausibly
// create "world.restore-2" by hand over SFTP, and sweeping it 24 hours later
// without a word is data loss. So the fix is to make the PRODUCED name
// unmistakable rather than to loosen the match: this suffix is narrower than
// the old rule, not wider.
const stageDirSuffix = ".stage"

// restoreStage matches a staging directory left behind by RunRestore:
// "<target>.restore-<random>.stage".
//
// This is the second directory a restore creates, and until it was added only
// the first was swept. A restore killed mid-extraction (SIGKILL, node redeploy,
// OOM) left its whole staging tree behind for good, with nothing that would
// ever remove it.
var restoreStage = regexp.MustCompile(`\` + stageDirInfix + `\d+\` + stageDirSuffix + `$`)

// StartRestoreCleanup runs a background goroutine that periodically deletes
// .pre-restore-<timestamp> directories left behind by RunRestore once they
// are older than preRestoreTTL. The async cleanup in RunRestore itself
// covers the happy path; this is the safety net for crash / SIGKILL.
func StartRestoreCleanup(ctx context.Context, sm *StorageManager) {
	go func() {
		// Run once shortly after boot so a clean restart immediately picks
		// up leftover stashes from the previous process.
		time.Sleep(30 * time.Second)
		runRestoreCleanup(sm)
		// Boot only: what it removes can only be left by the previous process.
		sweepInterruptedWork(storageRoots(sm))

		ticker := time.NewTicker(restoreCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runRestoreCleanup(sm)
			}
		}
	}()
}

func runRestoreCleanup(sm *StorageManager) {
	roots := storageRoots(sm)
	now := time.Now()
	preRestoreCutoff := now.Add(-preRestoreTTL)
	pendingCutoff := now.Add(-pendingDeleteTTL)
	purgedRestore, purgedPending := 0, 0
	for _, root := range roots {
		// root is e.g. ./dylaris_data/servers; one level down are
		// per-server UUID directories. The stash sits one more level
		// deeper, next to the sub-server we restored into.
		filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if !info.IsDir() {
				return nil
			}
			base := filepath.Base(path)
			// Both stashes are created at depth 1 (a whole-server restore, next
			// to the server dir) or depth 2 (a sub-server restore or a
			// delete_sub_server tombstone). Anything matching deeper than that
			// is not ours: the walk still VISITS depth 3, and a user can put a
			// directory there through the file browser, SFTP or a zip upload.
			// Neither name is reserved on the way in - only ".pending-delete-"
			// is, and only against the file API, not zip extraction.
			shallow := depthBelow(root, path) <= 2
			isRestore := shallow && (preRestoreStash.MatchString(base) || restoreStage.MatchString(base))
			isPending := shallow && strings.HasPrefix(base, ".pending-delete-")
			if !isRestore && !isPending {
				// Avoid descending into world-data unnecessarily; we only
				// care about top two levels (server / sub-server).
				if depthBelow(root, path) > 2 {
					return filepath.SkipDir
				}
				return nil
			}
			cutoff := preRestoreCutoff
			label := "pre-restore stash"
			switch {
			case isPending:
				cutoff = pendingCutoff
				label = "pending-delete tombstone"
			case restoreStage.MatchString(base):
				// One of a restore running here is never swept (see ownStages).
				// Any other is dead already, and held the space its redelivered
				// restore needs; on storage another node uses, it may be that
				// node's running restore, so it keeps the stash's 24h TTL.
				label = "restore staging dir"
				if _, ours := ownStages.Load(base); ours {
					return filepath.SkipDir
				}
				if !sharedWithAnotherNode(root) {
					cutoff = now
				}
			}
			finished := false
			age := info.ModTime()
			if m := preRestoreStash.FindString(base); m != "" {
				// The stash is the renamed directory and keeps its mtime, days old
				// on an idle server; only the name says when it was made.
				if at, err := time.Parse("20060102-150405", strings.TrimPrefix(m, ".pre-restore-")); err == nil && at.After(age) {
					age = at
				}
			}
			if m := preRestoreStash.FindString(base); m != "" {
				// A node killed between the swap's two renames leaves the stash
				// (the original world and, for a whole server, its node-local
				// archives) next to the restore's stage, with nothing at the
				// target's name. Put the stash back, so the redelivered restore
				// finds the server and its archive again; never delete it on the
				// TTL. Without a stage the target was deleted after the swap, and
				// the stash keeps its normal TTL rather than bring that back.
				target := strings.TrimSuffix(path, m)
				_, terr := os.Lstat(target)
				if errors.Is(terr, os.ErrNotExist) && hasStageOf(target) {
					rel, _ := filepath.Rel(root, target)
					uuid := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
					if !serverDeleted(uuid) {
						// Not while a restore of the server runs here (it may be
						// inside that window right now), nor on storage another
						// node uses.
						if !restoreServersInFlight.has(uuid) && !sharedWithAnotherNode(root) {
							if err := os.Rename(path, target); err == nil {
								log.Printf("restore-cleanup: %s was missing after an interrupted restore, moved its pre-restore stash back", target)
								return filepath.SkipDir
							}
						}
						if age.Before(cutoff) {
							keepStash(0, path)
							return filepath.SkipDir
						}
					}
				} else if terr == nil && depthBelow(root, path) == 1 && !serverDeleted(filepath.Base(target)) {
					// A node killed after the second rename but before the
					// node's own root entries were carried across leaves the
					// restored root in place and the live archives and node
					// config only here. Carry what the root lacks; whatever
					// still cannot go is never deleted on the TTL.
					ours := !restoreServersInFlight.has(filepath.Base(target)) && !sharedWithAnotherNode(root)
					if ours {
						// The stash is named at the swap. A node file in the root
						// written before it is the archive's, which the tenant may
						// have written, and gives way to the live one, as
						// carryArchivesAcrossSwap does; one written after it is the
						// node's own and stays. A name that does not parse replaces
						// nothing.
						swapAt, perr := time.Parse("20060102-150405", strings.TrimPrefix(m, ".pre-restore-"))
						for _, name := range nodeOwnedRootEntries {
							src, dst := filepath.Join(path, name), filepath.Join(target, name)
							st, serr := os.Lstat(src)
							dt, derr := os.Lstat(dst)
							if perr == nil && serr == nil && derr == nil && st.Mode().IsRegular() && dt.Mode().IsRegular() && dt.ModTime().Before(swapAt.Add(time.Second)) {
								if err := os.Rename(src, dst); err == nil {
									log.Printf("restore-cleanup: put the live %s back over the archive's after an interrupted restore", dst)
								}
								continue
							}
							moveMissing(src, dst)
						}
					}
					if slices.ContainsFunc(nodeOwnedRootEntries, func(name string) bool { return holdsFile(filepath.Join(path, name)) }) {
						if age.Before(cutoff) {
							keepStash(0, path)
							return filepath.SkipDir
						}
					} else if ours {
						// Swapped and carried: the old world the restore was deleting
						// when the node died. It keeps the server's quota project, so
						// waiting out the TTL counted it against the server for a day.
						finished = true
					}
				}
			}
			if finished || age.Before(cutoff) {
				log.Printf("restore-cleanup: removing %s %s (age %v)", label, path, now.Sub(age).Round(time.Minute))
				if err := os.RemoveAll(path); err == nil {
					if isRestore {
						purgedRestore++
					} else {
						purgedPending++
					}
				}
			}
			return filepath.SkipDir // never recurse into the stash itself
		})
	}
	if purgedRestore > 0 || purgedPending > 0 {
		log.Printf("restore-cleanup: removed %d stash + %d pending-delete dir(s)", purgedRestore, purgedPending)
	}
}

// storageRoots returns every server-root path the StorageManager knows
// about. Falls back to the legacy default for the no-storage-manager case
// so tests / local dev still work.
func storageRoots(sm *StorageManager) []string {
	if sm == nil {
		return []string{"./dylaris_data/servers"}
	}
	roots := sm.Paths()
	if len(roots) == 0 {
		roots = []string{"./dylaris_data/servers"}
	}
	return roots
}

// depthBelow returns how many directory hops separate `path` from `root`.
// Used so the walker bails out once it descends past the sub-server level.
func depthBelow(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}
	return strings.Count(filepath.ToSlash(rel), "/") + 1
}

// moveMissing moves src to dst, or, where both are directories, every entry
// of src that dst lacks. Nothing dst already has is replaced: it may be newer
// than the stash, such as a backup taken after the interrupted restore.
func moveMissing(src, dst string) {
	st, err := os.Lstat(src)
	if err != nil {
		return
	}
	dt, err := os.Lstat(dst)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(src, dst); err == nil {
			log.Printf("restore-cleanup: carried %s into %s after an interrupted restore", src, dst)
		}
		return
	}
	if err != nil || !st.IsDir() || !dt.IsDir() {
		return
	}
	entries, _ := os.ReadDir(src)
	for _, e := range entries {
		moveMissing(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
	}
}

// holdsFile reports whether p is, or holds, anything but directories. An
// unreadable tree counts as holding something.
func holdsFile(p string) bool {
	found := false
	err := filepath.WalkDir(p, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found || (err != nil && !errors.Is(err, os.ErrNotExist))
}

// hasStageOf reports whether a restore staging dir of target sits next to it.
func hasStageOf(target string) bool {
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		return false
	}
	prefix := filepath.Base(target) + stageDirInfix
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), prefix) && restoreStage.MatchString(e.Name()) {
			return true
		}
	}
	return false
}

// releaseStageDir ends dir's place in ownStages, once the restore that made it
// has returned. Whatever of the stage is still on disk then is dead.
func releaseStageDir(dir string) { ownStages.Delete(filepath.Base(dir)) }
