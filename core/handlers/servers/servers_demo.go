package servers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"dylaris-core/handlers/access"
	"dylaris-core/handlers/httpx"
	"dylaris-core/services/demo"

	"github.com/gorilla/mux"
)

// SetServerDemo PATCH /api/admin/servers/{id}/demo - PANEL settings.write
// (RequireCap at the route). Adds or removes the server from the demo list.
// Multiple servers may be demos. The StoreEnabled check below is feature
// availability (hosted-build-only), not authorization, and stays.
func (h *ServerHandler) SetServerDemo(w http.ResponseWriter, r *http.Request) {
	if !h.d.StoreEnabled {
		httpx.SendJSONError(w, "Demo feature not available", http.StatusNotFound)
		return
	}
	serverID, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		httpx.SendJSONError(w, "Invalid server ID", http.StatusBadRequest)
		return
	}
	// A demo server is readable by every signed-in account, so flagging one on a
	// customer's machine published their console and stats to everyone.
	srv, err := h.d.Store.GetServerByID(serverID)
	if err != nil || srv == nil || access.ForeignToCaller(h.d.Store, h.d.FeatureFlags, r, srv.NodeID) {
		httpx.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	err = demo.EditList(h.d.Store, func(current []string) ([]string, error) {
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
			if again, err := h.d.Store.GetServerByID(serverID); err != nil || again == nil || again.UUID != srv.UUID {
				return nil, demo.ErrServerGone
			}
			next = append(next, srv.UUID)
		}
		return next, nil
	})
	if errors.Is(err, demo.ErrServerGone) {
		httpx.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	if err != nil {
		httpx.SendJSONError(w, "Failed to save demo setting", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "enabled": req.Enabled})
}

// GetDemoAccount GET /api/admin/settings/demo-account - PANEL settings.read
// (RequireCap at the route). Returns the username of the designated demo
// account ("" when unset). The StoreEnabled check below is feature
// availability (hosted-build-only), not authorization, and stays.
func (h *ServerHandler) GetDemoAccount(w http.ResponseWriter, r *http.Request) {
	if !h.d.StoreEnabled {
		httpx.SendJSONError(w, "Demo feature not available", http.StatusNotFound)
		return
	}
	username := ""
	if uuid, _ := h.d.Store.GetSetting(demo.AccountUUIDSetting); uuid != "" {
		if u, err := h.d.Store.GetUserByID(uuid); err == nil && u != nil {
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
	if !h.d.StoreEnabled {
		httpx.SendJSONError(w, "Demo feature not available", http.StatusNotFound)
		return
	}
	var req struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(req.Username)
	if username == "" {
		if err := h.d.Store.SetSetting(demo.AccountUUIDSetting, ""); err != nil {
			httpx.SendJSONError(w, "Failed to clear demo account", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "username": ""})
		return
	}
	u, err := h.d.Store.GetUserByUsername(username)
	if err != nil || u == nil {
		httpx.SendJSONError(w, "User not found", http.StatusNotFound)
		return
	}
	if u.IsAdmin {
		httpx.SendJSONError(w, "An admin cannot be the demo account", http.StatusBadRequest)
		return
	}
	// Anyone can open a demo session, and it reads everything the account
	// reads. A staff account there handed users.read, audit.read or a node's
	// credentials to the public.
	if caps, ok := access.StrictPanelCaps(h.d.Store, u.ID); !ok || len(caps) > 0 {
		httpx.SendJSONError(w, "A staff account cannot be the demo account", http.StatusBadRequest)
		return
	}
	if err := h.d.Store.SetSetting(demo.AccountUUIDSetting, u.ID); err != nil {
		httpx.SendJSONError(w, "Failed to save demo account", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "username": u.Username})
}
