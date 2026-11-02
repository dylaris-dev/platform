package packs

import (
	"encoding/json"
	"net/http"
	"strconv"

	"dylaris-core/handlers/httpx"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/modcompat"
)

// BuildCompat GET /api/packs/{id}/builds/{buildId}/compat
func (h *PacksHandler) BuildCompat(w http.ResponseWriter, r *http.Request) {
	build, _, ok := h.loadBuild(r, packRead)
	if !ok {
		httpx.SendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	if build.Minecraft == "" || build.Loader == "" {
		httpx.SendJSONError(w, "This build has no Minecraft version or loader set, so nothing can be compared against it", http.StatusUnprocessableEntity)
		return
	}
	content, err := h.d.Store.ListBuildContent(build.ID)
	if err != nil {
		httpx.SendJSONError(w, "Failed to load build content", http.StatusInternalServerError)
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
			ContentType:          e.ContentType,
			Key:                  strconv.Itoa(e.ID),
			ProjectID:            e.ModrinthProjectID,
			Title:                e.PrettyName,
			Slug:                 e.ModSlug,
			Side:                 e.Side,
			CurrentVersionID:     e.ModrinthVersionID,
			CurrentVersionNumber: e.Version,
		})
	}

	mode, specific := modcompat.Query(r)
	loader := r.URL.Query().Get("loader")
	if loader == "" {
		loader = build.Loader
	}
	if loader != "fabric" && loader != "forge" && loader != "neoforge" && loader != "quilt" && loader != "vanilla" {
		httpx.SendJSONError(w, "Unsupported target loader", 400)
		return
	}
	targets, err := modcompat.ResolveTargets(r.Context(), build.Minecraft, mode, specific)
	if err != nil {
		httpx.SendJSONError(w, "Could not read the Minecraft version list from Modrinth: "+err.Error(), http.StatusBadGateway)
		return
	}
	if mode == services.CompatModeSpecific && specific == build.Minecraft {
		targets = []string{specific}
	}
	matrix, err := services.BuildMatrix(r.Context(), services.CompatRequest{
		Items:   items,
		Loader:  loader,
		Current: build.Minecraft,
		Mode:    mode,
		Targets: targets,
		Cache:   modcompat.AvailabilityCache(h.d.Cache),
	})
	if err != nil {
		httpx.SendJSONError(w, "Availability check failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "matrix": matrix})
}
