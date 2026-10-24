package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
)

// schedZoneStore is schedCapStore for a server that runs in Europe/Berlin,
// keeping the task it was asked to create.
type schedZoneStore struct {
	schedCapStore
	created *models.ScheduledTask
}

func (f *schedZoneStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, UUID: "srv-uuid", OwnerID: "owner-1", Timezone: "Europe/Berlin"}, nil
}
func (f *schedZoneStore) CreateScheduledTask(t *models.ScheduledTask) (int, error) {
	c := *t
	f.created = &c
	return 7, nil
}

// A new task is read in the server's zone unless it names one, and a zone that
// is not one is refused before anything is stored.
func TestScheduledTaskCreateTakesTheServersZone(t *testing.T) {
	cases := []struct {
		name, tz, want string
		status         int
	}{
		{"no zone given", "", "Europe/Berlin", http.StatusOK},
		{"own zone", "Asia/Tokyo", "Asia/Tokyo", http.StatusOK},
		{"unknown zone", "Mars/Olympus", "", http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &schedZoneStore{}
			h := NewScheduledTasksHandler(schedCapHandler(&fs.schedCapStore).state)
			h.state.Store = fs
			rec := httptest.NewRecorder()
			h.Create(rec, schedCapReq(http.MethodPost, "/api/servers/1/scheduled-tasks", "owner-1",
				scheduledTaskRequest{Name: "t", TaskType: "restart", ScheduleCron: "0 4 * * *", Timezone: c.tz}))
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.status, rec.Body.String())
			}
			if c.status != http.StatusOK {
				if fs.created != nil {
					t.Fatalf("stored a task with zone %q", fs.created.Timezone)
				}
				return
			}
			if fs.created == nil || fs.created.Timezone != c.want {
				t.Fatalf("created %+v, want zone %s", fs.created, c.want)
			}
			// 04:00 in that zone, not 04:00 UTC.
			if fs.created.NextRun == nil || fs.created.NextRun.UTC().Hour() == 4 {
				t.Fatalf("next run %v was computed in UTC", fs.created.NextRun)
			}
		})
	}
}
