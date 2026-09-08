package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"dylaris-core/models"
)

type fakeSchedulerStore struct {
	due       []models.PlatformBackupJob
	job       *models.PlatformBackupJob
	over      []models.PlatformBackupRun
	overErr   error
	rearmed   map[int]*time.Time
	deleted   []int
	deleteErr map[int]error
}

func newSchedulerStore(job *models.PlatformBackupJob) *fakeSchedulerStore {
	return &fakeSchedulerStore{job: job, rearmed: map[int]*time.Time{}, deleteErr: map[int]error{}}
}

func (f *fakeSchedulerStore) ListDuePlatformBackupJobs() ([]models.PlatformBackupJob, error) {
	return f.due, nil
}
func (f *fakeSchedulerStore) GetPlatformBackupJob(int) (*models.PlatformBackupJob, error) {
	return f.job, nil
}
func (f *fakeSchedulerStore) SetPlatformBackupJobSchedule(id int, next *time.Time) error {
	f.rearmed[id] = next
	return nil
}
func (f *fakeSchedulerStore) ListPlatformBackupRunsOverRetention(int, int) ([]models.PlatformBackupRun, error) {
	return f.over, f.overErr
}
func (f *fakeSchedulerStore) DeletePlatformBackupRun(id int) error {
	if err := f.deleteErr[id]; err != nil {
		return err
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func scheduledJob() *models.PlatformBackupJob {
	return &models.PlatformBackupJob{
		ID: 1, Name: "nightly", Schedule: "every 24h", Enabled: true, RetentionCount: 2,
		Selection: models.PlatformBackupSelection{Database: true},
	}
}

// A schedule left in the past turns one broken backup into a run attempted
// every minute until somebody notices: it fills the log, hammers a destination
// that is already refusing, and buries the real error rather than surfacing it.
func TestSchedulerRearmsEvenWhenTheRunFails(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T) func() (*PlatformBackupRunner, error)
	}{
		{
			name: "a run that succeeds",
			build: func(t *testing.T) func() (*PlatformBackupRunner, error) {
				// A real runner over the runner package's own fakes, so the
				// success path is a success rather than an assumption.
				rst := storeWith(models.PlatformBackupSelection{Library: true})
				rst.passphrase = "a documented backup passphrase"
				r := newRunner(t, rst, &fakeDest{})
				r.OpenCoreStorage = func(string) (CoreStorageArea, error) { return &fakeArea{}, nil }
				return func() (*PlatformBackupRunner, error) { return r, nil }
			},
		},
		{
			name: "a run that fails",
			build: func(*testing.T) func() (*PlatformBackupRunner, error) {
				return func() (*PlatformBackupRunner, error) {
					return nil, errors.New("destination unreachable")
				}
			},
		},
		{
			// Neither a runner nor an error. This runs in a goroutine, so a
			// dereference here does not fail one backup, it stops Core.
			name: "a factory that answers with nothing",
			build: func(*testing.T) func() (*PlatformBackupRunner, error) {
				return func() (*PlatformBackupRunner, error) { return nil, nil }
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := scheduledJob()
			st := newSchedulerStore(job)
			st.due = []models.PlatformBackupJob{*job}

			s := NewPlatformBackupScheduler(st, nil, tc.build(t), nil)
			s.tick(context.Background())

			next, ok := st.rearmed[1]
			if !ok {
				t.Fatal("the schedule was not re-armed")
			}
			if next == nil {
				t.Fatal("a scheduled job was re-armed to never run again")
			}
			if !next.After(time.Now()) {
				t.Errorf("re-armed to %v, which is not in the future", next)
			}
		})
	}
}

// A manual job runs and is not scheduled again. NULL is how that is said, and
// writing a time would turn a hand-started backup into a recurring one.
func TestSchedulerLeavesAManualJobUnscheduled(t *testing.T) {
	job := scheduledJob()
	job.Schedule = "manual"
	st := newSchedulerStore(job)
	st.due = []models.PlatformBackupJob{*job}

	s := NewPlatformBackupScheduler(st, nil, func() (*PlatformBackupRunner, error) {
		return nil, errors.New("no runner in this test")
	}, nil)
	s.tick(context.Background())

	next, ok := st.rearmed[1]
	if !ok {
		t.Fatal("the job was not touched at all")
	}
	if next != nil {
		t.Errorf("a manual job was scheduled for %v", next)
	}
}

