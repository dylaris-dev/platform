package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

// A backup run is committed as "running" before the work is handed to a node,
// and only a Pub/Sub result message ever moves it off that status. Pub/Sub is
// fire-and-forget, so a Core restart at the wrong moment loses the result and
// the row sits at "running" for good - PruneOldBackupRuns only ever deletes
// rows with status "success", so nothing else was going to clean it up either.

type reapUpdate struct {
	id      int
	status  string
	message string
	size    int64
	key     string
}

type reaperFakeStore struct {
	store.Store

	abandoned      []models.BackupRun
	listErr        error
	job            *models.BackupJob
	jobErr         error
	storage        *models.BackupStorage
	defaultStorage *models.BackupStorage
	updateErr      error

	// Captured for assertions.
	gotCutoff  time.Time
	gotLimit   int
	listCalls  int
	updates    []reapUpdate
	storageHit int
}

func (f *reaperFakeStore) ListAbandonedBackupRuns(startedBefore time.Time, limit int) ([]models.BackupRun, error) {
	f.listCalls++
	f.gotCutoff = startedBefore
	f.gotLimit = limit
	return f.abandoned, f.listErr
}

// GetBackupRun is the reaper's re-read before discarding an upload. None of
// these runs ever started one, which is what a missing row also answers.
func (f *reaperFakeStore) GetBackupRun(int) (*models.BackupRun, error) {
	return nil, errors.New("not tracked by this fake")
}

func (f *reaperFakeStore) UpdateBackupRunStatus(id int, status, message string, size int64, key string, _ time.Time) error {
	f.updates = append(f.updates, reapUpdate{id: id, status: status, message: message, size: size, key: key})
	return f.updateErr
}

func (f *reaperFakeStore) GetBackupJob(int) (*models.BackupJob, error) { return f.job, f.jobErr }

// The reaper resolves a run's storage through the owner chain now, so it asks
// who the server belongs to. A bare server with no owner is the platform case,
// which is what these tests are about.
func (f *reaperFakeStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id}, nil
}

func (f *reaperFakeStore) GetUserDefaultBackupStorage(string) (*models.BackupStorage, error) {
	return nil, nil
}

func (f *reaperFakeStore) GetBackupStorage(int) (*models.BackupStorage, error) {
	f.storageHit++
	return f.storage, nil
}

// Needed since storage resolution falls back to the default for a job with no
// storage of its own. The embedded store.Store is nil, so without this the
// fallback path nil-panics instead of returning "no storage".
func (f *reaperFakeStore) GetDefaultBackupStorage() (*models.BackupStorage, error) {
	return f.defaultStorage, nil
}

// localStorageAt builds a real "local" backup storage rooted at a temp dir, so
// the probe runs against a genuine filesystem backend rather than a stub. That
// matters here: the not-found branch depends on Stat reporting absence
// os.ErrNotExist style, which is a property of the provider, not of the reaper.
func localStorageAt(t *testing.T, dir string) *models.BackupStorage {
	t.Helper()
	cfg, err := json.Marshal(map[string]string{"basePath": dir})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return &models.BackupStorage{ID: 1, Name: "test", Provider: "local", Config: cfg}
}

func jobWithStorage(storageID int) *models.BackupJob {
	return &models.BackupJob{ID: 7, StorageID: &storageID}
}

// newReaper builds a scheduler with only the pieces reaping touches.
func newReaper(s store.Store) *BackupScheduler {
	return &BackupScheduler{store: s}
}

// TestReapAbandonedRuns_ResolvesRunsAccordingToWhatIsInStorage is the core
// behaviour. Every case ends at "failed" - never "success" - even the one where
// the archive is sitting right there, because Core has no confirmation from the
// node that it is complete, and presenting an unverified archive as a usable
// backup is the failure that is only discovered during a restore.
func TestReapAbandonedRuns_ResolvesRunsAccordingToWhatIsInStorage(t *testing.T) {
	tests := []struct {
		name         string
		storageKey   string
		writeArchive bool
		jobStorage   bool
		wantSize     int64
		wantInMsg    string
	}{
		{
			name:         "the archive is there but was never confirmed",
			storageKey:   "backups/present.tar.gz",
			writeArchive: true,
			jobStorage:   true,
			wantSize:     11,
			wantInMsg:    "UNVERIFIED",
		},
		{
			name:       "no archive was ever written",
			storageKey: "backups/missing.tar.gz",
			jobStorage: true,
			wantSize:   0,
			wantInMsg:  "did not complete",
		},
		{
			name:       "the run never got a storage key",
			storageKey: "",
			jobStorage: true,
			wantSize:   0,
			wantInMsg:  "never recorded a storage key",
		},
		{
			name:       "storage could not be asked",
			storageKey: "backups/unknown.tar.gz",
			jobStorage: false,
			wantSize:   0,
			wantInMsg:  "could not be determined",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.writeArchive {
				full := filepath.Join(dir, filepath.FromSlash(tt.storageKey))
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(full, []byte("tar.gz-data"), 0o644); err != nil {
					t.Fatalf("write archive: %v", err)
				}
			}

			now := time.Now()
			fs := &reaperFakeStore{
				abandoned: []models.BackupRun{{
					ID: 42, JobID: 7, Status: "running",
					StartedAt: now.Add(-8 * time.Hour), StorageKey: tt.storageKey,
				}},
				storage: localStorageAt(t, dir),
			}
			if tt.jobStorage {
				fs.job = jobWithStorage(1)
			} else {
				// No storage on the job AND none set as the platform default
				// (defaultStorage is nil here), so resolution genuinely has
				// nothing to open. A job with no storage of its own is NOT
				// enough on its own any more - it falls back to the default.
				fs.job = &models.BackupJob{ID: 7}
			}

			newReaper(fs).reapAbandonedRuns(context.Background(), now)

			if len(fs.updates) != 1 {
				t.Fatalf("updates = %d, want 1", len(fs.updates))
			}
			got := fs.updates[0]
			if got.id != 42 {
				t.Errorf("updated run id = %d, want 42", got.id)
			}
			if got.status != "failed" {
				t.Errorf("status = %q, want %q: an unconfirmed archive must never be reported as a usable backup", got.status, "failed")
			}
			if got.size != tt.wantSize {
				t.Errorf("size = %d, want %d", got.size, tt.wantSize)
			}
			if !strings.Contains(got.message, tt.wantInMsg) {
				t.Errorf("message = %q, want it to contain %q", got.message, tt.wantInMsg)
			}
			if !strings.Contains(got.message, "No result was received") {
				t.Errorf("message = %q, want it to say why the run was closed", got.message)
			}
			if got.key != tt.storageKey {
				t.Errorf("storage key = %q, want %q preserved", got.key, tt.storageKey)
			}
		})
	}
}

