package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
)

// RequireSetupComplete wraps every /api/* route except /api/setup/*. When
// the platform is in Fresh-Install mode (0 users), every gated request gets
// 503 setup_required + X-Setup-Required: 1. In Lost-Admin mode (>=1 user,
// 0 admins) non-admin routes still work — the auth middleware handles the
// admin-only routes. Both modes that pass need only "is there any user", so
// that is the one question asked: an EXISTS that stops at the first row. It
// used to be COUNT(*) of admins, then of users - two scans on every request.
//
// We deliberately do NOT cache it. The value of "setup completes on Core A,
// Core B unlocks on the very next request" is worth one indexed-cheap SELECT.
func (s *AppState) RequireSetupComplete(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/setup/") {
			next(w, r)
			return
		}
		// Complete, or Lost-Admin (non-admin routes work, admin routes get
		// 403 from AuthMiddleware as usual). An error locks, as before.
		if hasUsers, _ := s.Store.HasUsers(); hasUsers {
			next(w, r)
			return
		}
		// Fresh-Install — hard lock.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Setup-Required", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "setup_required",
			"message": "Platform setup not complete. Open /setup in the panel.",
		})
	}
}
