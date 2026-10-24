package servers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"dylaris-core/handlers"

	"github.com/gorilla/mux"
)

// SetServerDemo PATCH /api/admin/servers/{id}/demo - PANEL settings.write
// (RequireCap at the route). Adds or removes the server from the demo list.
// Multiple servers may be demos. The StoreEnabled check below is feature
// availability (hosted-build-only), not authorization, and stays.
func (h *ServerHandler) SetServerDemo(w http.ResponseWriter, r *http.Request) {
	if !h.state.StoreEnabled {
		handlers.SendJSONError(w, "Demo feature not available", http.StatusNotFound)
		return
	}
	serverID, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		handlers.SendJSONError(w, "Invalid server ID", http.StatusBadRequest)
		return
	}
	// A demo server is readable by every signed-in account, so flagging one on a
	// customer's machine published their console and stats to everyone.
	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil || srv == nil || handlers.ForeignToCaller(h.state, r, srv.NodeID) {
		handlers.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	err = handlers.EditDemoList(h.state.Store, func(current []string) ([]string, error) {
		next := make([]string, 0, len(current)+1)
		for _, u := range current {
			if u != srv.UUID {
				next = append(next, u)
			}
		}
		if req.Enabled {
			// Asked again under the lock: a delete that committed after the
			// load above has already run its dropDemoServers, and adding the
			// UUID now would put a deleted server back on the list.
			if again, err := h.state.Store.GetServerByID(serverID); err != nil || again == nil || again.UUID != srv.UUID {
				return nil, handlers.ErrDemoServerGone
			}
			next = append(next, srv.UUID)
		}
		return next, nil
	})
	if errors.Is(err, handlers.ErrDemoServerGone) {
		handlers.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	if err != nil {
		handlers.SendJSONError(w, "Failed to save demo setting", http.StatusInternalServerError)
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
		handlers.SendJSONError(w, "Demo feature not available", http.StatusNotFound)
		return
	}
	username := ""
	if uuid, _ := h.state.Store.GetSetting(handlers.DemoAccountUUIDSetting); uuid != "" {
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
		handlers.SendJSONError(w, "Demo feature not available", http.StatusNotFound)
		return
	}
	var req struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(req.Username)
	if username == "" {
		if err := h.state.Store.SetSetting(handlers.DemoAccountUUIDSetting, ""); err != nil {
			handlers.SendJSONError(w, "Failed to clear demo account", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "username": ""})
		return
	}
	u, err := h.state.Store.GetUserByUsername(username)
	if err != nil || u == nil {
		handlers.SendJSONError(w, "User not found", http.StatusNotFound)
		return
	}
	if u.IsAdmin {
		handlers.SendJSONError(w, "An admin cannot be the demo account", http.StatusBadRequest)
		return
	}
	// Anyone can open a demo session, and it reads everything the account
	// reads. A staff account there handed users.read, audit.read or a node's
	// credentials to the public.
	if caps, ok := handlers.StrictPanelCaps(h.state, u.ID); !ok || len(caps) > 0 {
		handlers.SendJSONError(w, "A staff account cannot be the demo account", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.SetSetting(handlers.DemoAccountUUIDSetting, u.ID); err != nil {
		handlers.SendJSONError(w, "Failed to save demo account", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "username": u.Username})
}
