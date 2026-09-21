package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// A task's EXECUTION capability is not the capability to manage the schedule.
func TestCapForTaskType(t *testing.T) {
	cases := []struct{ taskType, want string }{
		{"restart", "power.restart"},
		{"say", "console.send"},
		// Unknown types are validateTaskFields' refusal, not this one's: a new
		// type with no entry here must not be refused for everyone.
		{"", ""},
		{"backup", ""},
	}
	for _, c := range cases {
		if got := capForTaskType(c.taskType); got != c.want {
			t.Errorf("capForTaskType(%q) = %q, want %q", c.taskType, got, c.want)
		}
	}
}

// schedCapStore answers the resolver and the handler for one server owned by
// "owner-1", with one friend whose grant it carries.
type schedCapStore struct {
	store.Store
	friendCaps  []string
	createCalls int
	task        *models.ScheduledTask
}

func (f *schedCapStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, UUID: "srv-uuid", OwnerID: "owner-1"}, nil
}
func (f *schedCapStore) GetServerByUUID(string) (*models.Server, error) {
	return f.GetServerByID(1)
}
func (f *schedCapStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}
func (f *schedCapStore) GetPanelRole(int) (*store.PanelRole, error)   { return nil, nil }
func (f *schedCapStore) GetServerRole(int) (*store.ServerRole, error) { return nil, nil }
func (f *schedCapStore) GetServerGrant(serverID int, userID string) (*store.ServerGrant, error) {
	if userID != "friend-1" {
		return nil, nil
	}
	return &store.ServerGrant{
		ServerID:     &serverID,
		UserID:       userID,
		OwnerUserID:  "owner-1",
		CapOverrides: store.CapOverrides{Grant: f.friendCaps},
	}, nil
}
func (f *schedCapStore) GetAccountGrant(string, string) (*store.ServerGrant, error) { return nil, nil }
func (f *schedCapStore) CreateScheduledTask(*models.ScheduledTask) (int, error) {
	f.createCalls++
	return 7, nil
}
func (f *schedCapStore) GetScheduledTask(int) (*models.ScheduledTask, error) { return f.task, nil }
func (f *schedCapStore) UpdateScheduledTask(*models.ScheduledTask) error     { return nil }

func schedCapHandler(fs *schedCapStore) *ScheduledTasksHandler {
	return NewScheduledTasksHandler(&AppState{
		Store:  fs,
		Authz:  authz.NewResolver(fs),
		Events: services.NewSystemEventsPublisher(nil),
	})
}

func schedCapReq(method, path, userID string, body any) *http.Request {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r = mux.SetURLVars(r, map[string]string{"id": "1", "taskId": "7"})
	ctx := context.WithValue(r.Context(), "userID", userID)
	ctx = context.WithValue(ctx, "username", userID)
	ctx = context.WithValue(ctx, "isAdmin", false)
	return r.WithContext(ctx)
}

// Measured on production. An account holding schedule.read/write/delete and
// nothing else was refused POST /power {"action":"restart"} and
// POST /console/command with 403, then saved a minutely restart task: a minute
// later the run was recorded "ok" and the server restarted. The same account's
// "say" task reached the live console. schedule.write was power.restart and
// console.send under another name.
func TestScheduledTaskCreateNeedsTheCapabilityTheTaskUses(t *testing.T) {
	scheduleOnly := []string{"overview.read", "schedule.read", "schedule.write", "schedule.delete"}

	cases := []struct {
		name       string
		userID     string
		caps       []string
		taskType   string
		payload    string
		wantStatus int
	}{
		{
			name:   "schedule.write alone cannot schedule a restart",
			userID: "friend-1", caps: scheduleOnly,
			taskType: "restart", wantStatus: http.StatusForbidden,
		},
		{
			name:   "schedule.write alone cannot schedule a console command",
			userID: "friend-1", caps: scheduleOnly,
			taskType: "say", payload: "hello", wantStatus: http.StatusForbidden,
		},
		{
			name:   "with power.restart the restart task is theirs to make",
			userID: "friend-1", caps: append(append([]string{}, scheduleOnly...), "power.restart"),
			taskType: "restart", wantStatus: http.StatusOK,
		},
		{
			name:   "with console.send the say task is theirs to make",
			userID: "friend-1", caps: append(append([]string{}, scheduleOnly...), "console.send"),
			taskType: "say", payload: "hello", wantStatus: http.StatusOK,
		},
		{
			// The owner short-circuits every capability, so the ordinary path
			// must not have moved at all.
			name: "the owner is unaffected", userID: "owner-1",
			taskType: "restart", wantStatus: http.StatusOK,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &schedCapStore{friendCaps: c.caps}
			rec := httptest.NewRecorder()
			schedCapHandler(fs).Create(rec, schedCapReq(http.MethodPost, "/api/servers/1/scheduled-tasks", c.userID,
				scheduledTaskRequest{Name: "t", TaskType: c.taskType, ScheduleCron: "* * * * *", Payload: c.payload}))

			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.wantStatus, rec.Body.String())
			}
			wantCreate := 0
			if c.wantStatus == http.StatusOK {
				wantCreate = 1
			}
			if fs.createCalls != wantCreate {
				t.Errorf("CreateScheduledTask called %d time(s), want %d", fs.createCalls, wantCreate)
			}
		})
	}
}

// The PATCH is where the capability could be walked around: a task type the
// caller may create, changed afterwards into one they may not.
func TestScheduledTaskUpdateChecksTheResultingType(t *testing.T) {
	scheduleAndSay := []string{"schedule.read", "schedule.write", "console.send"}

	t.Run("turning a say task into a restart needs power.restart", func(t *testing.T) {
		fs := &schedCapStore{
			friendCaps: scheduleAndSay,
			task:       &models.ScheduledTask{ID: 7, ServerID: 1, Name: "t", TaskType: "say", Payload: "hi", ScheduleCron: "* * * * *"},
		}
		rec := httptest.NewRecorder()
		schedCapHandler(fs).Update(rec, schedCapReq(http.MethodPatch, "/api/servers/1/scheduled-tasks/7", "friend-1",
			scheduledTaskRequest{TaskType: "restart"}))

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("editing the task they may already run still works", func(t *testing.T) {
		fs := &schedCapStore{
			friendCaps: scheduleAndSay,
			task:       &models.ScheduledTask{ID: 7, ServerID: 1, Name: "t", TaskType: "say", Payload: "hi", ScheduleCron: "* * * * *"},
		}
		rec := httptest.NewRecorder()
		schedCapHandler(fs).Update(rec, schedCapReq(http.MethodPatch, "/api/servers/1/scheduled-tasks/7", "friend-1",
			scheduledTaskRequest{Payload: "hello again"}))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})
}
