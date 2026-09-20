package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
)

// GetSftpCredentials GET /api/servers/{id}/sftp-credentials
// Returns SFTP connection info. When fileAccessMode == "beam", returns empty to avoid node IP exposure.
func (h *ServerHandler) GetSftpCredentials(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	serverID, _ := strconv.Atoi(vars["id"])
	username, _ := r.Context().Value("username").(string)

	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil || srv == nil {
		sendJSONError(w, "Server not found", 404)
		return
	}

	// If file mode is beam-only, do not expose node IP
	fileMode, _ := h.state.Store.GetSetting("file_access_mode")
	if fileMode == "beam" {
		// With a reason, like the external-node branch below. Without one a
		// client gets four empty fields and nothing to say about them, which is
		// how the panel's SFTP line came to sit in its loading state forever.
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":  true,
			"host":     "",
			"port":     0,
			"username": "",
			"path":     "",
			"reason":   "beam_only",
		})
		return
	}

	node, err := h.state.Store.GetNodeByID(srv.NodeID)
	if err != nil || node == nil {
		sendJSONError(w, "Node not found", 404)
		return
	}

	// External/home nodes force beam locally and never expose SFTP — withhold
	// credentials even when the platform-global file mode is sftp/both.
	if node.IsExternal() {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":  true,
			"host":     "",
			"port":     0,
			"username": "",
			"path":     "",
			"reason":   "external_node_beam_only",
		})
		return
	}

	host := node.Address
	if node.PublicIP != "" {
		host = node.PublicIP
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"host":     host,
		"port":     25520,
		"username": username,
		"path":     "/" + srv.UUID,
	})
}
