package handlers

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"dylaris-core/services"
)

// slugify turns a display name into a URL-safe slug (lowercase, dashes for
// runs of non-alphanumerics, trimmed, max 64 chars). Shared by the pack
// builder handlers.
func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = regexp.MustCompile(`[^a-z0-9_-]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-_")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func IsAdmin(r *http.Request) bool {
	isAdmin, ok := r.Context().Value("isAdmin").(bool)
	return ok && isAdmin
}

// sendConnTestFailure answers a failed "Test connection" with the stage
// attached.
//
// The status stays 502 and `success` stays false, because that is what every
// existing caller of these endpoints already reads. What is new is `stage`:
// the panel words its heading from it, so "nothing answered on that address"
// and "answered, then refused" stop arriving as the same red box. See
// services/conncheck.go for why that distinction is the whole feature.
func sendConnTestFailure(w http.ResponseWriter, check services.ConnCheck) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"ok":      false,
		"stage":   check.Stage,
		"message": check.Message,
	})
}