// TestReapAbandonedRuns_DoesNotTouchStorageWithoutAKey: with no key there is
// nothing to look for, and opening a backend to ask about "" would be a
// pointless round trip on every sweep.
func TestReapAbandonedRuns_DoesNotTouchStorageWithoutAKey(t *testing.T) {
	now := time.Now()
	fs := &reaperFakeStore{
		abandoned: []models.BackupRun{{ID: 1, JobID: 7, StartedAt: now.Add(-8 * time.Hour)}},
		job:       jobWithStorage(1),
		storage:   localStorageAt(t, t.TempDir()),
	}

	newReaper(fs).reapAbandonedRuns(context.Background(), now)

	if fs.storageHit != 0 {
		t.Fatalf("storage was opened %d times for a run with no key, want 0", fs.storageHit)
	}
}

// TestReapAbandonedRuns_AsksForRunsOlderThanTheWindow pins the cutoff. Passing
// `now` rather than `now - window` would close every run the moment it started.
func TestReapAbandonedRuns_AsksForRunsOlderThanTheWindow(t *testing.T) {
	now := time.Now()
	fs := &reaperFakeStore{}

	newReaper(fs).reapAbandonedRuns(context.Background(), now)

	if fs.listCalls != 1 {
		t.Fatalf("list calls = %d, want 1", fs.listCalls)
	}
	want := now.Add(-backupRunAbandonedAfter)
	if !fs.gotCutoff.Equal(want) {
		t.Fatalf("cutoff = %s, want %s (now minus the abandonment window)", fs.gotCutoff, want)
	}
	if fs.gotCutoff.After(now) {
		t.Fatal("cutoff is in the future, which would close runs that just started")
	}
	if fs.gotLimit != backupReapBatchSize {
		t.Fatalf("limit = %d, want %d", fs.gotLimit, backupReapBatchSize)
	}
}

// TestBackupRunAbandonedAfterClearsALongBackup states the sizing argument as an
// assertion. The window has to outlast a slow legitimate backup, because the
// cost of being early is a "failed" verdict against work that was still running.
func TestBackupRunAbandonedAfterClearsALongBackup(t *testing.T) {
	if backupRunAbandonedAfter < 4*time.Hour {
		t.Fatalf("backupRunAbandonedAfter = %s, want at least 4h so a large archive over a slow uplink is not cut short", backupRunAbandonedAfter)
	}
}

// TestReapAbandonedRuns_KeepsGoingAfterAFailedUpdate: one row that cannot be
// written must not strand the rest of the sweep behind it.
func TestReapAbandonedRuns_KeepsGoingAfterAFailedUpdate(t *testing.T) {
	now := time.Now()
	fs := &reaperFakeStore{
		abandoned: []models.BackupRun{
			{ID: 1, JobID: 7, StartedAt: now.Add(-8 * time.Hour)},
			{ID: 2, JobID: 7, StartedAt: now.Add(-9 * time.Hour)},
		},
		job:       jobWithStorage(1),
		storage:   localStorageAt(t, t.TempDir()),
		updateErr: errors.New("database is unavailable"),
	}

	newReaper(fs).reapAbandonedRuns(context.Background(), now)

	if len(fs.updates) != 2 {
		t.Fatalf("attempted updates = %d, want 2: a failure on one run must not abandon the others", len(fs.updates))
	}
}

// TestReapAbandonedRuns_SurvivesAListFailure: the sweep runs on every scheduler
// tick, so a database blip has to be a skipped sweep rather than a panic.
func TestReapAbandonedRuns_SurvivesAListFailure(t *testing.T) {
	fs := &reaperFakeStore{listErr: errors.New("database is unavailable")}

	newReaper(fs).reapAbandonedRuns(context.Background(), time.Now())

	if len(fs.updates) != 0 {
		t.Fatalf("updates = %d, want 0 when the list failed", len(fs.updates))
	}
}
