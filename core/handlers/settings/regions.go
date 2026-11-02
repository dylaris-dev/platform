package settings

import (
	"encoding/json"
	"net/http"

	"dylaris-core/handlers/httpx"
	"dylaris-core/models"
)

type RegionsHandler struct {
	d Deps
}

func NewRegionsHandler(d Deps) *RegionsHandler {
	return &RegionsHandler{d: d}
}

// ListRegions GET /api/regions - available to all authenticated users.
// Returns only enabled regions. Rows are created by node heartbeats
// (NODE_REGION); there is no admin CRUD.
func (h *RegionsHandler) ListRegions(w http.ResponseWriter, r *http.Request) {
	regions, err := h.d.Store.ListRegions(false)
	if err != nil {
		httpx.SendJSONError(w, "Failed to load regions", http.StatusInternalServerError)
		return
	}
	if regions == nil {
		regions = []models.Region{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"regions": regions,
	})
}
