package handlers

import (
	"dylaris-core/models"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
)

// GetAdminServers GET /api/admin/servers — returns all DB servers with optional search filter
func (h *ServerHandler) GetAdminServers(w http.ResponseWriter, r *http.Request) {
	// The caller's own id matters even here: it is what keeps an operator's own
	// servers, and any they were invited to, in their own admin list once
	// customer-owned hardware stops being listed. Passing "" would have hidden
	// the admin's own BYON servers from the admin.
	userID, _ := r.Context().Value("userID").(string)
	servers, err := h.state.Store.ListServersForUser(userID, true)
	if err != nil {
		sendJSONError(w, "Database error", 500)
		return
	}
	if servers == nil {
		servers = []models.Server{}
	}
	// An admin's row on a customer's machine follows the owner's grant, as on
	// the tenant list: the settings fields only with server.settings.write.
	if isAdmin, _ := r.Context().Value("isAdmin").(bool); isAdmin {
		username, _ := r.Context().Value("username").(string)
		servers = applyResolvedTabPermissions(h.state, servers, userID, username)
	}

	search := strings.ToLower(r.URL.Query().Get("search"))
	if search != "" {
		filtered := servers[:0]
		for _, s := range servers {
			if strings.Contains(strings.ToLower(s.Name), search) ||
				strings.Contains(strings.ToLower(s.UUID), search) ||
				strings.Contains(strings.ToLower(s.OwnerName), search) {
				filtered = append(filtered, s)
			}
		}
		servers = filtered
	}

	// Same as the tenant list: an install nobody is working on says so here too,
	// or the admin overview is the one screen that still shows a bare spinner.
	annotateStalledInstallsFor(r.Context(), h.state, servers)

	memberCounts, _ := h.state.Store.CountInvitesPerServer()
	type adminServerRow struct {
		models.Server
		MemberCount int `json:"memberCount"`
	}
	// servers.read is held by the seeded 'support' role, whose whole point is
	// read-only oversight without the high-privilege panel caps. The row carries
	// the NODE's address, which is infrastructure rather than anything support
	// needs to answer a ticket, so a reader who is not an actual admin does not
	// get it. Everything else on the row is about the server itself.
	isAdmin, _ := r.Context().Value("isAdmin").(bool)
	rows := make([]adminServerRow, len(servers))
	for i, s := range servers {
		if !isAdmin {
			s.NodeAddress = ""
		}
		rows[i] = adminServerRow{Server: s, MemberCount: memberCounts[s.ID]}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"servers": rows,
	})
}

// AdminUpdateServerOwner PATCH /api/admin/servers/{id}/owner — reassigns a server to a different user
func (h *ServerHandler) AdminUpdateServerOwner(w http.ResponseWriter, r *http.Request) {
	// Admin-only, whatever servers.write says. The new owner gets the
	// resolver's owner short-circuit - files, console, RCON, backups - so a
	// staff member holding servers.write took any server on the platform's
	// machines for themselves, an admin's included, and nothing recorded it.
	// Servers do not move between accounts; this stays as the operator's
	// escape hatch, on the record.
	if !IsAdmin(r) {
		sendJSONError(w, "Only an administrator can change a server's owner", http.StatusForbidden)
		return
	}
	vars := mux.Vars(r)
	serverID, _ := strconv.Atoi(vars["id"])

	var req struct {
		UserID string `json:"userId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" {
		sendJSONError(w, "userId required", 400)
		return
	}

	// Handing a server to a new owner hands them the resolver's owner
	// short-circuit, so on a customer's machine this was a staff member taking
	// the customer's world for themselves.
	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil || srv == nil || foreignToCaller(h.state, r, srv.NodeID) {
		sendJSONError(w, "Server not found", 404)
		return
	}

	if _, err := h.state.Store.GetUserByID(req.UserID); err != nil {
		sendJSONError(w, "User not found", 404)
		return
	}

	if err := h.state.Store.UpdateServerOwner(serverID, &req.UserID); err != nil {
		sendJSONError(w, "Failed to update owner", 500)
		return
	}
	// On both accounts, so the trail of the one that LOST the server shows it.
	actorID, _ := r.Context().Value("userID").(string)
	meta := map[string]interface{}{
		"serverId": serverID, "serverUuid": srv.UUID, "previousOwner": srv.OwnerID, "newOwner": req.UserID,
	}
	LogIdentityAudit(h.state, r, AuditEventServerOwnerChanged, actorID, srv.OwnerID, meta)
	if req.UserID != srv.OwnerID {
		LogIdentityAudit(h.state, r, AuditEventServerOwnerChanged, actorID, req.UserID, meta)
	}

	h.state.Events.Publish(r.Context(), "servers.changed", nil)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
