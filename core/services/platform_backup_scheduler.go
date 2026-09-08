package services

import (
	"context"
	"log"
	"time"

	"dylaris-core/models"
)

// Running platform backups on their schedule, and pruning what they leave.
//
// Without this the schedule field on the screen is a lie: a job saying "every
// 24h" would sit there forever and the retention count would prune nothing. A
// control that does nothing is worse than an absent one, because somebody
// believes it.

// platformSchedulerTick is how often the due list is asked. A platform backup is
// measured in hours, so a minute of drift on when it starts costs nothing and a
// tighter loop only adds queries.
const platformSchedulerTick = time.Minute

type platformSchedulerStore interface {
	ListDuePlatformBackupJobs() ([]models.PlatformBackupJob, error)
	GetPlatformBackupJob(id int) (*models.PlatformBackupJob, error)
	SetPlatformBackupJobSchedule(id int, next *time.Time) error
	ListPlatformBackupRunsOverRetention(jobID, keep int) ([]models.PlatformBackupRun, error)
	DeletePlatformBackupRun(id int) error
}

// PlatformBackupScheduler runs due platform jobs and prunes old bundles.
type PlatformBackupScheduler struct {
	store  platformSchedulerStore
	leader LeaderChecker

	// Runner is built per run rather than held: the destination, the Core
	// storage configuration and the database version can all change under a
	// running Core.
	Runner func() (*PlatformBackupRunner, error)
	// DeleteArchive removes the object a pruned run wrote.
	DeleteArchive func(ctx context.Context, run *models.PlatformBackupRun) error
}

func NewPlatformBackupScheduler(st platformSchedulerStore, leader LeaderChecker,
	runner func() (*PlatformBackupRunner, error),
	deleteArchive func(ctx context.Context, run *models.PlatformBackupRun) error) *PlatformBackupScheduler {
	return &PlatformBackupScheduler{store: st, leader: leader, Runner: runner, DeleteArchive: deleteArchive}
}

// Start ticks until ctx is cancelled.
func (s *PlatformBackupScheduler) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(platformSchedulerTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.tick(ctx)
			}
		}
	}()
}

func (s *PlatformBackupScheduler) tick(ctx context.Context) {
	// Leader-gated HERE rather than at the ticker, so the gate is part of the
	// thing being tested rather than of the loop around it. Every replica reads
	// the same due list; without this each one would start the same backup.
	if s.leader != nil && !s.leader.IsLeader() {
		return
	}
	jobs, err := s.store.ListDuePlatformBackupJobs()
	if err != nil {
		logErrf("platform-backup", "reading the due jobs: %v", err)
		return
	}
	for i := range jobs {
		s.runDue(ctx, &jobs[i])
	}
}

// runDue runs one due job and re-arms it.
//
// The schedule is re-armed whether the run SUCCEEDED or not, and that is
// deliberate. Leaving next_run_at in the past on a failure turns one broken
// backup into a run attempted every minute until somebody notices - which is
// the shape that fills a log, hammers a destination that is already refusing,
// and makes the real error harder to find rather than easier.
func (s *PlatformBackupScheduler) runDue(ctx context.Context, job *models.PlatformBackupJob) {
	defer s.rearm(job)

	if s.Runner == nil {
		logErrf("platform-backup", "job %d is due but no runner is configured", job.ID)
		return
	}
	runner, err := s.Runner()
	if err != nil {
		logErrf("platform-backup", "job %d: %v", job.ID, err)
		return
	}
	if runner == nil {
		// A factory that answers with neither a runner nor an error. Guarded
		// rather than trusted: this runs in a goroutine, so dereferencing it
		// would not fail one backup, it would take Core down.
		logErrf("platform-backup", "job %d: the runner factory returned nothing", job.ID)
		return
	}
	if _, err := runner.Run(ctx, job.ID); err != nil {
		logErrf("platform-backup", "job %d (%s): %v", job.ID, job.Name, err)
		return
	}
	s.Prune(ctx, job.ID)
}

func (s *PlatformBackupScheduler) rearm(job *models.PlatformBackupJob) {
	next := ComputeBackupNextRun(job.Schedule, time.Now())
	if err := s.store.SetPlatformBackupJobSchedule(job.ID, next); err != nil {
		logErrf("platform-backup", "job %d: re-arming the schedule: %v", job.ID, err)
	}
}

// Prune removes the bundles of one job beyond its retention count.
//
// The OBJECT goes first, then the row. The other order leaves an archive
// nothing points at: retention walks rows, so a file whose row is gone is
// invisible to every later prune while still counting against the destination.
// A failed delete therefore keeps its row, so the next prune tries again.
//
// Only successful runs are counted, which the store query enforces: counting
// every row would delete a good bundle to make room for a failed one.
func (s *PlatformBackupScheduler) Prune(ctx context.Context, jobID int) {
	job, err := s.store.GetPlatformBackupJob(jobID)
	if err != nil || job == nil {
		return
	}
	if job.RetentionCount <= 0 {
		// No retention is "keep everything", not "keep none". A zero here would
		// otherwise delete the bundle a run has just written.
		return
	}
	over, err := s.store.ListPlatformBackupRunsOverRetention(jobID, job.RetentionCount)
	if err != nil {
		logErrf("platform-backup", "job %d: reading prunable runs: %v", jobID, err)
		return
	}
	for i := range over {
		run := over[i]
		if run.StorageKey != "" && s.DeleteArchive != nil {
			if err := s.DeleteArchive(ctx, &run); err != nil {
				logErrf("platform-backup", "job %d: removing bundle %s: %v", jobID, run.StorageKey, err)
				continue
			}
		}
		if err := s.store.DeletePlatformBackupRun(run.ID); err != nil {
			logErrf("platform-backup", "job %d: removing run %d: %v", jobID, run.ID, err)
			continue
		}
		log.Printf("platform backup: pruned run %d of job %d", run.ID, jobID)
	}
}
