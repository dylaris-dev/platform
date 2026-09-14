package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// Demo servers are normal servers an admin flags as public read-only showcases.
// The list lives in a single setting (demo_server_uuids, JSON array of UUIDs)
// rather than a servers column, so no server scan/insert path changes. Non-owner
// viewers get READ-ONLY access to these servers (file list + read, console + stats
// view); every write stays denied by the default-deny access model (a demo viewer
// has no invite, so checkServerAccess returns false for all write endpoints).

const demoServerUUIDsSetting = "demo_server_uuids"

// demoAccountUUIDSetting holds the UUID of the single designated demo account.
// That account is forced read-only (GET-only) in AuthMiddleware and is the only
// account that sees the demo servers in its sidebar. Empty = feature off.
const demoAccountUUIDSetting = "demo_account_uuid"

// isDemoAccount reports whether the given user id is the designated demo account.
// Always false when the store integration is off — the whole demo feature only
// exists on the hosted build (STORE_URL + STORE_SHARED_KEY set).
func isDemoAccount(s *AppState, userID string) bool {
	demo, _ := isDemoAccountChecked(s, userID)
	return demo
}

// isDemoAccountChecked is isDemoAccount with the lookup failure kept.
//
// The error used to be discarded, and the zero value is the PERMISSIVE answer:
// a failed settings read made the demo account look like an ordinary user, so
// AuthMiddleware skipped its read-only gate and a public demo session could
// mutate. Callers that gate a WRITE must use this form and refuse when the
// answer is unknown; callers that only decide how to render may keep the
// boolean, where being wrong costs a misdrawn button.
//
// Deliberately NOT "assume demo on error": isDemoAccount is consulted for every
// non-admin, so returning true on a failed read would turn one unreadable
// setting into a fleet-wide write outage. Unknown is its own answer, and only
// the write paths have to care.
func isDemoAccountChecked(s *AppState, userID string) (bool, error) {
	if s == nil || !s.StoreEnabled || userID == "" {
		return false, nil
	}
	v, err := s.Store.GetSetting(demoAccountUUIDSetting)
	if err != nil {
		// A missing row is the answer "no demo account is designated", not a
		// failed read. The setting is never seeded, so treating ErrNoRows as an
		// error made AuthMiddleware refuse EVERY non-admin write on any hosted
		// install where nobody had ever nominated a demo account - the whole
		// tenant side of the panel, 503, until an unrelated feature was
		// configured. Same convention as the node admission gate.
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return v != "" && v == userID, nil
}

// loadDemoServerUUIDs returns the admin-flagged demo server UUIDs.
func loadDemoServerUUIDs(st store.Store) []string {
	raw, _ := st.GetSetting(demoServerUUIDsSetting)
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// isDemoServer reports whether the given server UUID is on the demo list.
// Always false when the store integration is off (open-core/self-host build).
func isDemoServer(s *AppState, uuid string) bool {
	if s == nil || !s.StoreEnabled || uuid == "" {
		return false
	}
	for _, u := range loadDemoServerUUIDs(s.Store) {
		if u == uuid {
			return true
		}
	}
	return false
}

// IsDemoServerID reports whether the numeric server id maps to an admin-flagged
// demo showcase server. Exported so main can hand the authz resolver a demo-read
// predicate without the resolver importing handlers. Always false when the
// store integration is off (open-core/self-host build).
func (s *AppState) IsDemoServerID(serverID int) bool {
	if s == nil || !s.StoreEnabled || s.Store == nil {
		return false
	}
	srv, err := s.Store.GetServerByID(serverID)
	if err != nil || srv == nil {
		return false
	}
	return isDemoServer(s, srv.UUID)
}

// SetServerDemo PATCH /api/admin/servers/{id}/demo - PANEL settings.write
// (RequireCap at the route). Adds or removes the server from the demo list.
// Multiple servers may be demos. The StoreEnabled check below is feature
// availability (hosted-build-only), not authorization, and stays.
func (h *ServerHandler) SetServerDemo(w http.ResponseWriter, r *http.Request) {
	if !h.state.StoreEnabled {
		sendJSONError(w, "Demo feature not available", http.StatusNotFound)
		return
	}
	serverID, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", http.StatusBadRequest)
		return
	}
	// A demo server is readable by every signed-in account, so flagging one on a
	// customer's machine published their console and stats to everyone.
	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil || srv == nil || foreignToCaller(h.state, r, srv.NodeID) {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	current := loadDemoServerUUIDs(h.state.Store)
	next := make([]string, 0, len(current)+1)
	seen := false
	for _, u := range current {
		if u == srv.UUID {
			seen = true
			if req.Enabled {
				next = append(next, u) // keep
			}
			// when disabling, drop it (don't append)
			continue
		}
		next = append(next, u)
	}
	if req.Enabled && !seen {
		next = append(next, srv.UUID)
	}

	data, _ := json.Marshal(next)
	if err := h.state.Store.SetSetting(demoServerUUIDsSetting, string(data)); err != nil {
		sendJSONError(w, "Failed to save demo setting", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "enabled": req.Enabled})
}

// GetDemoAccount GET /api/admin/settings/demo-account - PANEL settings.read
// (RequireCap at the route). Returns the username of the designated demo
// account ("" when unset). The StoreEnabled check below is feature
// availability (hosted-build-only), not authorization, and stays.
func (h *ServerHandler) GetDemoAccount(w http.ResponseWriter, r *http.Request) {
	if !h.state.StoreEnabled {
		sendJSONError(w, "Demo feature not available", http.StatusNotFound)
		return
	}
	username := ""
	if uuid, _ := h.state.Store.GetSetting(demoAccountUUIDSetting); uuid != "" {
		if u, err := h.state.Store.GetUserByID(uuid); err == nil && u != nil {
			username = u.Username
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "username": username})
}

// SetDemoAccount PUT /api/admin/settings/demo-account - PANEL settings.write
// (RequireCap at the route). Designates (by username) the read-only demo
// account, or clears it when the username is empty. An admin can never be the
// demo account. The StoreEnabled check below is feature availability
// (hosted-build-only), not authorization, and stays.
func (h *ServerHandler) SetDemoAccount(w http.ResponseWriter, r *http.Request) {
	if !h.state.StoreEnabled {
		sendJSONError(w, "Demo feature not available", http.StatusNotFound)
		return
	}
	var req struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(req.Username)
	if username == "" {
		if err := h.state.Store.SetSetting(demoAccountUUIDSetting, ""); err != nil {
			sendJSONError(w, "Failed to clear demo account", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "username": ""})
		return
	}
	u, err := h.state.Store.GetUserByUsername(username)
	if err != nil || u == nil {
		sendJSONError(w, "User not found", http.StatusNotFound)
		return
	}
	if u.IsAdmin {
		sendJSONError(w, "An admin cannot be the demo account", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.SetSetting(demoAccountUUIDSetting, u.ID); err != nil {
		sendJSONError(w, "Failed to save demo account", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "username": u.Username})
}
