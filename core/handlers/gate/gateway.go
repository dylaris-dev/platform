package gate

import (
	"dylaris-core/services"
	"encoding/json"
	"net/http"
)

// RequireGatewayEnabled blocks the request with 503 gateway_disabled when the
// platform routing mode is ip_port. Wrap gateway-route WRITE endpoints with
// this so they refuse instead of silently accepting no-op work that the
// (absent) gateway would never apply. Reads + DELETE stay ungated: reads are
// harmless and DELETE must work for cleanup after switching back to ip_port.
func (s Gates) RequireGatewayEnabled(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !services.GatewayRoutingEnabled(s.Store) {
			GatewayDisabledResponse(w)
			return
		}
		next(w, r)
	}
}

// RequireAutoMoveEnabled blocks the request with 503 auto_move_disabled unless
// the auto-move feature flag is on. It runs in either routing mode: without the
// gateway a moved server takes its new node's address.
func (s Gates) RequireAutoMoveEnabled(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.FeatureFlags.IsAutoMoveEnabled(r.Context()) {
			autoMoveDisabledResponse(w)
			return
		}
		next(w, r)
	}
}

// autoMoveDisabledResponse writes the canonical 503 + machine-readable body.
func autoMoveDisabledResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"error":   "auto_move_disabled",
		"message": "Auto-move is disabled; enable it in Settings → Features while gateway routing is active.",
	})
}

// GatewayDisabledResponse writes the canonical 503 + machine-readable body,
// mirroring FeatureDisabledResponse.
func GatewayDisabledResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"error":   "gateway_disabled",
		"message": "Gateway routing is disabled; switch routing mode to gateway or both to manage routes.",
	})
}
