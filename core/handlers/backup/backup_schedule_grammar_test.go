package backup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/authz"
)

// The server job handlers store the zone the schedule is read in, default it
// to UTC for a caller that sends none, and refuse what the scheduler could not
// run with the parser's own reason rather than a generic line.
func TestServerBackupJobScheduleAndZone(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode int
		wantZone string
		wantErr  string
	}{
		{"daily in the sent zone", `{"name":"n","schedule":"daily 03:00","timezone":"Europe/Berlin"}`, 200, "Europe/Berlin", ""},
		{"no zone is UTC", `{"name":"n","schedule":"every 6h from 12:00"}`, 200, "UTC", ""},
		{"the original form still works", `{"name":"n","schedule":"every 6h"}`, 200, "UTC", ""},
		{"cron", `{"name":"n","schedule":"cron 0 4 * * 1-5","timezone":"America/New_York"}`, 200, "America/New_York", ""},
		{"bad clock", `{"name":"n","schedule":"daily 25:00"}`, 400, "", "invalid time"},
		{"interval that does not divide the day", `{"name":"n","schedule":"every 5h from 12:00"}`, 400, "", "1, 2, 3, 4, 6, 8, 12 or 24"},
		{"cron every minute", `{"name":"n","schedule":"cron * * * * *"}`, 400, "", "at most once an hour"},
		{"unknown zone", `{"name":"n","schedule":"daily 03:00","timezone":"Mars/Base"}`, 400, "", "invalid time zone"},
		{"garbage", `{"name":"n","schedule":"banana"}`, 400, "", "Schedule must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &backupSubServerFakeStore{}
			h := &BackupHandler{d: testDeps(Deps{Store: fs, Authz: authz.NewResolver(fs)})}
			rw := httptest.NewRecorder()
			h.CreateJob(rw, backupJobRequest(http.MethodPost, "/api/servers/7/backup-jobs", tc.body, map[string]string{"id": "7"}))
			if rw.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (%s)", rw.Code, tc.wantCode, rw.Body.String())
			}
			if tc.wantCode != 200 {
				if fs.created != nil {
					t.Fatal("a refused job was stored")
				}
				if !strings.Contains(rw.Body.String(), tc.wantErr) {
					t.Fatalf("error %s does not say %q", rw.Body.String(), tc.wantErr)
				}
				return
			}
			if fs.created.Timezone != tc.wantZone {
				t.Errorf("zone = %q, want %q", fs.created.Timezone, tc.wantZone)
			}
			if fs.created.NextRunAt == nil || !fs.created.NextRunAt.After(time.Now()) {
				t.Errorf("next run = %v, want armed in the future", fs.created.NextRunAt)
			}
		})
	}
}

// A zone change alone moves "daily 03:00" to another instant, so it re-arms
// the job and is validated like a schedule change.
func TestUpdatingOnlyTheZoneRearmsTheJob(t *testing.T) {
	fs := &backupSubServerFakeStore{}
	h := &BackupHandler{d: testDeps(Deps{Store: fs, Authz: authz.NewResolver(fs)})}
	rw := httptest.NewRecorder()
	h.UpdateJob(rw, backupJobRequest(http.MethodPatch, "/api/backup-jobs/1", `{"schedule":"daily 03:00","timezone":"Asia/Tokyo"}`, map[string]string{"jobId": "1"}))
	if rw.Code != 200 {
		t.Fatalf("status = %d (%s)", rw.Code, rw.Body.String())
	}
	if fs.updated.Timezone != "Asia/Tokyo" || fs.updated.NextRunAt == nil {
		t.Fatalf("updated = %+v, want the zone stored and the job armed", fs.updated)
	}
	if local := fs.updated.NextRunAt.In(mustZone(t, "Asia/Tokyo")); local.Hour() != 3 || local.Minute() != 0 {
		t.Errorf("next run %v is not 03:00 in Tokyo", local)
	}

	fs = &backupSubServerFakeStore{}
	h = &BackupHandler{d: testDeps(Deps{Store: fs, Authz: authz.NewResolver(fs)})}
	rw = httptest.NewRecorder()
	h.UpdateJob(rw, backupJobRequest(http.MethodPatch, "/api/backup-jobs/1", `{"timezone":"Local"}`, map[string]string{"jobId": "1"}))
	if rw.Code != 400 || fs.updated != nil {
		t.Fatalf("the host zone was accepted: %d %s", rw.Code, rw.Body.String())
	}
}

func TestPreviewScheduleListsTheNextRuns(t *testing.T) {
	h := &BackupHandler{}
	rw := httptest.NewRecorder()
	h.PreviewSchedule(rw, httptest.NewRequest(http.MethodPost, "/api/backup-schedules/preview",
		strings.NewReader(`{"schedule":"every 6h from 12:00","timezone":"Europe/Berlin"}`)))
	var out struct {
		Valid    bool        `json:"valid"`
		Error    string      `json:"error"`
		NextRuns []time.Time `json:"nextRuns"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Valid || len(out.NextRuns) != 5 {
		t.Fatalf("preview = %+v, want five runs", out)
	}
	berlin := mustZone(t, "Europe/Berlin")
	for i, r := range out.NextRuns {
		if l := r.In(berlin); l.Hour()%6 != 0 || l.Minute() != 0 {
			t.Errorf("run %d at %v, want 00/06/12/18 in Berlin", i, l)
		}
		if i > 0 && !r.After(out.NextRuns[i-1]) {
			t.Errorf("runs are not increasing: %v", out.NextRuns)
		}
	}

	rw = httptest.NewRecorder()
	h.PreviewSchedule(rw, httptest.NewRequest(http.MethodPost, "/api/backup-schedules/preview",
		strings.NewReader(`{"schedule":"cron 0 0 30 2 *"}`)))
	out.Valid, out.Error = true, ""
	_ = json.Unmarshal(rw.Body.Bytes(), &out)
	if out.Valid || !strings.Contains(out.Error, "never runs") {
		t.Fatalf("a schedule that never fires previewed as %+v", out)
	}
}

// The platform handlers share the grammar and store the zone too.
func TestPlatformBackupJobTakesTheNewGrammar(t *testing.T) {
	st := &platformScheduleStore{}
	h := &PlatformBackupHandler{d: testDeps(Deps{Store: st})}
	rec := httptest.NewRecorder()
	h.CreateJob(rec, pbReq(http.MethodPost, `{"name":"n","schedule":"weekly sun 04:00","timezone":"America/New_York","selection":{"database":true}}`, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if st.created.Timezone != "America/New_York" || st.created.NextRunAt == nil {
		t.Fatalf("created = %+v", st.created)
	}
	if l := st.created.NextRunAt.In(mustZone(t, "America/New_York")); l.Weekday() != time.Sunday || l.Hour() != 4 {
		t.Errorf("next run %v, want a Sunday 04:00 in New York", l)
	}

	soon := time.Now().Add(2 * time.Hour)
	st = &platformScheduleStore{job: dailyJob(&soon, true)}
	h = &PlatformBackupHandler{d: testDeps(Deps{Store: st})}
	rec = httptest.NewRecorder()
	h.UpdateJob(rec, pbReq(http.MethodPatch, `{"schedule":"daily 99:00","selection":{"database":true}}`, map[string]string{"id": "3"}))
	if rec.Code != http.StatusBadRequest || st.updated != nil {
		t.Fatalf("an invalid daily time answered %d and stored %v", rec.Code, st.updated)
	}
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
