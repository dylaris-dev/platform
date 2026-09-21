package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// schedCreateFakeStore embeds store.Store (nil) so it satisfies the interface
// at compile time; only what Create touches is overridden. CreateScheduledTask
// records whether the insert was attempted at all, which is the point of the
// test below: an unknown server must never reach the INSERT.
type schedCreateFakeStore struct {
	store.Store

	server    *models.Server
	serverErr error

	createCalls int
	createErr   error
}

func (f *schedCreateFakeStore) GetServerByID(int) (*models.Server, error) {
	if f.serverErr != nil {
		return nil, f.serverErr
	}
	return f.server, nil
}

func (f *schedCreateFakeStore) CreateScheduledTask(*models.ScheduledTask) (int, error) {
	f.createCalls++
	if f.createErr != nil {
		return 0, f.createErr
	}
	return 42, nil
}

func (f *schedCreateFakeStore) GetScheduledTask(int) (*models.ScheduledTask, error) {
	return &models.ScheduledTask{ID: 42}, nil
}

func newSchedCreateHandler(fs *schedCreateFakeStore) *ScheduledTasksHandler {
	return NewScheduledTasksHandler(&AppState{
		Store: fs,
		// Creating a task now needs the capability the task USES, so these
		// cases need a resolver. They are about the server lookup rather than
		// about authorization, so the request below acts as an admin and
		// short-circuits it; the capability itself is covered next door in
		// scheduled_tasks_cap_test.go.
		Authz:  authz.NewResolver(fs),
		Events: services.NewSystemEventsPublisher(nil),
	})
}

func schedCreateRequest(serverID string, body any) *http.Request {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/servers/"+serverID+"/scheduled-tasks", bytes.NewReader(raw))
	req = mux.SetURLVars(req, map[string]string{"id": serverID})
	ctx := context.WithValue(req.Context(), "userID", "admin-1")
	ctx = context.WithValue(ctx, "username", "admin")
	ctx = context.WithValue(ctx, "isAdmin", true)
	return req.WithContext(ctx)
}

// TestScheduledTasksCreate_UnknownServerIs404 is the regression guard for a
// live finding: with a valid body and an id no server has, Create ran the
// INSERT and let the server_id foreign key reject it, so the caller got a 500
// "Failed to create task". The sibling handlers (tabs, backup jobs, members)
// all answer 404 for the same request.
func TestScheduledTasksCreate_UnknownServerIs404(t *testing.T) {
	fs := &schedCreateFakeStore{serverErr: errors.New("sql: no rows in result set")}
	h := newSchedCreateHandler(fs)

	rw := httptest.NewRecorder()
	h.Create(rw, schedCreateRequest("999999", scheduledTaskRequest{
		Name:         "ghost",
		TaskType:     "restart",
		ScheduleCron: "0 4 * * *",
	}))

	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rw.Code, rw.Body.String())
	}
	if fs.createCalls != 0 {
		t.Errorf("CreateScheduledTask called %d time(s) for an unknown server; the FK must not be the check", fs.createCalls)
	}
}

// The check must not break the ordinary path.
func TestScheduledTasksCreate_KnownServerStillCreates(t *testing.T) {
	fs := &schedCreateFakeStore{server: &models.Server{ID: 1}}
	h := newSchedCreateHandler(fs)

	rw := httptest.NewRecorder()
	h.Create(rw, schedCreateRequest("1", scheduledTaskRequest{
		Name:         "nightly restart",
		TaskType:     "restart",
		ScheduleCron: "0 4 * * *",
	}))

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.createCalls != 1 {
		t.Errorf("CreateScheduledTask called %d time(s), want 1", fs.createCalls)
	}
}
