package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MaintenanceState mirrors the maintenance.* settings keys. Default is "off"
// — fresh installs are never in maintenance mode.
//
// BlockLevel values:
//   - "off":         feature off entirely (banner hidden, no blocking)
//   - "banner_only": show banner, allow all traffic (advisory)
//   - "block_writes": block POST/PUT/PATCH/DELETE for non-admins (GETs ok)
//   - "block_all":   block everything for non-admins
//
// Admins are never blocked — otherwise they couldn't turn maintenance off.
type MaintenanceState struct {
	Active      bool   `json:"active"`
	Title       string `json:"title"`
	Message     string `json:"message"`
	ExpectedEnd string `json:"expectedEnd"` // ISO timestamp; "" if not set
	BlockLevel  string `json:"blockLevel"`
}

var defaultMaintenanceState = MaintenanceState{
	Active:      false,
	Title:       "Maintenance in progress",
	Message:     "We're performing scheduled maintenance and will be back shortly.",
	ExpectedEnd: "",
	BlockLevel:  "banner_only",
}

func validMaintenanceLevel(v string) bool {
	return v == "off" || v == "banner_only" || v == "block_writes" || v == "block_all"
}

// LoadMaintenanceState reads the current state from settings. Package-level
// helper so the middleware (and the public GET endpoint) share one path.
func LoadMaintenanceState(state *AppState) MaintenanceState {
	s := defaultMaintenanceState
	if v, _ := state.Store.GetSetting("maintenance.active"); v != "" {
		s.Active = v == "true"
	}
	if v, _ := state.Store.GetSetting("maintenance.title"); v != "" {
		s.Title = v
	}
	if v, _ := state.Store.GetSetting("maintenance.message"); v != "" {
		s.Message = v
	}
	if v, _ := state.Store.GetSetting("maintenance.expected_end"); v != "" {
		s.ExpectedEnd = v
	}
	if v, _ := state.Store.GetSetting("maintenance.block_level"); validMaintenanceLevel(v) {
		s.BlockLevel = v
	}
	return s
}

// maintenanceCache memoizes the maintenance state for the hot request path so
// the gate middleware doesn't do five settings reads on every API call. Short
// TTL so toggling maintenance takes effect within a few seconds.
var (
	maintCacheMu  sync.Mutex
	maintCache    MaintenanceState
	maintCacheSet bool
	maintCacheExp time.Time
)

func loadMaintenanceStateCached(state *AppState) MaintenanceState {
	maintCacheMu.Lock()
	defer maintCacheMu.Unlock()
	if maintCacheSet && time.Now().Before(maintCacheExp) {
		return maintCache
	}
	maintCache = LoadMaintenanceState(state)
	maintCacheSet = true
	maintCacheExp = time.Now().Add(5 * time.Second)
	return maintCache
}

type MaintenanceHandler struct {
	state *AppState
}

func NewMaintenanceHandler(state *AppState) *MaintenanceHandler {
	return &MaintenanceHandler{state: state}
}

// GetState GET /api/maintenance — public. Drives the frontend banner. Never
// returns admin-only metadata so an unauthenticated visitor can see the
// banner copy on the login page.
func (h *MaintenanceHandler) GetState(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"state":   LoadMaintenanceState(h.state),
	})
}

