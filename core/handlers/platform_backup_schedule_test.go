package handlers

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// platformScheduleStore records what the handler arms. Production job 3 was
// stored with no next run and the scheduler only re-arms what it has run, so
// it never ran on its schedule; these tests pin that every write path arms it.
type platformScheduleStore struct {
	store.Store
	job      *models.PlatformBackupJob
	created  *models.PlatformBackupJob
	updated  *models.PlatformBackupJob
	runOpen  bool
	armedSet bool
	armed    *time.Time
}

func (f *platformScheduleStore) CreatePlatformBackupJob(j *models.PlatformBackupJob) (int, error) {
	c := *j
	f.created = &c
	return 3, nil
}
func (f *platformScheduleStore) GetPlatformBackupJob(int) (*models.PlatformBackupJob, error) {
	c := *f.job
	return &c, nil
}
func (f *platformScheduleStore) UpdatePlatformBackupJob(j *models.PlatformBackupJob) error {
	c := *j
	f.updated = &c
	return nil
}
func (f *platformScheduleStore) GetSetting(string) (string, error) {
	return "a documented backup passphrase", nil
}
func (f *platformScheduleStore) GetDefaultBackupStorage() (*models.BackupStorage, error) {
	return &models.BackupStorage{ID: 4}, nil
}
func (f *platformScheduleStore) CreatePlatformBackupRun(int, *int) (int, error) {
	f.runOpen = true
	return 9, nil
}
func (f *platformScheduleStore) FinishPlatformBackupRun(int, string, int64, string, string, []models.PlatformBackupComponent) error {
	return nil
}
func (f *platformScheduleStore) SetPlatformBackupJobSchedule(_ int, next *time.Time) error {
	f.armedSet = true
	f.armed = next
	return nil
}

func dailyJob(next *time.Time, enabled bool) *models.PlatformBackupJob {
	return &models.PlatformBackupJob{
		ID: 3, Name: "Daily platform databases", Schedule: "every 1d", Enabled: enabled,
		Selection: models.PlatformBackupSelection{Database: true}, NextRunAt: next,
	}
}

func aboutADayFromNow(t *testing.T, got *time.Time) {
	t.Helper()
	if got == nil {
		t.Fatal("no next run was set")
	}
	if d := time.Until(*got); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("next run in %v, want about a day", d)
	}
}

func TestCreatingAPlatformBackupJobArmsIt(t *testing.T) {
	st := &platformScheduleStore{}
	h := &PlatformBackupHandler{state: &AppState{Store: st}}
	rec := httptest.NewRecorder()
	h.CreateJob(rec, pbReq(http.MethodPost, `{"name":"daily","schedule":"every 1d","selection":{"database":true}}`, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	aboutADayFromNow(t, st.created.NextRunAt)

	// A schedule nothing can parse is refused, not stored to never run.
	st = &platformScheduleStore{}
	h = &PlatformBackupHandler{state: &AppState{Store: st}}
	rec = httptest.NewRecorder()
	h.CreateJob(rec, pbReq(http.MethodPost, `{"name":"daily","schedule":"daily","selection":{"database":true}}`, nil))
	if rec.Code != http.StatusBadRequest || st.created != nil {
		t.Fatalf("an unparseable schedule answered %d and stored %v", rec.Code, st.created)
	}
}

func TestUpdatingAPlatformBackupJobRearmsOnlyWhenTheTimingChanges(t *testing.T) {
	soon := time.Now().Add(2 * time.Hour)
	cases := []struct {
		name    string
		job     *models.PlatformBackupJob
		body    string
		wantDay bool // re-armed to about a day from now; otherwise unchanged
	}{
		{"a rename keeps the next run", dailyJob(&soon, true), `{"name":"renamed","selection":{"database":true}}`, false},
		{"the same schedule sent again keeps it", dailyJob(&soon, true), `{"schedule":"every 1d","selection":{"database":true}}`, false},
		{"a changed schedule re-arms", dailyJob(&soon, true), `{"schedule":"every 24h","selection":{"database":true}}`, true},
		{"re-enabling re-arms", dailyJob(&soon, false), `{"enabled":true,"selection":{"database":true}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &platformScheduleStore{job: tc.job}
			h := &PlatformBackupHandler{state: &AppState{Store: st}}
			rec := httptest.NewRecorder()
			h.UpdateJob(rec, pbReq(http.MethodPatch, tc.body, map[string]string{"id": "3"}))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			if tc.wantDay {
				aboutADayFromNow(t, st.updated.NextRunAt)
			} else if st.updated.NextRunAt == nil || !st.updated.NextRunAt.Equal(soon) {
				t.Fatalf("next = %v, want it unchanged at %v", st.updated.NextRunAt, soon)
			}
		})
	}
}

// Production, 2026-10-10: a run-now of job 3 succeeded and left last_run_at and
// next_run_at NULL. A run that was opened records the run and arms the next one
// even when it then fails; a refusal before any run touches neither.
func TestRunningAPlatformBackupJobByHandRecordsIt(t *testing.T) {
	// A WorkDir under a regular file cannot be created, so the run is opened and
	// then fails without needing pg_dump on the test host.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	st := &platformScheduleStore{job: dailyJob(nil, true)}
	h := &PlatformBackupHandler{state: &AppState{
		Store:                 st,
		PlatformDB:            services.PGConn{Name: "dylaris_db"},
		PlatformBackupWorkDir: filepath.Join(blocker, "work"),
	}}
	rec := httptest.NewRecorder()
	h.RunJob(rec, pbReq(http.MethodPost, "{}", map[string]string{"id": "3"}))
	if !st.runOpen {
		t.Fatalf("no run was opened (%d: %s)", rec.Code, rec.Body.String())
	}
	if !st.armedSet {
		t.Fatal("a manual run left last_run_at and next_run_at untouched")
	}
	aboutADayFromNow(t, st.armed)
}
