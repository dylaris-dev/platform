package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
)

// LinkServerToProxy: Link a game server to a proxy server
func (h *ServerHandler) LinkServerToProxy(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}

	// Check if proxy feature is enabled
	val, _ := h.state.Store.GetSetting("feature_proxy_enabled")
	if val == "false" {
		sendJSONError(w, "Proxy feature is disabled", 403)
		return
	}

	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", 400)
		return
	}

	var req struct {
		ProxyID int `json:"proxyId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ProxyID == 0 {
		sendJSONError(w, "proxyId required", 400)
		return
	}

	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", 404)
		return
	}

	if srv.ServerType == "proxy" {
		sendJSONError(w, "Cannot link a proxy to another proxy", 400)
		return
	}

	proxy, err := h.state.Store.GetServerByID(req.ProxyID)
	if err != nil {
		sendJSONError(w, "Proxy not found", 404)
		return
	}

	if proxy.ServerType != "proxy" {
		sendJSONError(w, "Target server is not a proxy", 400)
		return
	}

	proxyID := req.ProxyID
	if err := h.state.Store.UpdateServerProxyID(serverID, &proxyID); err != nil {
		sendJSONError(w, "Failed to link server", 500)
		return
	}

	// The proxy can only reach this server once the node has been told to allow
	// it, so republish now rather than at the next tick. Linking is the moment
	// somebody is watching for it to work.
	if h.state.NetPolicy != nil {
		h.state.NetPolicy.RunOnce(r.Context())
	}

	h.state.Events.Publish(r.Context(), "servers.changed", nil)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// GetProxyEndpoint returns the private IP a server holds inside its proxy's
// overlay network. Used by the NetworkView to surface the container's
// reachable address inside the network for Bungee/Velocity config.
// GET /api/servers/{id}/proxy-endpoint
func (h *ServerHandler) GetProxyEndpoint(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", 400)
		return
	}
	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", 404)
		return
	}

	// Resolve which proxy to query. Game-servers expose their endpoint via
	// the proxy they're linked to; proxy-servers can list all linked children.
	type endpoint struct {
		ServerID   int    `json:"serverId"`
		ServerName string `json:"serverName"`
		ProxyID    int    `json:"proxyId"`
		ProxyUUID  string `json:"proxyUuid"`
		IP         string `json:"ip"`
		Hostname   string `json:"hostname"` // container name = stable DNS in overlay
	}

	resolveIP := func(containerUUID, proxyUUID string) string {
		if h.state.Redis == nil {
			return ""
		}
		key := fmt.Sprintf("dylaris:server:%s:proxy_ip:%s", containerUUID, proxyUUID)
		val, _ := h.state.Redis.Get(r.Context(), key).Result()
		return val
	}

	if srv.ServerType == "proxy" {
		// Return endpoints for every linked game-server.
		// The fleet, not a visible list: a proxy on a customer's own node has
		// its backends on that same node, and filtering them out here would
		// stop that customer's proxy routing rather than hide anything.
		linked, _ := h.state.Store.ListAllServers()
		var out []endpoint
		for _, child := range linked {
			if child.ProxyID == nil || *child.ProxyID != srv.ID {
				continue
			}
			out = append(out, endpoint{
				ServerID:   child.ID,
				ServerName: child.Name,
				ProxyID:    srv.ID,
				ProxyUUID:  srv.UUID,
				IP:         resolveIP(child.UUID, srv.UUID),
				Hostname:   fmt.Sprintf("mc_%s", child.UUID),
			})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"endpoints": out,
		})
		return
	}

	if srv.ProxyID == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "endpoints": []endpoint{}})
		return
	}
	proxy, err := h.state.Store.GetServerByID(*srv.ProxyID)
	if err != nil {
		sendJSONError(w, "Proxy not found", 404)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"endpoints": []endpoint{
			{
				ServerID:   srv.ID,
				ServerName: srv.Name,
				ProxyID:    proxy.ID,
				ProxyUUID:  proxy.UUID,
				IP:         resolveIP(srv.UUID, proxy.UUID),
				Hostname:   fmt.Sprintf("mc_%s", srv.UUID),
			},
		},
	})
}

// UnlinkServerFromProxy: Remove a server's proxy link
func (h *ServerHandler) UnlinkServerFromProxy(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}

	// Check if proxy feature is enabled
	val, _ := h.state.Store.GetSetting("feature_proxy_enabled")
	if val == "false" {
		sendJSONError(w, "Proxy feature is disabled", 403)
		return
	}

	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", 400)
		return
	}

	if _, err := h.state.Store.GetServerByID(serverID); err != nil {
		sendJSONError(w, "Server not found", 404)
		return
	}

	if err := h.state.Store.UpdateServerProxyID(serverID, nil); err != nil {
		sendJSONError(w, "Failed to unlink server", 500)
		return
	}

	h.state.Events.Publish(r.Context(), "servers.changed", nil)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
