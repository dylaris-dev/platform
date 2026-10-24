package packs

import (
	"encoding/json"
	"net/http"
	"strconv"

	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"
)

// BuildCompat GET /api/packs/{id}/builds/{buildId}/compat
func (h *PacksHandler) BuildCompat(w http.ResponseWriter, r *http.Request) {
	build, _, ok := h.loadBuild(r, packRead)
	if !ok {
		handlers.SendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	if build.Minecraft == "" || build.Loader == "" {
		handlers.SendJSONError(w, "This build has no Minecraft version or loader set, so nothing can be compared against it", http.StatusUnprocessableEntity)
		return
	}
	content, err := h.state.Store.ListBuildContent(build.ID)
	if err != nil {
		handlers.SendJSONError(w, "Failed to load build content", http.StatusInternalServerError)
		return
	}

	items := make([]services.CompatItem, 0, len(content))
	for _, e := range content {
		// Config bundles and other non-mod content have no Modrinth identity to
		// check and would only pad the unlinked count with things that are
		// meant to be local.
		if e.ContentType != models.ContentTypeMod && e.ModrinthProjectID == "" {
			continue
		}
		items = append(items, services.CompatItem{
			Key:                  strconv.Itoa(e.ID),
			ProjectID:            e.ModrinthProjectID,
			Title:                e.PrettyName,
			Slug:                 e.ModSlug,
			Side:                 e.Side,
			CurrentVersionID:     e.ModrinthVersionID,
			CurrentVersionNumber: e.Version,
		})
	}

	mode, specific := handlers.CompatQuery(r)
	targets, err := handlers.ResolveTargets(r.Context(), build.Minecraft, mode, specific)
	if err != nil {
		handlers.SendJSONError(w, "Could not read the Minecraft version list from Modrinth: "+err.Error(), http.StatusBadGateway)
		return
	}
	matrix, err := services.BuildMatrix(r.Context(), services.CompatRequest{
		Items:   items,
		Loader:  build.Loader,
		Current: build.Minecraft,
		Mode:    mode,
		Targets: targets,
		Cache:   h.state.AvailabilityCache(),
	})
	if err != nil {
		handlers.SendJSONError(w, "Availability check failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "matrix": matrix})
}
