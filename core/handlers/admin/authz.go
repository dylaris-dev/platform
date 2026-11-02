package admin

import (
	"encoding/json"
	"net/http"

	"dylaris-core/authz"
)

// AuthzHandler serves the read-only capability catalog. It is stateless: the
// catalog is a compile-time code registry, so no AppState/store is needed.
type AuthzHandler struct{}

func NewAuthzHandler() *AuthzHandler { return &AuthzHandler{} }

// Catalog GET /api/authz/catalog - returns the capability catalog grouped by
// scope then category, so the panel role editor and simple/advanced UI render
// entirely from the backend list (no hard-coded frontend permission arrays).
func (h *AuthzHandler) Catalog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"catalog": authz.Grouped(),
	})
}

// Presets GET /api/authz/presets - the simple-mode preset bundles, so the
// assign-only UI renders from the backend (no hard-coded frontend presets).
func (h *AuthzHandler) Presets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"presets": authz.Presets(),
	})
}
