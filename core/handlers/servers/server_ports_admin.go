package servers

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gorilla/mux"

	"dylaris-core/handlers"
	"dylaris-core/services"
)

// PortsAdminHandler is the administrator's side of extra ports: settings,
// per node ranges, the overview with search, permissions and limits.
type PortsAdminHandler struct {
	state *handlers.AppState
}

func NewPortsAdminHandler(state *handlers.AppState) *PortsAdminHandler {
	return &PortsAdminHandler{state: state}
}

type portsNodeUsage struct {
	NodeID    int     `json:"nodeId"`
	NodeName  string  `json:"nodeName"`
	Range     string  `json:"range"`
	OwnRange  bool    `json:"ownRange"`
	GameRange string  `json:"gameRange"`
	Size      int     `json:"size"`
	Used      int     `json:"used"`
	Percent   float64 `json:"percent"`
	Conflict  bool    `json:"conflict"`
}

type portsSettingsView struct {
	Enabled    bool   `json:"enabled"`
	Range      string `json:"range"`
	MaxPerUser string `json:"maxPerUser"`
}

func (h *PortsAdminHandler) settingsView() portsSettingsView {
	en, _ := h.state.Store.GetSetting(services.PortsEnabledSettingKey)
	rng, _ := h.state.Store.GetSetting(services.PortsRangeSettingKey)
	max, _ := h.state.Store.GetSetting(services.PortsMaxPerUserSettingKey)
	on, _ := strconv.ParseBool(strings.TrimSpace(en))
	def := services.DefaultPortsPerUser
	return portsSettingsView{Enabled: on, Range: strings.TrimSpace(rng),
		MaxPerUser: services.FormatLimitSetting(services.ParseLimitSetting(strings.TrimSpace(max), &def))}
}

// GetOverview GET /api/admin/ports?q= - every extra port (searchable), usage
// per node range, and the feature settings.
func (h *PortsAdminHandler) GetOverview(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) > 100 {
		q = q[:100]
	}
	rows, err := h.state.Store.ListAllServerPorts(q)
	if err != nil {
		log.Printf("ports admin: %v", err)
		handlers.SendJSONError(w, "Failed to read the ports", http.StatusInternalServerError)
		return
	}
	all := rows
	if q != "" {
		if all, err = h.state.Store.ListAllServerPorts(""); err != nil {
			handlers.SendJSONError(w, "Failed to read the ports", http.StatusInternalServerError)
			return
		}
	}
	nodes, err := h.state.Store.ListNodes()
	if err != nil {
		handlers.SendJSONError(w, "Failed to read the nodes", http.StatusInternalServerError)
		return
	}
	own, err := h.state.Store.GetNodeExtraPortRanges()
	if err != nil {
		handlers.SendJSONError(w, "Failed to read the node ranges", http.StatusInternalServerError)
		return
	}
	settings := h.settingsView()
	beats := services.LoadHeartbeats(r.Context(), h.state.Redis)
	usage := make([]portsNodeUsage, 0, len(nodes))
	for _, n := range nodes {
		u := portsNodeUsage{NodeID: n.ID, NodeName: n.Name, Range: settings.Range}
		if rng := own[n.ID]; rng != "" {
			u.Range, u.OwnRange = rng, true
		}
		if hb := beats[n.Token]; hb != nil {
			u.GameRange = hb.PortRange
		}
		if min, max, err := services.ParsePortRange(u.Range); err == nil {
			// Size counts both protocols: TCP and UDP each have the range.
			u.Size = 2 * (max - min + 1)
			for _, p := range all {
				if p.NodeID == n.ID && p.HostPort >= min && p.HostPort <= max {
					u.Used++
				}
			}
			u.Percent = float64(int(float64(u.Used)/float64(u.Size)*1000)) / 10
			u.Conflict = u.GameRange != "" && services.PortRangesOverlap(u.Range, u.GameRange)
		}
		usage = append(usage, u)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "ports": rows, "nodes": usage, "settings": settings})
}

