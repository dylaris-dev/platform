package packs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dylaris-core/handlers/httpx"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/storage/modpack"

	"github.com/gorilla/mux"
)

type replaceModrinthRequest struct {
	VersionID string `json:"versionId"` // the Modrinth version to align to
}

// ReplaceWithModrinth swaps a stored content artifact for a reference to
// Modrinth's exact file: the modversion is rewritten to the chosen version's
// primary file (URL and published hashes, a clean files[] reference that is
// auto-update eligible) and the old storage object is deleted. Nothing is
// downloaded or stored: a render fetches the file from cdn.modrinth.com.
func (h *PacksHandler) ReplaceWithModrinth(w http.ResponseWriter, r *http.Request) {
	build, owner, ok := h.loadBuild(r, packEdit)
	if !ok {
		httpx.SendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	// loadOwnedBuild does not gate frozen; replacing content on a frozen build
	// would break byte-identity of an already-published build, so reject here.
	if build.Frozen {
		httpx.SendJSONError(w, "Build is frozen", http.StatusConflict)
		return
	}
	mvID, _ := strconv.Atoi(mux.Vars(r)["modversionId"])
	mv, err := h.state.Store.GetModversion(mvID)
	if err != nil || mv == nil {
		httpx.SendJSONError(w, "Content not found", http.StatusNotFound)
		return
	}
	// GetModversion is a globally unscoped by-id lookup; loadOwnedBuild only proves
	// pack/build ownership, not that this modversion belongs to it. Without this
	// check a caller could pass another tenant's modversionId and repoint their
	// storage_key/hashes to this caller's storage path.
	if ok, _ := h.state.Store.IsModversionInBuild(build.ID, mvID); !ok {
		httpx.SendJSONError(w, "Content not found", http.StatusNotFound)
		return
	}
	var req replaceModrinthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.VersionID == "" {
		httpx.SendJSONError(w, "versionId required", http.StatusBadRequest)
		return
	}
	if err := h.swapModversionToModrinth(r.Context(), owner, build, mv, req.VersionID); err != nil {
		httpx.SendJSONError(w, err.Error(), http.StatusBadGateway)
		return
	}
	publishPackEvent(r, h.state, "pack_content.changed", build.PackID, map[string]interface{}{"buildId": build.ID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "modversionId": mv.ID, "linked": true})
}

// swapModversionToModrinth aligns a modversion to a specific Modrinth version:
// fetch the version, rewrite the modversion into a reference to its primary
// file, then delete the old object once nothing points at it. Shared by
// ReplaceWithModrinth and UpdateMods. Sets modrinth_latest_version_id to the
// aligned version and stamps modrinth_last_checked so "update available" clears.
func (h *PacksHandler) swapModversionToModrinth(ctx context.Context, ownerID string, build *models.PackBuild, mv *models.Modversion, versionID string) error {
	v, err := services.FetchModrinthVersion(versionID)
	if err != nil {
		return fmt.Errorf("modrinth version lookup failed: %w", err)
	}
	kind := models.ContentTypeMod
	if strings.HasPrefix(mv.TargetPath, "resourcepacks/") {
		kind = models.ContentTypeResourcepack
	}
	if strings.HasPrefix(mv.TargetPath, "shaderpacks/") {
		kind = models.ContentTypeShaderpack
	}
	if mv.ModrinthProjectID != "" && mv.ModrinthProjectID != v.ProjectID {
		return fmt.Errorf("the selected version belongs to another Modrinth project")
	}
	if err := validateModrinthContent(build, v, kind); err != nil {
		return err
	}
	// validateModrinthContent has proven the primary file is on the Modrinth CDN
	// with both hashes: exactly what a files[] reference needs. Without them an
	// entry with no stored bytes would silently leave every render.
	file := v.PrimaryFile()
	oldKey := mv.StorageKey

	now := time.Now()
	mv.Version = v.VersionNum
	mv.Filesize = file.Size
	mv.StorageKey = ""
	mv.MD5 = ""
	mv.SHA1 = file.Hashes["sha1"]
	mv.SHA512 = file.Hashes["sha512"]
	mv.ModrinthDownloadURL = file.URL
	mv.Source = models.SourceModrinth
	mv.ModrinthProjectID = v.ProjectID
	mv.ModrinthVersionID = v.ID
	mv.ModrinthVersionNumber = v.VersionNum
	mv.ModrinthGameVersions = strings.Join(v.GameVersions, ",")
	mv.ModrinthLatestVersionID = v.ID // aligned: current == latest-known -> clears "update available"
	mv.ModrinthLastChecked = &now
	if err := h.state.Store.UpdateModversion(mv); err != nil {
		return fmt.Errorf("failed to update content: %w", err)
	}
	if oldKey != "" {
		if prov, err := h.state.BuildModpackStorageProvider(); err == nil && prov != nil {
			h.deleteIfUnreferenced(ctx, prov, oldKey)
		}
	}
	return nil
}

// deleteIfUnreferenced removes a content object only once no modversion row
// points at it any more.
//
// The delete used to be unconditional, on the reasoning that "the DB no longer
// references it" - true of the row just rewritten, not of the object.
// MigrateBuild's copyUploadedContent deliberately creates a NEW modversion for
// the SAME storage key (so that updating one build cannot rewrite the other's
// row), which leaves two rows on one object. Updating either copy then deleted
// the file the other build still points at, and that build's render fails on a
// key that is simply gone.
//
// On a count error the object is LEFT in place: an orphan costs storage, a
// wrong delete costs someone's build.
func (h *PacksHandler) deleteIfUnreferenced(ctx context.Context, prov modpack.ModpackStorageProvider, key string) {
	if key == "" {
		return
	}
	n, err := h.state.Store.CountModversionsByStorageKey(key)
	if err != nil || n > 0 {
		return
	}
	_ = prov.Delete(ctx, key)
}
