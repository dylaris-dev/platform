package gate

import (
	"encoding/json"
	"net/http"
	"strings"
)

// RequireSetupComplete wraps every /api/* route except /api/setup/*. When
// the platform is in Fresh-Install mode (0 users), every gated request gets
// 503 setup_required + X-Setup-Required: 1. In Lost-Admin mode (>=1 user,
// 0 admins) non-admin routes still work — the auth middleware handles the
// admin-only routes. In Complete mode this is a 0-cost pass-through after
// the CountAdmins check (~0.1ms).
//
// We deliberately do NOT cache CountAdmins. The value of "setup completes on
// Core A, Core B unlocks on the very next request" is worth the SELECT cost.
func (s Gates) RequireSetupComplete(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/setup/") {
			next(w, r)
			return
		}
		adminCount, _ := s.Store.CountAdmins()
		if adminCount > 0 {
			next(w, r)
			return
		}
		userCount, _ := s.Store.CountUsers()
		if userCount > 0 {
			// Lost-Admin: non-admin routes work, admin routes get 403
			// from AuthMiddleware as usual.
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
