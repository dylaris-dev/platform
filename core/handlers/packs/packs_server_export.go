package packs

import (
	"errors"
	"log"
	"net/http"

	"dylaris-core/handlers"
	"dylaris-core/models"
)

// exportServerPack serves the same bounded, content-addressed archive as share
// downloads, after ExportMrpack has checked the caller's pack and build access.
// It does not create a public link or require the share-links feature.
func (h *PacksHandler) exportServerPack(w http.ResponseWriter, r *http.Request, pack *models.Pack, build *models.PackBuild, content []models.BuildContentEntry) {
	provider, err := h.state.BuildModpackStorageProvider()
	if err != nil || provider == nil {
		handlers.SendJSONError(w, "Storage unavailable", http.StatusInternalServerError)
		return
	}
	key, err := h.shareServerPackKey(r.Context(), provider, pack, build, content)
	if errors.Is(err, errServerPackBusy) {
		w.Header().Set("Retry-After", "60")
		handlers.SendJSONError(w, "The server archive is being prepared. Try the download again in a minute.", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		log.Printf("server export: render failed for build %d: %v", build.ID, err)
		handlers.SendJSONError(w, "Failed to prepare server archive", http.StatusInternalServerError)
		return
	}
	filename := pack.InternalSlug + "-" + build.VersionString + "-server.zip"
	if err := handlers.ServeModpackObject(w, r, provider, key, handlers.DeliverStream, "application/zip", filename); err != nil {
		log.Printf("server export: read failed for build %d: %v", build.ID, err)
		handlers.SendJSONError(w, "Failed to read server archive", http.StatusInternalServerError)
	}
}
