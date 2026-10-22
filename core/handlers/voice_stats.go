package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"

	"dylaris-core/services"
	"dylaris-core/store"
)

// VoiceStatsHandler serves the admin voice-chat overview. Admin-gated
// (settings.read) at the route.
type VoiceStatsHandler struct {
	state *AppState
}

func NewVoiceStatsHandler(state *AppState) *VoiceStatsHandler {
	return &VoiceStatsHandler{state: state}
}

// VoiceOverview is voice chat per region and per server for one period.
type VoiceOverview struct {
	Period  string                `json:"period"`
	Relays  []services.VoiceRelay `json:"relays"`
	Regions []VoiceRegionStat     `json:"regions"`
	Servers []VoiceServerStat     `json:"servers"`
}

// VoiceRegionStat sums one region. PortsUsed counts allocated ports, not
// players: a server holds its port while voice is on, connected or not.
type VoiceRegionStat struct {
	Region     string `json:"region"`
	Relays     int    `json:"relays"`
	PortsUsed  int    `json:"portsUsed"`
	PortsTotal int    `json:"portsTotal"`
	RxBytes    int64  `json:"rxBytes"`
	TxBytes    int64  `json:"txBytes"`
}

// VoiceServerStat is one server in one region. Port and Mod are empty for a
// server that moved bytes this period but no longer holds a port there.
type VoiceServerStat struct {
	ServerID   int    `json:"serverId"`
	ServerName string `json:"serverName"`
	Region     string `json:"region"`
	Mod        string `json:"mod"`
	Port       int    `json:"port"`
	RxBytes    int64  `json:"rxBytes"`
	TxBytes    int64  `json:"txBytes"`
}

// GetOverview GET /api/voice/overview?period=YYYY-MM
// Live relays, port use per region, and voice bytes per region and server for
// the period (default: the current month).
func (h *VoiceStatsHandler) GetOverview(w http.ResponseWriter, r *http.Request) {
	if h.state.Redis == nil {
		sendJSONError(w, "Redis is not configured", http.StatusServiceUnavailable)
		return
	}
	period := usagePeriod(r)
	ports, err := h.state.Store.ListVoicePorts()
	if err != nil {
		log.Printf("[voice] overview: list ports: %v", err)
		sendJSONError(w, "Lookup failed", http.StatusInternalServerError)
		return
	}
	usage, err := h.state.Store.ListVoiceUsage(period)
	if err != nil {
		log.Printf("[voice] overview: list usage: %v", err)
		sendJSONError(w, "Lookup failed", http.StatusInternalServerError)
		return
	}
	relays, err := services.LiveVoiceRelays(r.Context(), h.state.Redis)
	if err != nil {
		log.Printf("[voice] overview: %v", err)
		sendJSONError(w, "Lookup failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(buildVoiceOverview(period.Format("2006-01"), relays, ports, usage))
}

// buildVoiceOverview joins ports and usage on (server, region). A region
// appears when it has a relay, a port or usage; servers are listed busiest
// first, as the store returns usage, then idle port holders.
func buildVoiceOverview(period string, relays []services.VoiceRelay, ports []store.VoicePort, usage []store.VoiceUsageRow) VoiceOverview {
	type key struct {
		id     int
		region string
	}
	regions := map[string]*VoiceRegionStat{}
	region := func(name string) *VoiceRegionStat {
		if rs, ok := regions[name]; ok {
			return rs
		}
		rs := &VoiceRegionStat{Region: name, PortsTotal: services.VoicePortMax - services.VoicePortMin + 1}
		regions[name] = rs
		return rs
	}
	for _, rl := range relays {
		region(rl.Region).Relays++
	}

	portAt := map[key]store.VoicePort{}
	for _, p := range ports {
		region(p.Region).PortsUsed++
		portAt[key{p.ServerID, p.Region}] = p
	}

	out := VoiceOverview{Period: period, Relays: relays, Regions: []VoiceRegionStat{}, Servers: []VoiceServerStat{}}
	if out.Relays == nil {
		out.Relays = []services.VoiceRelay{}
	}
	listed := map[key]bool{}
	for _, u := range usage {
		k := key{u.ServerID, u.Region}
		listed[k] = true
		rs := region(u.Region)
		rs.RxBytes += u.RxBytes
		rs.TxBytes += u.TxBytes
		p := portAt[k]
		out.Servers = append(out.Servers, VoiceServerStat{
			ServerID: u.ServerID, ServerName: u.ServerName, Region: u.Region,
			Mod: p.Mod, Port: p.Port, RxBytes: u.RxBytes, TxBytes: u.TxBytes,
		})
	}
	// Port holders with no usage yet.
	for _, p := range ports {
		if listed[key{p.ServerID, p.Region}] {
			continue
		}
		out.Servers = append(out.Servers, VoiceServerStat{
			ServerID: p.ServerID, ServerName: p.ServerName, Region: p.Region, Mod: p.Mod, Port: p.Port,
		})
	}

	for _, rs := range regions {
		out.Regions = append(out.Regions, *rs)
	}
	sort.Slice(out.Regions, func(i, j int) bool { return out.Regions[i].Region < out.Regions[j].Region })
	return out
}
