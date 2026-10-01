package handlers

import (
	"encoding/json"
	"net/http"
)

type updateModsRequest struct {
	ModversionID int    `json:"modversionId"` // 0 = not a single-mod request
	VersionID    string `json:"versionId"`    // optional; empty = upgrade to cached latest
	All          bool   `json:"all"`          // upgrade every linked mod with an available update
}

type updateModsResult struct {
	ModversionID int    `json:"modversionId"`
	Error        string `json:"error,omitempty"`
}

// UpdateMods upgrades Modrinth-linked mods in a DRAFT build to a newer version.
// Single mode ({modversionId[, versionId]}) upgrades one mod (to versionId, or to
// its cached latest when versionId is empty). Batch mode ({all:true}) upgrades
// every linked mod whose cached latest differs from its current version. Reuses
// the shared safe-cutover align (swapModversionToModrinth). Per-mod failures are
// reported, not fatal.
func (h *PacksHandler) UpdateMods(w http.ResponseWriter, r *http.Request) {
	build, ok := h.loadOwnedBuild(r)
	if !ok {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	if build.Frozen {
		sendJSONError(w, "Build is frozen", http.StatusConflict)
		return
	}
	var req updateModsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	userID, _ := r.Context().Value("userID").(string)

	type job struct {
		mvID   int
		target string
	}
	var jobs []job

	if req.All {
		content, err := h.state.Store.ListBuildContent(build.ID)
		if err != nil {
			sendJSONError(w, "Failed to load build content", http.StatusInternalServerError)
			return
		}
		for _, e := range content {
			if e.ModrinthProjectID != "" && e.ModrinthLatestVersionID != "" && e.ModrinthLatestVersionID != e.ModrinthVersionID {
				jobs = append(jobs, job{mvID: e.ID, target: e.ModrinthLatestVersionID})
			}
		}
	} else {
		if req.ModversionID == 0 {
			sendJSONError(w, "modversionId or all required", http.StatusBadRequest)
			return
		}
		mv, err := h.state.Store.GetModversion(req.ModversionID)
		if err != nil || mv == nil {
			sendJSONError(w, "Content not found", http.StatusNotFound)
			return
		}
		// GetModversion is a globally unscoped by-id lookup; loadOwnedBuild only
		// proves pack/build ownership, not that this modversion belongs to it.
		// Without this check a caller could pass another tenant's modversionId and
		// repoint their storage_key/hashes to this caller's storage path. Batch
		// mode is already safe (it iterates ListBuildContent(build.ID)).
		if ok, _ := h.state.Store.IsModversionInBuild(build.ID, req.ModversionID); !ok {
			sendJSONError(w, "Content not found", http.StatusNotFound)
			return
		}
		if mv.ModrinthProjectID == "" {
			sendJSONError(w, "Content is not linked to Modrinth", http.StatusUnprocessableEntity)
			return
		}
		target := req.VersionID
		if target == "" {
			target = mv.ModrinthLatestVersionID
		}
		if target == "" {
			sendJSONError(w, "No newer version known for this content", http.StatusUnprocessableEntity)
			return
		}
		jobs = append(jobs, job{mvID: mv.ID, target: target})
	}

	results := make([]updateModsResult, 0, len(jobs))
	upgraded := 0
	for _, j := range jobs {
		mv, err := h.state.Store.GetModversion(j.mvID)
		if err != nil || mv == nil {
			results = append(results, updateModsResult{ModversionID: j.mvID, Error: "content not found"})
			continue
		}
		if err := h.swapModversionToModrinth(r.Context(), userID, mv, j.target); err != nil {
			results = append(results, updateModsResult{ModversionID: j.mvID, Error: err.Error()})
			continue
		}
		upgraded++
		results = append(results, updateModsResult{ModversionID: j.mvID})
	}

	if upgraded > 0 {
		publishPackEvent(r, h.state, "pack_content.changed", build.PackID, map[string]interface{}{"buildId": build.ID})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "upgraded": upgraded, "results": results})
}
