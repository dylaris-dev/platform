package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

// ScheduledTasksHandler exposes per-server cron task CRUD. Access is enforced
// by the RequireCap chokepoint in routes.go (schedule.read/write/delete).

type ScheduledTasksHandler struct {
	state *AppState
}

func NewScheduledTasksHandler(state *AppState) *ScheduledTasksHandler {
	return &ScheduledTasksHandler{state: state}
}

type scheduledTaskRequest struct {
	Name         string `json:"name"`
	TaskType     string `json:"taskType"`
	ScheduleCron string `json:"scheduleCron"`
	Payload      string `json:"payload"`
	Enabled      *bool  `json:"enabled,omitempty"`
}

// Task types pinned to restart + say. RCON-via-cron was considered but kept
// out: external API keys + the panel RCON tab already cover scheduled-RCON use
// cases without a third execution path.
var validTaskTypes = map[string]bool{"restart": true, "say": true}

const (
	scheduledTaskMaxName    = 128
	scheduledTaskMaxPayload = 512
)

// capForTaskType names the capability a task's EXECUTION needs, which is not
// the same thing as the capability to manage the schedule.
//
// A "restart" task restarts the server and a "say" task writes a command on its
// console, so schedule.write on its own was power.restart and console.send
// under another name. Measured on production: an account refused
// POST /power {"action":"restart"} and POST /console/command with 403 saved a
// minutely restart task, the server restarted a minute later with the run
// recorded "ok", and the same account's "say" task reached the live console.
//
// An unknown type returns "": validateTaskFields already refuses those, and a
// new type with no entry here would otherwise be refused for everyone.
func capForTaskType(taskType string) string {
	switch taskType {
	case "restart":
		return "power.restart"
	case "say":
		return "console.send"
	}
	return ""
}

// refuseWithoutTaskCap answers the request when the caller may edit the
// schedule but not run this kind of task, and reports whether it did. The
// resulting task type is what matters, so Update calls it with the patched
// value rather than the request's.
//
// Fails CLOSED with no resolver: this is an authorization check, and Core
// always has one.
func (h *ScheduledTasksHandler) refuseWithoutTaskCap(w http.ResponseWriter, r *http.Request, serverID int, taskType string) bool {
	capID := capForTaskType(taskType)
	if capID == "" {
		return false
	}
	if h.state == nil || h.state.Authz == nil {
		sendJSONError(w, "Authorization check failed", http.StatusInternalServerError)
		return true
	}
	res, err := h.state.Authz.Resolve(authz.IdentityFromContext(r.Context()), serverID)
	if err != nil || !res.HasCap(capID) {
		sendJSONError(w, "This task needs "+capID+", which you do not hold on this server",
			http.StatusForbidden)
		return true
	}
	return false
}

// normalizeTaskName and normalizeTaskPayload are the ONE place either field is
// cleaned. They exist because Create did all of this inline and Update did none
// of it - it only TrimSpace'd the payload - so a PATCH could store what a POST
// refuses.
//
// The payload becomes "say " + payload on the server's stdin queue, so an
// embedded newline is a second console command. Today the log-shipper strips
// CR/LF again before writing to the JVM's stdin, which is what kept the PATCH
// gap from being a live console-command injection for anyone holding
// schedule.write but not console.send. That is one line in a different service
// standing between a stored payload and command execution; the value must not
// carry a newline in the first place.
func normalizeTaskPayload(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", ""), "\n", "")
	return strings.TrimSpace(s)
}

func normalizeTaskName(s string) string {
	return strings.TrimSpace(s)
}

// validateTaskFields runs the checks both Create and Update need, against the
// already-normalized values. Returns "" when the task is acceptable.
func validateTaskFields(name, taskType, payload string) string {
	if len(name) > scheduledTaskMaxName {
		return "Name too long (max 128 characters)"
	}
	if len(payload) > scheduledTaskMaxPayload {
		return "Payload too long (max 512 characters)"
	}
	if !validTaskTypes[taskType] {
		return "Unsupported task type"
	}
	// A task flipped to "say" with nothing to say errors on every tick forever.
	// Create refuses it; Update could reach it by changing only the type.
	if taskType == "say" && payload == "" {
		return "Payload (message) required for 'say' task"
	}
	return ""
}

// List GET /api/servers/{id}/scheduled-tasks - the cron tasks configured on
// one server.
func (h *ScheduledTasksHandler) List(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	tasks, err := h.state.Store.ListScheduledTasksByServer(serverID)
	if err != nil {
		sendJSONError(w, "Failed to load tasks", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"tasks":   tasks,
	})
}

