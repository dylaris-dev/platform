package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"dylaris-core/authz"
	"dylaris-core/models"

	"github.com/gorilla/mux"
)

// Server audit event types — kept in one block so producers and consumers
// share a single vocabulary. New events get appended here.
const (
	ServerAuditEventPowerAction        = "power_action"
	ServerAuditEventMemberInvited      = "member_invited"
	ServerAuditEventMemberRemoved      = "member_removed"
	ServerAuditEventMemberPermsChanged = "member_perms_changed"
	ServerAuditEventResourcesChanged   = "resources_changed"
	ServerAuditEventNameChanged        = "name_changed"
	ServerAuditEventSetup              = "setup"
	ServerAuditEventReinstall          = "reinstall"
	ServerAuditEventSubServerDeleted   = "subserver_deleted"
	ServerAuditEventSubServerSwitched  = "subserver_switched"
	// Deliberately has no producer: server_audit_events.server_id is
	// ON DELETE CASCADE, so a row written while the server is being deleted is
	// removed by the same statement. Recording a server deletion needs a log
	// that outlives the server, not this one.
	ServerAuditEventDeleted = "deleted"
	// The Java image or the JVM flags, changed WITHOUT reinstalling. Its own
	// event rather than resources_changed: that one means RAM, CPU and disk, and
	// an audit trail that answers "what happened to this server" with the wrong
	// noun is worse than one that says nothing.
	ServerAuditEventRuntimeChanged         = "runtime_changed"
	ServerAuditEventForceOnChanged         = "audit_force_on_changed"
	ServerAuditEventLoaderMetadataDeclared = "loader_metadata_declared"
)

// LogServerAudit appends one audit row when the server has audit enabled
// (either auto-flipped by InviteMember or admin-forced). Best-effort:
// failures log but never block the originating request.
//
// Set targetUserID to "" when the event isn't about a specific user (most
// events). Use it for member-related events so admins can answer "who was
// removed from my server" without parsing metadata.
func LogServerAudit(state *AppState, r *http.Request, serverID int, eventType string, actorID, targetID string, metadata map[string]interface{}) {
	if state == nil || state.Store == nil || serverID <= 0 {
		return
	}
	// Gate on the effective audit state. Avoids inserting rows that nobody
	// can ever see — they'd just consume disk + retention sweep cycles.
	enabled, force, _, err := state.Store.GetServerAuditState(serverID)
	if err != nil {
		log.Printf("server-audit: state lookup for %d: %v", serverID, err)
		return
	}
	if !enabled && !force {
		return
	}

	ev := &models.ServerAuditEvent{
		ServerID:  serverID,
		EventType: eventType,
		Metadata:  metadata,
	}
	if actorID != "" {
		a := actorID
		ev.ActorUserID = &a
	}
	if targetID != "" {
		t := targetID
		ev.TargetUserID = &t
	}
	if r != nil {
		ev.IPAddress = clientIP(r)
		ev.UserAgent = r.UserAgent()
	}
	if err := state.Store.InsertServerAudit(ev); err != nil {
		log.Printf("server-audit: insert %s for %d: %v", eventType, serverID, err)
		return
	}
	// This handler has said what happened, in its own words and with its own
	// metadata. RecordServerWrite then stays quiet rather than adding a second,
	// blunter row for the same action.
	if r != nil {
		authz.MarkAudited(r.Context())
	}
}

// RecordServerWrite is the audit recorder RequireCap calls after an authorized
// server-scoped action. It is installed on the resolver in main.
//
// The capability IS the event type. It reads as what the owner granted -
// "files.delete" is the line in the preset they picked - and it needs no
// per-capability vocabulary to be kept in step with the catalog, which is the
// kind of list that goes stale the first time somebody adds a capability.
//
// RequireCap calls this only for an action that succeeded and that no handler
// has already described, so there is nothing to filter here.
func RecordServerWrite(state *AppState) authz.WriteAuditFunc {
	return func(r *http.Request, serverID int, capID string, status int) {
		actorID, _ := r.Context().Value("userID").(string)
		LogServerAudit(state, r, serverID, capID, actorID, "", map[string]interface{}{
			"method": r.Method,
			"path":   r.URL.Path,
		})
	}
}

