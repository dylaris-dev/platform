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

	// existing is how many tasks the server already holds; settings backs
	// GetSetting (a missing key answers "", as the real store does).
	existing int
	settings map[string]string
}

func (f *schedCapStore) GetSetting(key string) (string, error) { return f.settings[key], nil }
func (f *schedCapStore) ListScheduledTasksByServer(int) ([]models.ScheduledTask, error) {
	return make([]models.ScheduledTask, f.existing), nil
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

// Switching a task OFF performs nothing, so it does not need the task's action
// right - but switching it back ON is exactly the re-enable the cap check exists
// to stop. A delegate with schedule.write and no power.restart could otherwise
// not pause the owner's restart task, only delete it.
func TestScheduledTaskDisablingNeedsNoActionRight(t *testing.T) {
	scheduleOnly := []string{"schedule.read", "schedule.write"}
	off, on := false, true

	t.Run("pausing a restart task", func(t *testing.T) {
		fs := &schedCapStore{
			friendCaps: scheduleOnly,
			task:       &models.ScheduledTask{ID: 7, ServerID: 1, Name: "t", TaskType: "restart", ScheduleCron: "0 4 * * *", Enabled: true},
		}
		rec := httptest.NewRecorder()
		schedCapHandler(fs).Update(rec, schedCapReq(http.MethodPatch, "/api/servers/1/scheduled-tasks/7", "friend-1",
			scheduledTaskRequest{Enabled: &off}))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("switching it back on still needs power.restart", func(t *testing.T) {
		fs := &schedCapStore{
			friendCaps: scheduleOnly,
			task:       &models.ScheduledTask{ID: 7, ServerID: 1, Name: "t", TaskType: "restart", ScheduleCron: "0 4 * * *", Enabled: false},
		}
		rec := httptest.NewRecorder()
		schedCapHandler(fs).Update(rec, schedCapReq(http.MethodPatch, "/api/servers/1/scheduled-tasks/7", "friend-1",
			scheduledTaskRequest{Enabled: &on}))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("disabling plus any other change is not a pure disable", func(t *testing.T) {
		fs := &schedCapStore{
			friendCaps: scheduleOnly,
			task:       &models.ScheduledTask{ID: 7, ServerID: 1, Name: "t", TaskType: "restart", ScheduleCron: "0 4 * * *", Enabled: true},
		}
		rec := httptest.NewRecorder()
		schedCapHandler(fs).Update(rec, schedCapReq(http.MethodPatch, "/api/servers/1/scheduled-tasks/7", "friend-1",
			scheduledTaskRequest{Enabled: &off, ScheduleCron: "0 5 * * *"}))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})
}

// The cap on tasks per server, under the platform limit convention: the product
// default when never saved, a real "none" at 0, no cap at "unlimited". It is
// checked for everyone, the owner included - the same as the sub-server cap.
func TestScheduledTaskCreateHonoursTheServerLimit(t *testing.T) {
	cases := []struct {
		name       string
		setting    string
		existing   int
		wantStatus int
	}{
		{"default 25, below", "", 24, http.StatusOK},
		{"default 25, at it", "", 25, http.StatusBadRequest},
		{"0 means none", "0", 0, http.StatusBadRequest},
		{"a set cap, below", "3", 2, http.StatusOK},
		{"a set cap, at it", "3", 3, http.StatusBadRequest},
		{"unlimited", "unlimited", 5000, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &schedCapStore{existing: c.existing, settings: map[string]string{}}
			if c.setting != "" {
				fs.settings[SettingMaxScheduledTasks] = c.setting
			}
			rec := httptest.NewRecorder()
			schedCapHandler(fs).Create(rec, schedCapReq(http.MethodPost, "/api/servers/1/scheduled-tasks", "owner-1",
				scheduledTaskRequest{Name: "n", TaskType: "restart", ScheduleCron: "0 4 * * *"}))
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.wantStatus, rec.Body.String())
			}
			if wantCreate := map[bool]int{true: 1, false: 0}[c.wantStatus == http.StatusOK]; fs.createCalls != wantCreate {
				t.Errorf("CreateScheduledTask called %d time(s), want %d", fs.createCalls, wantCreate)
			}
		})
	}
}
