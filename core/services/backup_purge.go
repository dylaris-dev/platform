package services

import (
	"context"
	"log"

	"dylaris-core/models"
	backupstorage "dylaris-core/storage/backup"
	"dylaris-core/store"
)

// openBackupStorage is the seam the purge is tested through. It is the real
// factory everywhere else.
var openBackupStorage = backupstorage.Open

// PurgeBackupArchives deletes the archives named by refs from the storage each
// one was written to.
//
// It exists because deleting a backup SCHEDULE or a SERVER used to delete rows
// only. backup_runs.job_id and backup_jobs.server_id both cascade, and the run
// row is the only record of an archive's storage key, so every such delete left
// objects behind that nothing could name again - we kept paying for them, and
// the customer's data stayed after they removed the thing that produced it.
// Deleting ONE backup already removed its object; these two paths never did.
//
// Called BEFORE the rows go, because afterwards the keys are unknowable.
//
// Best-effort by construction: the caller is about to complete a delete the user
// asked for, and an unreachable bucket must not turn that into a failure. A
// failure is logged with the key, which is the one thing that makes the leftover
// findable by hand.
//
// A storage the TENANT connected themselves is skipped, the same rule the
// billing retention pass follows: we pay nothing for it, and deleting from a
// bucket we do not own is not ours to do on their behalf. The trade is that such
// a bucket keeps archives nothing lists any more; it is the smaller harm of the
// two, and the log line says which keys.
//
// Returns how many objects were deleted and how many were left behind.
func PurgeBackupArchives(ctx context.Context, s backupStorageLookup, deps backupstorage.Deps, refs []store.BackupRunRef) (deleted, kept int) {
	for _, ref := range refs {
		if ref.StorageKey == "" {
			continue
		}
		// The owner is only for the "may this tenant use that storage" check on a
		// storage the run NAMES. A run that names none predates the column, and a
		// run that predates it went to the PLATFORM default - resolving it
		// through the tenant's chain would delete the key from a bucket they
		// connected later, where S3 answers a delete of a missing key with
		// success. We would report the archive gone and leave the real object
		// behind, which is the very outcome this purge exists to prevent.
		owner := ref.OwnerID
		if ref.StorageID == nil {
			owner = ""
		}
		bs, err := ResolveJobStorage(s, ref.StorageID, owner)
		if err != nil || bs == nil {
			kept++
			log.Printf("backup purge: run %d (%s): cannot resolve its storage: %v — the archive is left behind", ref.RunID, ref.StorageKey, err)
			continue
		}
		if !archiveIsOursToDelete(bs) {
			kept++
			log.Printf("backup purge: run %d (%s) lives on %q, which the tenant connected themselves — left in place", ref.RunID, ref.StorageKey, bs.Name)
			continue
		}
		provider, err := openBackupStorage(ctx, bs, deps)
		if err != nil {
			kept++
			log.Printf("backup purge: run %d (%s): cannot open storage %q: %v — the archive is left behind", ref.RunID, ref.StorageKey, bs.Name, err)
			continue
		}
		if err := provider.Delete(ctx, ref.StorageKey); err != nil {
			kept++
			log.Printf("backup purge: run %d (%s): delete failed: %v — the archive is left behind and nothing will name it again", ref.RunID, ref.StorageKey, err)
			continue
		}
		deleted++
	}
	if deleted > 0 || kept > 0 {
		log.Printf("backup purge: %d archive(s) deleted, %d left behind", deleted, kept)
	}
	return deleted, kept
}

// archiveIsOursToDelete is the one rule that decides whether an archive may be
// removed on the platform's initiative: a storage with an owner is a bucket the
// tenant connected, and emptying it frees us nothing.
func archiveIsOursToDelete(bs *models.BackupStorage) bool {
	return bs != nil && bs.OwnerID == nil
}