// Create POST /api/servers/{id}/scheduled-tasks - adds a cron task. The server
// is resolved first so an unknown id is 404: left to the insert, the server_id
// foreign key would surface as a 500 for what is plainly a missing server.
func (h *ScheduledTasksHandler) Create(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	// Without this the INSERT is what rejects an unknown server, via the
	// server_id foreign key, and the caller gets a 500 "Failed to create task"
	// for what is plainly a 404. Update and Delete already resolve the task
	// first, so Create was the only entry point left without the check; the
	// sibling handlers (tabs, backup jobs, members) all answer 404 here.
	if _, err := h.state.Store.GetServerByID(serverID); err != nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	var req scheduledTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.Name = normalizeTaskName(req.Name)
	req.ScheduleCron = strings.TrimSpace(req.ScheduleCron)
	req.Payload = normalizeTaskPayload(req.Payload)
	if msg := validateTaskFields(req.Name, req.TaskType, req.Payload); msg != "" {
		sendJSONError(w, msg, http.StatusBadRequest)
		return
	}
	if h.refuseWithoutTaskCap(w, r, serverID, req.TaskType) {
		return
	}
	next, err := services.ComputeNextRun(req.ScheduleCron, time.Now().UTC())
	if err != nil {
		sendJSONError(w, "Invalid cron expression", http.StatusBadRequest)
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	userID, _ := r.Context().Value("userID").(string)
	var createdBy *string
	if userID != "" {
		v := userID
		createdBy = &v
	}

	t := &models.ScheduledTask{
		ServerID:     serverID,
		Name:         req.Name,
		TaskType:     req.TaskType,
		ScheduleCron: req.ScheduleCron,
		Payload:      req.Payload,
		Enabled:      enabled,
		NextRun:      &next,
		CreatedBy:    createdBy,
	}
	id, err := h.state.Store.CreateScheduledTask(t)
	if err != nil {
		sendJSONError(w, "Failed to create task", http.StatusInternalServerError)
		return
	}
	created, _ := h.state.Store.GetScheduledTask(id)
	if created == nil {
		t.ID = id
		created = t
	}

	h.state.Events.Publish(r.Context(), "scheduled_tasks.changed", nil)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"task":    created,
	})
}

// Update PATCH /api/servers/{id}/scheduled-tasks/{taskId} - edits a cron task
// belonging to the server in the path.
func (h *ScheduledTasksHandler) Update(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	taskID, _ := strconv.Atoi(mux.Vars(r)["taskId"])
	existing, err := h.state.Store.GetScheduledTask(taskID)
	if err != nil || existing == nil || existing.ServerID != serverID {
		sendJSONError(w, "Task not found", http.StatusNotFound)
		return
	}
	var req scheduledTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	// Apply the patch onto a copy first, then validate the RESULT with the same
	// rules Create uses. Validating the request alone would miss the combination
	// that only a patch can reach - changing taskType to "say" while leaving the
	// existing empty payload in place.
	if req.Name != "" {
		existing.Name = normalizeTaskName(req.Name)
	}
	if req.TaskType != "" {
		existing.TaskType = req.TaskType
	}
	if req.Payload != "" {
		existing.Payload = normalizeTaskPayload(req.Payload)
	}
	if msg := validateTaskFields(existing.Name, existing.TaskType, existing.Payload); msg != "" {
		sendJSONError(w, msg, http.StatusBadRequest)
		return
	}
	// The PATCHED type, not the request's: changing a "say" task into a
	// "restart" one is how schedule.write would otherwise still reach a power
	// action, and a patch that leaves the type alone must still not let someone
	// who lost the capability re-enable the task.
	if h.refuseWithoutTaskCap(w, r, serverID, existing.TaskType) {
		return
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	if req.ScheduleCron != "" && req.ScheduleCron != existing.ScheduleCron {
		next, err := services.ComputeNextRun(req.ScheduleCron, time.Now().UTC())
		if err != nil {
			sendJSONError(w, "Invalid cron expression", http.StatusBadRequest)
			return
		}
		existing.ScheduleCron = req.ScheduleCron
		existing.NextRun = &next
	}
	// If we're (re-)enabling a task that lost its next_run, compute one.
	if existing.Enabled && existing.NextRun == nil {
		next, err := services.ComputeNextRun(existing.ScheduleCron, time.Now().UTC())
		if err == nil {
			existing.NextRun = &next
		}
	}
	if !existing.Enabled {
		existing.NextRun = nil
	}
	if err := h.state.Store.UpdateScheduledTask(existing); err != nil {
		sendJSONError(w, "Failed to save task", http.StatusInternalServerError)
		return
	}

	h.state.Events.Publish(r.Context(), "scheduled_tasks.changed", nil)

	updated, _ := h.state.Store.GetScheduledTask(taskID)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"task":    updated,
	})
}

// Delete DELETE /api/servers/{id}/scheduled-tasks/{taskId} - removes a cron
// task belonging to the server in the path.
func (h *ScheduledTasksHandler) Delete(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	taskID, _ := strconv.Atoi(mux.Vars(r)["taskId"])
	existing, err := h.state.Store.GetScheduledTask(taskID)
	if err != nil || existing == nil || existing.ServerID != serverID {
		sendJSONError(w, "Task not found", http.StatusNotFound)
		return
	}
	if err := h.state.Store.DeleteScheduledTask(taskID); err != nil {
		sendJSONError(w, "Failed to delete", http.StatusInternalServerError)
		return
	}

	h.state.Events.Publish(r.Context(), "scheduled_tasks.changed", nil)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// ValidateCron POST /api/scheduled-tasks/validate — used by the panel to
// preview the next-run timestamp for a cron string before the user saves.
// No server scope here — the cron preview is a pure transform with no
// side effects. Body: {scheduleCron}. Response: {success, valid, nextRun}.
func (h *ScheduledTasksHandler) ValidateCron(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ScheduleCron string `json:"scheduleCron"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	next, err := services.ComputeNextRun(strings.TrimSpace(req.ScheduleCron), time.Now().UTC())
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"valid":   false,
			"error":   err.Error(),
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"valid":   true,
		"nextRun": next,
	})
}
