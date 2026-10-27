package packs

import (
	"dylaris-core/handlers"
	"encoding/json"
	"net/http"
	"strings"
)

// Folders stores empty authoring directories as metadata; file paths already
// imply their parent directories. No host filesystem path is exposed.
func (h *PacksHandler) Folders(w http.ResponseWriter, r *http.Request) {
	need := packRead
	if r.Method == http.MethodPost {
		need = packEdit
	} else if r.Method == http.MethodDelete {
		need = packDelete
	}
	build, _, ok := h.loadBuild(r, need)
	if !ok {
		handlers.SendJSONError(w, "Forbidden", 403)
		return
	}
	folders, err := h.state.Store.ListPackFolders(r.Context(), build.ID)
	if err != nil {
		handlers.SendJSONError(w, "Could not load folders", 500)
		return
	}
	if r.Method == http.MethodGet {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "folders": folders})
		return
	}
	if build.Frozen {
		handlers.SendJSONError(w, "Build is frozen", 409)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || !validContentTarget(req.Path) || len(req.Path) > 400 {
		handlers.SendJSONError(w, "Invalid relative folder path", 400)
		return
	}
	content, err := h.state.Store.ListBuildContent(build.ID)
	if err != nil {
		handlers.SendJSONError(w, "Could not inspect folder content", 500)
		return
	}
	for _, e := range content {
		if strings.EqualFold(e.TargetPath, req.Path) || r.Method == http.MethodDelete && strings.HasPrefix(e.TargetPath, req.Path+"/") {
			handlers.SendJSONError(w, "Path conflicts with a file or folder is not empty", 409)
			return
		}
	}
	if r.Method == http.MethodDelete {
		for _, folder := range folders {
			if strings.HasPrefix(folder, req.Path+"/") {
				handlers.SendJSONError(w, "Folder contains subfolders", 409)
				return
			}
		}
	}
	if err := h.state.Store.SetPackFolder(r.Context(), build.ID, req.Path, r.Method == http.MethodDelete); err != nil {
		handlers.SendJSONError(w, "Could not change folder", 409)
		return
	}
	publishPackEvent(r, h.state, "pack_content.changed", build.PackID, map[string]interface{}{"buildId": build.ID})
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
