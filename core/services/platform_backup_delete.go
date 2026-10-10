package services

import (
	"context"
	"errors"
	"fmt"
	"log"

	"dylaris-core/models"
)

// ErrPlatformBackupRunning refuses deleting a job while one of its runs is
// writing: its bundle key is recorded only when the upload ends, so a delete
// now would leave that bundle with no row and no job.
var ErrPlatformBackupRunning = errors.New("a backup of this job is running; try again when it finishes")

type platformJobDeleteStore interface {
	ListPlatformBackupRuns(jobID, limit int) ([]models.PlatformBackupRun, error)
	DeletePlatformBackupRun(id int) error
	DeletePlatformBackupJob(id int) error
}

// DeletePlatformBackupJob removes a platform job together with its bundles.
//
// Same order as Prune, object first and row second, because the run row is
// the only record of where a bundle lives: deleting the job alone cascades the
// rows away and leaves every bundle in the bucket with nothing naming it. A
// bundle that cannot be deleted keeps its row AND the job, and the error says
// so - the operator retries, rather than the bucket quietly keeping it.
func DeletePlatformBackupJob(ctx context.Context, st platformJobDeleteStore, jobID int,
	deleteArchive func(ctx context.Context, run *models.PlatformBackupRun) error) error {
	runs, err := st.ListPlatformBackupRuns(jobID, 500)
	if err != nil {
		return fmt.Errorf("reading the job's runs: %w", err)
	}
	// Newest first, so a running run is on this first page.
	for _, r := range runs {
		if r.Status == "running" {
			return ErrPlatformBackupRunning
		}
	}
	failed := 0
	for {
		// ListPlatformBackupRuns caps a page at 500, so this walks in pages and
		// stops once a page deletes nothing (only failures are left).
		runs, err := st.ListPlatformBackupRuns(jobID, 500)
		if err != nil {
			return fmt.Errorf("reading the job's runs: %w", err)
		}
		failed = 0
		removed := 0
		for i := range runs {
			run := runs[i]
			if run.StorageKey != "" && deleteArchive != nil {
				if err := deleteArchive(ctx, &run); err != nil {
					log.Printf("platform backup: job %d: removing bundle %s: %v", jobID, run.StorageKey, err)
					failed++
					continue
				}
			}
			if err := st.DeletePlatformBackupRun(run.ID); err != nil {
				return fmt.Errorf("removing run %d: %w", run.ID, err)
			}
			removed++
		}
		if removed == 0 {
			break
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d backup file(s) of this job could not be deleted from the storage; the job was kept so the delete can be retried", failed)
	}
	return st.DeletePlatformBackupJob(jobID)
}
