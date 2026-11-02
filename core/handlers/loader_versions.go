package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"dylaris-core/handlers/httpx"
	"dylaris-core/services/modcompat"
)

// GetLoaderVersions returns exact compatible loader builds, not Minecraft lines.
func (h *VersionHandler) GetLoaderVersions(w http.ResponseWriter, r *http.Request) {
	software, mc := r.URL.Query().Get("software"), r.URL.Query().Get("minecraft")
	if !modcompat.LoaderGameVersion.MatchString(mc) || strings.Contains(mc, "..") {
		httpx.SendJSONError(w, "A valid Minecraft version is required", http.StatusBadRequest)
		return
	}
	switch software {
	case "fabric", "quilt", "forge", "neoforge":
	default:
		httpx.SendJSONError(w, "Unknown mod loader", http.StatusBadRequest)
		return
	}
	key := "dylaris:loader-versions:" + software + ":" + mc
	if h.state.Redis != nil {
		if data, err := h.state.Redis.Get(r.Context(), key).Bytes(); err == nil && len(data) > 0 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)
			return
		}
	}
	versions, err := modcompat.FetchLoaderVersions(r.Context(), h.loaderHTTP, software, mc)
	if err != nil {
		httpx.SendJSONError(w, "Failed to fetch loader versions: "+err.Error(), http.StatusBadGateway)
		return
	}
	data, _ := json.Marshal(map[string]any{"success": true, "software": software, "minecraft": mc, "versions": versions})
	if h.state.Redis != nil {
		h.state.Redis.Set(r.Context(), key, data, 5*time.Minute)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}