// EnableServerAuditIfNeeded flips audit_enabled to TRUE the first time it's
// called on a server that hasn't enabled it yet. Called from the member
// invite path. Cheap when audit is already on — just one indexed SELECT.
func EnableServerAuditIfNeeded(state *AppState, serverID int) {
	if state == nil || state.Store == nil {
		return
	}
	enabled, _, _, err := state.Store.GetServerAuditState(serverID)
	if err != nil || enabled {
		return
	}
	if err := state.Store.SetServerAuditEnabled(serverID, true); err != nil {
		log.Printf("server-audit: enable for %d: %v", serverID, err)
	}
}

// ── Endpoints ────────────────────────────────────────────────────────

type ServerAuditHandler struct {
	state *AppState
}

func NewServerAuditHandler(state *AppState) *ServerAuditHandler {
	return &ServerAuditHandler{state: state}
}

// gateView loads the server for an audit-log read. Access control lives at
// the route (RequireCap(server.audit.read)); this only resolves the path id
// and 404s on a missing server.
func (h *ServerAuditHandler) gateView(w http.ResponseWriter, r *http.Request) (*models.Server, string, bool) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil || id <= 0 {
		sendJSONError(w, "Invalid server id", http.StatusBadRequest)
		return nil, "", false
	}
	srv, err := h.state.Store.GetServerByID(id)
	if err != nil || srv == nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return nil, "", false
	}
	userID, _ := r.Context().Value("userID").(string)
	return srv, userID, true
}

// ListAudit GET /api/servers/{id}/audit - the server's audit trail, paged with
// ?limit and ?offset and filterable by ?eventType. The reply carries the
// unpaged total alongside the page.
func (h *ServerAuditHandler) ListAudit(w http.ResponseWriter, r *http.Request) {
	srv, _, ok := h.gateView(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit := parseIntDefault(q.Get("limit"), 100)
	offset := parseIntDefault(q.Get("offset"), 0)
	eventType := q.Get("eventType")
	events, total, err := h.state.Store.ListServerAudit(srv.ID, eventType, limit, offset)
	if err != nil {
		sendJSONError(w, "Failed to load audit", http.StatusInternalServerError)
		return
	}
	if events == nil {
		events = []models.ServerAuditEvent{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"events":  events,
		"total":   total,
		"limit":   limit,
		"offset":  offset,
	})
}

// GetStatus GET /api/servers/{id}/audit/status - whether auditing is on for
// this server, whether an admin forced it on, and how many events are stored.
func (h *ServerAuditHandler) GetStatus(w http.ResponseWriter, r *http.Request) {
	srv, _, ok := h.gateView(w, r)
	if !ok {
		return
	}
	enabled, force, count, err := h.state.Store.GetServerAuditState(srv.ID)
	if err != nil {
		sendJSONError(w, "Failed to load state", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"state": models.ServerAuditState{
			Enabled:     enabled,
			ForceOn:     force,
			EffectiveOn: enabled || force,
			EventCount:  count,
		},
	})
}

type setForceRequest struct {
	ForceOn bool `json:"forceOn"`
}

// SetForce PUT /api/servers/{id}/audit/force - gated by the route
// (RequireCap(server.settings.write)): the owner, or a role-holder granted
// that cap, can force their own server's audit on regardless of member state.
// Admin still passes via the resolver's admin short-circuit.
func (h *ServerAuditHandler) SetForce(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil || id <= 0 {
		sendJSONError(w, "Invalid id", http.StatusBadRequest)
		return
	}
	if _, err := h.state.Store.GetServerByID(id); err != nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	var req setForceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.SetServerAuditForceOn(id, req.ForceOn); err != nil {
		sendJSONError(w, "Update failed", http.StatusInternalServerError)
		return
	}
	// Self-audit: the act of flipping the toggle is itself audited iff the
	// new state turns audit on. This way disabling force-on doesn't write
	// the row right before the table effectively stops accepting writes.
	if req.ForceOn {
		actorID, _ := r.Context().Value("userID").(string)
		LogServerAudit(h.state, r, id, ServerAuditEventForceOnChanged, actorID, "", map[string]interface{}{
			"force_on": req.ForceOn,
		})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"forceOn": req.ForceOn,
	})
}
