package tabs

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"dylaris-core/handlers"
)

// TabProxySettingsHandler is the admin GET/PUT bundle for the WS5 custom-tab
// reverse-proxy toggles. Mirrors FeatureSettingsHandler.
type TabProxySettingsHandler struct {
	state *handlers.AppState
}

func NewTabProxySettingsHandler(state *handlers.AppState) *TabProxySettingsHandler {
	return &TabProxySettingsHandler{state: state}
}

type tabProxySettings struct {
	Enabled          bool `json:"enabled"`
	AllowPublicLinks bool `json:"allowPublicLinks"`
	// Who sees the Custom Tabs navbar entry: "admin" or "all". It is set here
	// rather than in Settings -> Modules because the module row is DERIVED from
	// it - see syncCustomTabsModule.
	Audience string `json:"audience"`
	// Both caps are per USER. Per server alone is not a ceiling - a user with
	// twenty servers would hold twenty times it - and a server-wide count on a
	// shared server lets whoever gets there first spend everyone's allowance.
	MaxPerUserPerServer  int `json:"maxPerUserPerServer"`
	MaxPerUserTotal      int `json:"maxPerUserTotal"`
	MaxShareLinksPerUser int `json:"maxShareLinksPerUser"`
}

// Get GET /api/admin/settings/tab-proxy - PANEL settings.read (RequireCap at the route).
func (h *TabProxySettingsHandler) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := tabProxySettings{
		Enabled:              h.state.FeatureFlags.IsTabProxyEnabled(ctx),
		AllowPublicLinks:     h.state.FeatureFlags.TabProxyAllowPublicLinks(ctx),
		MaxPerUserPerServer:  h.state.FeatureFlags.TabProxyMaxPerUserPerServer(ctx),
		MaxPerUserTotal:      h.state.FeatureFlags.TabProxyMaxPerUserTotal(ctx),
		MaxShareLinksPerUser: h.state.FeatureFlags.TabProxyMaxShareLinksPerUser(ctx),
		Audience:             h.state.FeatureFlags.TabProxyAudience(ctx),
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "settings": out})
}

// Set PUT /api/admin/settings/tab-proxy - PANEL settings.write (RequireCap at the route).
func (h *TabProxySettingsHandler) Set(w http.ResponseWriter, r *http.Request) {
	var req tabProxySettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.MaxPerUserPerServer <= 0 {
		req.MaxPerUserPerServer = 3
	}
	if req.MaxPerUserTotal <= 0 {
		req.MaxPerUserTotal = 10
	}
	// A total below the per-server allowance is unreachable by construction and
	// would read as a bug from the user's side ("it says 5 per server but stops
	// at 2"), so the total wins and the per-server figure is clamped to it.
	if req.MaxPerUserPerServer > req.MaxPerUserTotal {
		req.MaxPerUserPerServer = req.MaxPerUserTotal
	}
	if req.MaxShareLinksPerUser <= 0 {
		req.MaxShareLinksPerUser = 20
	}
	// An unknown value is corrected rather than refused: this field arrives from
	// a two-button control, so anything else is a caller bug, and widening the
	// audience by accident is the direction that matters.
	if req.Audience != "admin" && req.Audience != "all" {
		req.Audience = "all"
	}
	writes := []struct{ k, v string }{
		{"feature_tab_proxy_enabled", strconv.FormatBool(req.Enabled)},
		{"tab_proxy_allow_public_links", strconv.FormatBool(req.AllowPublicLinks)},
		{"tab_proxy_max_per_user_per_server", strconv.Itoa(req.MaxPerUserPerServer)},
		{"tab_proxy_max_per_user_total", strconv.Itoa(req.MaxPerUserTotal)},
		{"tab_proxy_max_share_links_per_user", strconv.Itoa(req.MaxShareLinksPerUser)},
		{"tab_proxy_audience", req.Audience},
	}
	for _, kv := range writes {
		if err := h.state.Store.SetSetting(kv.k, kv.v); err != nil {
			handlers.SendJSONError(w, "Save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		h.state.FeatureFlags.Invalidate(kv.k)
	}
	// The navbar row follows these settings; it is not separately editable.
	// Cosmetic, so a failure is logged rather than failing the save the operator
	// just made - same call shape as the Modpacks sync.
	if err := handlers.SyncCustomTabsModule(h.state, req.Enabled, req.Audience); err != nil {
		log.Printf("tab-proxy: syncing the Custom Tabs module row failed: %v", err)
	}
	h.state.Events.Publish(r.Context(), "features.changed", map[string]interface{}{
		"feature": "tabProxy",
		"enabled": req.Enabled,
	})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "settings": req})
}