// SaveState PUT /api/admin/maintenance - PANEL settings.write (RequireCap at the route).
func (h *MaintenanceHandler) SaveState(w http.ResponseWriter, r *http.Request) {
	var req MaintenanceState
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if !validMaintenanceLevel(req.BlockLevel) {
		req.BlockLevel = "banner_only"
	}
	req.Title = strings.TrimSpace(req.Title)
	req.Message = strings.TrimSpace(req.Message)
	req.ExpectedEnd = strings.TrimSpace(req.ExpectedEnd)

	actorID, _ := r.Context().Value("userID").(string)
	pairs := []struct{ k, v string }{
		{"maintenance.active", fmt.Sprintf("%t", req.Active)},
		{"maintenance.title", req.Title},
		{"maintenance.message", req.Message},
		{"maintenance.expected_end", req.ExpectedEnd},
		{"maintenance.block_level", req.BlockLevel},
	}
	for _, kv := range pairs {
		if err := h.state.Store.SetSettingBy(kv.k, kv.v, actorID); err != nil {
			sendJSONError(w, "Failed to save: "+kv.k, http.StatusInternalServerError)
			return
		}
	}

	LogIdentityAudit(h.state, r, AuditEventMaintenanceToggled, actorID, "", map[string]interface{}{
		"active":       req.Active,
		"block_level":  req.BlockLevel,
		"expected_end": req.ExpectedEnd,
	})

	h.state.Events.Publish(r.Context(), "maintenance.changed", nil)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"state":   req,
	})
}

// MaintenanceMuxMiddleware is a gorilla/mux middleware (api.Use) that blocks
// traffic per block_level while maintenance is active. It runs before
// AuthMiddleware, so isAdminFn resolves admin status from the request token
// directly to honor the admin bypass. Returns 503 on a block — standard HTTP
// semantics for planned downtime, with an optional Retry-After.
func MaintenanceMuxMiddleware(state *AppState, isAdminFn func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !shouldBlockForMaintenance(state, r, isAdminFn(r)) {
				next.ServeHTTP(w, r)
				return
			}
			writeMaintenanceBlock(state, w)
		})
	}
}

// maintenanceExemptMachinePaths are the endpoints a customer's warp and link
// call with a kit key. Each is key-authenticated and does its own refusals.
var maintenanceExemptMachinePaths = map[string]bool{
	"/api/warp/enroll":         true,
	"/api/warp/assignment":     true,
	"/api/warp/link-boot":      true,
	"/api/warp/link/edges":     true,
	"/api/warp/link/heartbeat": true,
	"/api/warp/link/stats":     true,
}

// shouldBlockForMaintenance returns true when the request must be rejected
// because maintenance mode is on and the caller/method isn't on the pass-list.
// isAdmin is resolved by the caller — the mux middleware runs before
// AuthMiddleware, so it parses the token itself.
func shouldBlockForMaintenance(state *AppState, r *http.Request, isAdmin bool) bool {
	// Always allow the state endpoint, login + status — otherwise users
	// can't even see why they're being blocked.
	if r.URL.Path == "/api/maintenance" || r.URL.Path == "/api/auth/login" || r.URL.Path == "/api/status" {
		return false
	}
	// Machines, not people: a customer's warp and link authenticate with their
	// kit key and keep the player path up. Blocking them turned a maintenance
	// window into an outage - every route-only link went offline within 15s, a
	// link restarting in the window crash-looped, and past 24h every tunnel
	// token expired.
	if maintenanceExemptMachinePaths[r.URL.Path] {
		return false
	}
	s := loadMaintenanceStateCached(state)
	if !s.Active || s.BlockLevel == "off" || s.BlockLevel == "banner_only" {
		return false
	}
	// Admins are never blocked — they still need to manage the platform and
	// turn maintenance back off.
	if isAdmin {
		return false
	}
	if s.BlockLevel == "block_writes" {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			return false
		}
	}
	return true
}

// writeMaintenanceBlock emits the 503 response with a Retry-After hint when
// an expected end is configured.
func writeMaintenanceBlock(state *AppState, w http.ResponseWriter) {
	s := LoadMaintenanceState(state)
	w.Header().Set("Content-Type", "application/json")
	if s.ExpectedEnd != "" {
		if t, err := time.Parse(time.RFC3339, s.ExpectedEnd); err == nil {
			secs := int(time.Until(t).Seconds())
			if secs > 0 {
				w.Header().Set("Retry-After", fmt.Sprintf("%d", secs))
			}
		}
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     false,
		"maintenance": true,
		"title":       s.Title,
		"message":     s.Message,
		"expectedEnd": s.ExpectedEnd,
	})
}
