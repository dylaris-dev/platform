package corestorage

import (
	"context"
	"log"
	"time"

	"dylaris-core/services"
	backupstorage "dylaris-core/storage/backup"
)

// BackupDeps assembles the backup-storage dependencies.
//
// One place, because a provider that needs one of them must not work on one
// path and be refused on another - the backup scheduler's own comment records
// two providers that were broken exactly that way by a second Deps literal.
func (s *Manager) BackupDeps() backupstorage.Deps {
	return backupstorage.Deps{
		Registry:    s.Registry,
		NodeStore:   s.Store,
		CoreStorage: s.BackupBuilder(),
		Connection:  s.ConnectionBackupBuilder(),
	}
}

// PurgeBackupArchivesForServers removes the archives of every backup schedule of
// these servers. Call it BEFORE the server rows are deleted: backup_jobs and
// backup_runs cascade with them, and the run row is the only record of where an
// archive lives.
//
// Best-effort, like every other post-delete cleanup: the user asked for the
// delete and it is about to succeed either way. What a failure buys is a log
// line naming the key, which is what makes a leftover findable at all.
func (s *Manager) PurgeBackupArchivesForServers(serverIDs []int) {
	if s == nil || s.Store == nil || len(serverIDs) == 0 {
		return
	}
	ctx, cancel := purgeContext()
	defer cancel()
	refs, err := s.Store.ListBackupRunRefsForServers(serverIDs)
	if err != nil {
		log.Printf("backup purge: listing the archives of %d server(s) failed, they are about to become unnameable: %v", len(serverIDs), err)
		return
	}
	services.PurgeBackupArchives(ctx, s.Store, s.BackupDeps(), refs)
}

// PurgeBackupArchivesForJob is the same for one schedule, whose runs cascade
// with it.
func (s *Manager) PurgeBackupArchivesForJob(jobID int) {
	if s == nil || s.Store == nil {
		return
	}
	ctx, cancel := purgeContext()
	defer cancel()
	refs, err := s.Store.ListBackupRunRefsForJob(jobID)
	if err != nil {
		log.Printf("backup purge: listing the archives of schedule %d failed, they are about to become unnameable: %v", jobID, err)
		return
	}
	services.PurgeBackupArchives(ctx, s.Store, s.BackupDeps(), refs)
}

// purgeContext is deliberately NOT the request's.
//
// The row delete that follows does not take a context at all, so a browser that
// hung up would cancel the purge and leave the delete to go ahead - which is
// exactly the orphan this code exists to prevent. Same reasoning as the route
// cleanup next to it, which runs on a background context for the same reason.
// The bound is generous: one S3 delete per archive, and a force-deleted machine
// can hold many.
func purgeContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Minute)
}