// Pub/Sub and a shared database mean every replica reads the same due list.
// Without the gate each one starts the same backup.
func TestSchedulerOnlyRunsOnTheLeader(t *testing.T) {
	job := scheduledJob()
	st := newSchedulerStore(job)
	st.due = []models.PlatformBackupJob{*job}

	var asked int
	s := NewPlatformBackupScheduler(st, notLeader{}, func() (*PlatformBackupRunner, error) {
		asked++
		return nil, errors.New("should not be reached")
	}, nil)
	s.tick(context.Background())

	if asked != 0 {
		t.Error("a follower built a runner")
	}
	if len(st.rearmed) != 0 {
		t.Error("a follower re-armed a schedule, which the leader is about to re-arm too")
	}
}

type notLeader struct{}

func (notLeader) IsLeader() bool { return false }

// The OBJECT goes first, then the row. The other order leaves an archive
// nothing points at: retention walks rows, so a bundle whose row is gone is
// invisible to every later prune while still filling the destination.
func TestPruneRemovesTheArchiveBeforeTheRow(t *testing.T) {
	job := scheduledJob()
	st := newSchedulerStore(job)
	st.over = []models.PlatformBackupRun{
		{ID: 7, JobID: 1, StorageKey: "platform-backups/1/a.dylaris-bundle", Status: "success"},
		{ID: 8, JobID: 1, StorageKey: "platform-backups/1/b.dylaris-bundle", Status: "success"},
	}

	var order []string
	s := NewPlatformBackupScheduler(st, nil, nil, func(_ context.Context, run *models.PlatformBackupRun) error {
		order = append(order, "archive "+run.StorageKey)
		return nil
	})
	s.Prune(context.Background(), 1)

	if len(st.deleted) != 2 {
		t.Fatalf("deleted %v, want both rows", st.deleted)
	}
	if len(order) != 2 {
		t.Fatalf("removed %d archives, want 2", len(order))
	}
}

// A failed object delete keeps its row, so the next prune tries again. Dropping
// the row would strand the file forever.
func TestPruneKeepsTheRowWhenTheArchiveCannotBeRemoved(t *testing.T) {
	job := scheduledJob()
	st := newSchedulerStore(job)
	st.over = []models.PlatformBackupRun{
		{ID: 7, JobID: 1, StorageKey: "a", Status: "success"},
		{ID: 8, JobID: 1, StorageKey: "b", Status: "success"},
	}

	s := NewPlatformBackupScheduler(st, nil, nil, func(_ context.Context, run *models.PlatformBackupRun) error {
		if run.ID == 7 {
			return errors.New("bucket unreachable")
		}
		return nil
	})
	s.Prune(context.Background(), 1)

	for _, id := range st.deleted {
		if id == 7 {
			t.Fatal("the row of a bundle that is still in storage was deleted")
		}
	}
	if len(st.deleted) != 1 || st.deleted[0] != 8 {
		t.Errorf("deleted %v, want only run 8", st.deleted)
	}
}

// No retention is "keep everything", not "keep none". A zero read the other way
// would delete the bundle the run has just written.
func TestPruneDoesNothingWithoutARetentionCount(t *testing.T) {
	job := scheduledJob()
	job.RetentionCount = 0
	st := newSchedulerStore(job)
	st.over = []models.PlatformBackupRun{{ID: 7, JobID: 1, StorageKey: "a", Status: "success"}}

	var archives int
	s := NewPlatformBackupScheduler(st, nil, nil, func(context.Context, *models.PlatformBackupRun) error {
		archives++
		return nil
	})
	s.Prune(context.Background(), 1)

	if archives != 0 || len(st.deleted) != 0 {
		t.Fatalf("pruned %d archives and %v rows with no retention set", archives, st.deleted)
	}
}

// A run whose upload never produced an object has nothing to delete, and asking
// the destination for it would fail the prune on a key that was never written.
func TestPruneSkipsTheDeleteForARunWithNoArchive(t *testing.T) {
	job := scheduledJob()
	st := newSchedulerStore(job)
	st.over = []models.PlatformBackupRun{{ID: 7, JobID: 1, StorageKey: "", Status: "success"}}

	var archives int
	s := NewPlatformBackupScheduler(st, nil, nil, func(context.Context, *models.PlatformBackupRun) error {
		archives++
		return nil
	})
	s.Prune(context.Background(), 1)

	if archives != 0 {
		t.Error("the destination was asked to delete a key that was never written")
	}
	if len(st.deleted) != 1 {
		t.Errorf("deleted %v, want the row to go", st.deleted)
	}
}