// SetSettings PUT /api/admin/ports/settings - {enabled?, range?, maxPerUser?}.
// maxPerUser is "" (product default), "unlimited" or a number.
func (h *PortsAdminHandler) SetSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled    *bool   `json:"enabled"`
		Range      *string `json:"range"`
		MaxPerUser *string `json:"maxPerUser"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if req.Range != nil {
		*req.Range = strings.TrimSpace(*req.Range)
		if *req.Range != "" {
			if _, _, err := services.ParsePortRange(*req.Range); err != nil {
				handlers.SendJSONErrorCode(w, "The port range is invalid: "+err.Error()+".", "port_range_invalid", http.StatusBadRequest)
				return
			}
		}
	}
	if req.MaxPerUser != nil {
		v := strings.TrimSpace(*req.MaxPerUser)
		if v != "" && v != services.LimitUnlimited {
			if n, err := strconv.Atoi(v); err != nil || n < 0 {
				handlers.SendJSONError(w, `maxPerUser must be "", "unlimited" or a number of 0 or more`, http.StatusBadRequest)
				return
			}
		}
		*req.MaxPerUser = v
	}
	set := func(key, val string) bool {
		if err := h.state.Store.SetSetting(key, val); err != nil {
			log.Printf("ports admin: save %s: %v", key, err)
			handlers.SendJSONError(w, "Failed to save the settings", http.StatusInternalServerError)
			return false
		}
		return true
	}
	if req.Enabled != nil && !set(services.PortsEnabledSettingKey, strconv.FormatBool(*req.Enabled)) {
		return
	}
	if req.Range != nil && !set(services.PortsRangeSettingKey, *req.Range) {
		return
	}
	if req.MaxPerUser != nil && !set(services.PortsMaxPerUserSettingKey, *req.MaxPerUser) {
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "settings": h.settingsView()})
}

// SetNodeRange PUT /api/admin/ports/nodes/{id}/range - {range}; "" goes back
// to the global range. Refused when it overlaps the node's game port range.
func (h *PortsAdminHandler) SetNodeRange(w http.ResponseWriter, r *http.Request) {
	nodeID := handlers.MustAtoi(mux.Vars(r)["id"])
	node, err := h.state.Store.GetNodeByID(nodeID)
	if err != nil || node == nil {
		handlers.SendJSONError(w, "Node not found", http.StatusNotFound)
		return
	}
	var req struct {
		Range string `json:"range"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	req.Range = strings.TrimSpace(req.Range)
	if req.Range != "" {
		if _, _, err := services.ParsePortRange(req.Range); err != nil {
			handlers.SendJSONErrorCode(w, "The port range is invalid: "+err.Error()+".", "port_range_invalid", http.StatusBadRequest)
			return
		}
		if hb := services.LoadHeartbeat(r.Context(), h.state.Redis, node.Token); hb != nil && services.PortRangesOverlap(req.Range, hb.PortRange) {
			handlers.SendJSONErrorCode(w, "The range overlaps this node's game port range "+hb.PortRange+". Pick one outside it.", "ports_range_conflict", http.StatusBadRequest)
			return
		}
	}
	if err := h.state.Store.SetNodeExtraPortRange(nodeID, req.Range); err != nil {
		log.Printf("ports admin: node %d range: %v", nodeID, err)
		handlers.SendJSONError(w, "Failed to save the range", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// ListPermissions GET /api/admin/port-permissions
func (h *PortsAdminHandler) ListPermissions(w http.ResponseWriter, r *http.Request) {
	rows, err := h.state.Store.ListPortPermissions()
	if err != nil {
		handlers.SendJSONError(w, "Failed to read the port permissions", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "permissions": rows, "global": h.settingsView().Enabled})
}

// SetPermission PUT /api/admin/port-permissions - {scope, allowed}
func (h *PortsAdminHandler) SetPermission(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope   string `json:"scope"`
		Allowed *bool  `json:"allowed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Allowed == nil || !services.ValidPortScope(req.Scope, false) {
		handlers.SendJSONError(w, `Invalid request: expected {"scope": "server:<id>|user:<id>|role:<id>", "allowed": true|false}`, http.StatusBadRequest)
		return
	}
	if err := h.state.Store.SetPortPermission(req.Scope, *req.Allowed); err != nil {
		handlers.SendJSONError(w, "Failed to save the port permission", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// DeletePermission DELETE /api/admin/port-permissions?scope=
func (h *PortsAdminHandler) DeletePermission(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if !services.ValidPortScope(scope, false) {
		handlers.SendJSONError(w, "Invalid scope", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.DeletePortPermission(scope); err != nil {
		handlers.SendJSONError(w, "Failed to delete the port permission", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// ListLimits GET /api/admin/port-limits
func (h *PortsAdminHandler) ListLimits(w http.ResponseWriter, r *http.Request) {
	rows, err := h.state.Store.ListPortLimits()
	if err != nil {
		handlers.SendJSONError(w, "Failed to read the port limits", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "limits": rows, "global": h.settingsView().MaxPerUser})
}

// SetLimit PUT /api/admin/port-limits - {scope, maxPorts}; maxPorts null is
// "no limit", 0 is "none". To defer to the next scope, DELETE the row.
func (h *PortsAdminHandler) SetLimit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope    string          `json:"scope"`
		MaxPorts json.RawMessage `json:"maxPorts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !services.ValidPortScope(req.Scope, true) || len(req.MaxPorts) == 0 {
		handlers.SendJSONError(w, `Invalid request: expected {"scope": "user:<id>|role:<id>", "maxPorts": null|<n>}`, http.StatusBadRequest)
		return
	}
	var max *int
	if string(req.MaxPorts) != "null" {
		var n int
		if err := json.Unmarshal(req.MaxPorts, &n); err != nil || n < 0 {
			handlers.SendJSONError(w, "maxPorts must be null or a number of 0 or more", http.StatusBadRequest)
			return
		}
		max = &n
	}
	if err := h.state.Store.SetPortLimit(req.Scope, max); err != nil {
		handlers.SendJSONError(w, "Failed to save the port limit", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// DeleteLimit DELETE /api/admin/port-limits?scope=
func (h *PortsAdminHandler) DeleteLimit(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if !services.ValidPortScope(scope, true) {
		handlers.SendJSONError(w, "Invalid scope", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.DeletePortLimit(scope); err != nil {
		handlers.SendJSONError(w, "Failed to delete the port limit", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}
