package servers

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/gorilla/mux"

	"dylaris-core/handlers"
	"dylaris-core/handlers/httpx"
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
	NodeID   int    `json:"nodeId"`
	NodeName string `json:"nodeName"`
	Range    string `json:"range"`
	OwnRange bool   `json:"ownRange"`
	// DefaultRange: Range is the self-hosted default, nothing was saved.
	DefaultRange bool `json:"defaultRange"`
	// Host is the address players use for the node's extra ports; "" = its
	// public IP.
	Host      string  `json:"host"`
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
	// DefaultRange is the range a node without its own uses because the
	// global one was never saved; "" when a saved value or the SaaS rules it out.
	DefaultRange string `json:"defaultRange"`
}

func (h *PortsAdminHandler) settingsView() portsSettingsView {
	en, _ := h.state.Store.GetSetting(services.PortsEnabledSettingKey)
	rng, saved, err := services.GlobalPortRange(h.state.Store)
	if err != nil {
		log.Printf("ports admin: %v", err)
		saved = true // unknown: do not claim the default applies
	}
	max, _ := h.state.Store.GetSetting(services.PortsMaxPerUserSettingKey)
	on, _ := strconv.ParseBool(strings.TrimSpace(en))
	def := services.DefaultPortsPerUser
	v := portsSettingsView{Enabled: on, Range: rng,
		MaxPerUser: services.FormatLimitSetting(services.ParseLimitSetting(strings.TrimSpace(max), &def))}
	if d, isDefault := services.EffectivePortRange("", rng, saved, h.state.Hosted); isDefault {
		v.DefaultRange = d
	}
	return v
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
		httpx.SendJSONError(w, "Failed to read the ports", http.StatusInternalServerError)
		return
	}
	all := rows
	if q != "" {
		if all, err = h.state.Store.ListAllServerPorts(""); err != nil {
			httpx.SendJSONError(w, "Failed to read the ports", http.StatusInternalServerError)
			return
		}
	}
	nodes, err := h.state.Store.ListNodes()
	if err != nil {
		httpx.SendJSONError(w, "Failed to read the nodes", http.StatusInternalServerError)
		return
	}
	own, err := h.state.Store.GetNodeExtraPortRanges()
	if err != nil {
		httpx.SendJSONError(w, "Failed to read the node ranges", http.StatusInternalServerError)
		return
	}
	hosts, err := h.state.Store.GetNodeExtraPortsHosts()
	if err != nil {
		httpx.SendJSONError(w, "Failed to read the node addresses", http.StatusInternalServerError)
		return
	}
	settings := h.settingsView()
	beats := services.LoadHeartbeats(r.Context(), h.state.Redis)
	usage := make([]portsNodeUsage, 0, len(nodes))
	for _, n := range nodes {
		u := portsNodeUsage{NodeID: n.ID, NodeName: n.Name, Range: settings.Range, Host: hosts[n.ID]}
		if rng := own[n.ID]; rng != "" {
			u.Range, u.OwnRange = rng, true
		} else if settings.DefaultRange != "" {
			u.Range, u.DefaultRange = settings.DefaultRange, true
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
	traffic := map[int]serverTraffic{}
	for _, p := range rows {
		if _, done := traffic[p.ServerID]; done {
			continue
		}
		if t, ok := h.readServerTraffic(r.Context(), p.ServerUUID); ok {
			traffic[p.ServerID] = t
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "ports": rows, "nodes": usage, "settings": settings, "traffic": traffic})
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
		httpx.SendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if req.Range != nil {
		*req.Range = strings.TrimSpace(*req.Range)
		if *req.Range != "" {
			if _, _, err := services.ParsePortRange(*req.Range); err != nil {
				httpx.SendJSONErrorCode(w, "The port range is invalid: "+err.Error()+".", "port_range_invalid", http.StatusBadRequest)
				return
			}
		}
	}
	if req.MaxPerUser != nil {
		v := strings.TrimSpace(*req.MaxPerUser)
		if v != "" && v != services.LimitUnlimited {
			if n, err := strconv.Atoi(v); err != nil || n < 0 {
				httpx.SendJSONError(w, `maxPerUser must be "", "unlimited" or a number of 0 or more`, http.StatusBadRequest)
				return
			}
		}
		*req.MaxPerUser = v
	}
	set := func(key, val string) bool {
		if err := h.state.Store.SetSetting(key, val); err != nil {
			log.Printf("ports admin: save %s: %v", key, err)
			httpx.SendJSONError(w, "Failed to save the settings", http.StatusInternalServerError)
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

// SetNodeRange PUT /api/admin/ports/nodes/{id}/range - {range, host?}; range
// "" goes back to the global range, host "" to the node's public IP, a host
// left out stays. Refused when the range overlaps the node's game ports.
func (h *PortsAdminHandler) SetNodeRange(w http.ResponseWriter, r *http.Request) {
	nodeID := httpx.MustAtoi(mux.Vars(r)["id"])
	node, err := h.state.Store.GetNodeByID(nodeID)
	if err != nil || node == nil {
		httpx.SendJSONError(w, "Node not found", http.StatusNotFound)
		return
	}
	var req struct {
		Range string  `json:"range"`
		Host  *string `json:"host"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	var host string
	if req.Host != nil {
		if host, err = normPortsHost(*req.Host); err != nil {
			httpx.SendJSONErrorCode(w, "The address is invalid: "+err.Error()+".", "ports_host_invalid", http.StatusBadRequest)
			return
		}
	}
	req.Range = strings.TrimSpace(req.Range)
	if req.Range != "" {
		if _, _, err := services.ParsePortRange(req.Range); err != nil {
			httpx.SendJSONErrorCode(w, "The port range is invalid: "+err.Error()+".", "port_range_invalid", http.StatusBadRequest)
			return
		}
		if hb := services.LoadHeartbeat(r.Context(), h.state.Redis, node.Token); hb != nil && services.PortRangesOverlap(req.Range, hb.PortRange) {
			httpx.SendJSONErrorCode(w, "The range overlaps this node's game port range "+hb.PortRange+". Pick one outside it.", "ports_range_conflict", http.StatusBadRequest)
			return
		}
	}
	if err := h.state.Store.SetNodeExtraPortRange(nodeID, req.Range); err != nil {
		log.Printf("ports admin: node %d range: %v", nodeID, err)
		httpx.SendJSONError(w, "Failed to save the range", http.StatusInternalServerError)
		return
	}
	if req.Host != nil {
		if err := h.state.Store.SetNodeExtraPortsHost(nodeID, host); err != nil {
			log.Printf("ports admin: node %d host: %v", nodeID, err)
			httpx.SendJSONError(w, "Failed to save the address", http.StatusInternalServerError)
			return
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// normPortsHost checks the address players use for a node's extra ports: ""
// (automatic), an IPv4 address or a DNS name. IPv6 is refused because Simple
// Voice Chat reads voice_host as host:port, which an unbracketed IPv6 breaks.
func normPortsHost(s string) (string, error) {
	v := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	if v == "" {
		return "", nil
	}
	if ip := net.ParseIP(v); ip != nil {
		if ip.To4() == nil {
			return "", errors.New("use an IPv4 address or a hostname with an AAAA record")
		}
		return v, nil
	}
	labels := strings.Split(v, ".")
	if _, err := strconv.Atoi(labels[len(labels)-1]); err == nil || !handlers.IsHostname(v, 2) {
		return "", errors.New("enter an IPv4 address or a hostname such as play.example.com")
	}
	return v, nil
}

// ListPermissions GET /api/admin/port-permissions
func (h *PortsAdminHandler) ListPermissions(w http.ResponseWriter, r *http.Request) {
	rows, err := h.state.Store.ListPortPermissions()
	if err != nil {
		httpx.SendJSONError(w, "Failed to read the port permissions", http.StatusInternalServerError)
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
		httpx.SendJSONError(w, `Invalid request: expected {"scope": "server:<id>|user:<id>|role:<id>", "allowed": true|false}`, http.StatusBadRequest)
		return
	}
	if err := h.state.Store.SetPortPermission(req.Scope, *req.Allowed); err != nil {
		httpx.SendJSONError(w, "Failed to save the port permission", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// DeletePermission DELETE /api/admin/port-permissions?scope=
func (h *PortsAdminHandler) DeletePermission(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if !services.ValidPortScope(scope, false) {
		httpx.SendJSONError(w, "Invalid scope", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.DeletePortPermission(scope); err != nil {
		httpx.SendJSONError(w, "Failed to delete the port permission", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// ListLimits GET /api/admin/port-limits
func (h *PortsAdminHandler) ListLimits(w http.ResponseWriter, r *http.Request) {
	rows, err := h.state.Store.ListPortLimits()
	if err != nil {
		httpx.SendJSONError(w, "Failed to read the port limits", http.StatusInternalServerError)
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
		httpx.SendJSONError(w, `Invalid request: expected {"scope": "user:<id>|role:<id>", "maxPorts": null|<n>}`, http.StatusBadRequest)
		return
	}
	var max *int
	if string(req.MaxPorts) != "null" {
		var n int
		if err := json.Unmarshal(req.MaxPorts, &n); err != nil || n < 0 {
			httpx.SendJSONError(w, "maxPorts must be null or a number of 0 or more", http.StatusBadRequest)
			return
		}
		max = &n
	}
	if err := h.state.Store.SetPortLimit(req.Scope, max); err != nil {
		httpx.SendJSONError(w, "Failed to save the port limit", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// DeleteLimit DELETE /api/admin/port-limits?scope=
func (h *PortsAdminHandler) DeleteLimit(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if !services.ValidPortScope(scope, true) {
		httpx.SendJSONError(w, "Invalid scope", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.DeletePortLimit(scope); err != nil {
		httpx.SendJSONError(w, "Failed to delete the port limit", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// serverTraffic is a server's whole network traffic as its node last counted
// it (node/net_traffic.go): every byte of the container, game port and extra
// ports together. Rx/Tx are since the container started, the rates over the
// node's last interval. Not per port: that would need NET_ADMIN on the host.
type serverTraffic struct {
	Rx    uint64 `json:"rx"`
	Tx    uint64 `json:"tx"`
	RxBps uint64 `json:"rxBps"`
	TxBps uint64 `json:"txBps"`
	At    int64  `json:"at"`
}

func (h *PortsAdminHandler) readServerTraffic(ctx context.Context, uuid string) (serverTraffic, bool) {
	if h.state.Redis == nil || uuid == "" {
		return serverTraffic{}, false
	}
	m, err := h.state.Redis.HGetAll(ctx, "dylaris:server:"+uuid+":net").Result()
	if err != nil || len(m) == 0 {
		return serverTraffic{}, false
	}
	u := func(k string) uint64 { v, _ := strconv.ParseUint(m[k], 10, 64); return v }
	at, _ := strconv.ParseInt(m["ts"], 10, 64)
	return serverTraffic{Rx: u("rx"), Tx: u("tx"), RxBps: u("rx_bps"), TxBps: u("tx_bps"), At: at}, true
}
