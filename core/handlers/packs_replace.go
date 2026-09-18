package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/storage/modpack"

	"github.com/gorilla/mux"
)

type replaceModrinthRequest struct {
	VersionID string `json:"versionId"` // the Modrinth version to align to
}

// ReplaceWithModrinth swaps a stored content artifact for Modrinth's exact file:
// download the chosen Modrinth version's primary file, verify its bytes against
// Modrinth's published hashes, re-wrap + store it, delete the old storage object,
// and rewrite the modversion so it becomes byte-identical to Modrinth (clean
// files[] reference + auto-update eligible).
func (h *PacksHandler) ReplaceWithModrinth(w http.ResponseWriter, r *http.Request) {
	build, ok := h.loadOwnedBuild(r)
	if !ok {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	// loadOwnedBuild does not gate frozen; replacing content on a frozen build
	// would break byte-identity of an already-published build, so reject here.
	if build.Frozen {
		sendJSONError(w, "Build is frozen", http.StatusConflict)
		return
	}
	mvID, _ := strconv.Atoi(mux.Vars(r)["modversionId"])
	mv, err := h.state.Store.GetModversion(mvID)
	if err != nil || mv == nil {
		sendJSONError(w, "Content not found", http.StatusNotFound)
		return
	}
	// GetModversion is a globally unscoped by-id lookup; loadOwnedBuild only proves
	// pack/build ownership, not that this modversion belongs to it. Without this
	// check a caller could pass another tenant's modversionId and repoint their
	// storage_key/hashes to this caller's storage path.
	if ok, _ := h.state.Store.IsModversionInBuild(build.ID, mvID); !ok {
		sendJSONError(w, "Content not found", http.StatusNotFound)
		return
	}
	var req replaceModrinthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.VersionID == "" {
		sendJSONError(w, "versionId required", http.StatusBadRequest)
		return
	}
	userID, _ := r.Context().Value("userID").(string)
	if err := h.swapModversionToModrinth(r.Context(), userID, mv, req.VersionID); err != nil {
		sendJSONError(w, err.Error(), http.StatusBadGateway)
		return
	}
	h.state.Events.Publish(r.Context(), "pack_content.changed", map[string]interface{}{"buildId": build.ID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "modversionId": mv.ID, "linked": true})
}

// swapModversionToModrinth aligns a modversion to a specific Modrinth version:
// fetch the version, download + integrity-verify its primary file against
// Modrinth's published hashes, re-wrap + store under a fresh key, rewrite the
// modversion refs, then delete the old object (safe-cutover ordering). Shared by
// ReplaceWithModrinth and UpdateMods. Sets modrinth_latest_version_id to the
// aligned version and stamps modrinth_last_checked so "update available" clears.
func (h *PacksHandler) swapModversionToModrinth(ctx context.Context, ownerID string, mv *models.Modversion, versionID string) error {
	v, err := services.FetchModrinthVersion(versionID)
	if err != nil {
		return fmt.Errorf("modrinth version lookup failed: %w", err)
	}
	file := v.PrimaryFile()
	if file.URL == "" {
		return fmt.Errorf("modrinth version has no downloadable file")
	}
	// At least one published hash, checked BEFORE the download: this function's
	// whole promise is that the stored artifact becomes byte-identical to
	// Modrinth's, and DownloadModrinthJar verifies only the hashes it is handed
	// - two empty strings would make it accept whatever arrived and call it
	// aligned.
	if file.Hashes["sha1"] == "" && file.Hashes["sha512"] == "" {
		return fmt.Errorf("modrinth version publishes no sha1/sha512 to verify against")
	}
	// services.DownloadModrinthJar rather than a bare client: it refuses any URL
	// outside cdn.modrinth.com, caps the body, and verifies the hashes. The three
	// other places that fetch a Modrinth file URL already go through it or its
	// streaming twin; this one used its own client with no host check at all, so
	// whatever URL the Modrinth API named in files[].url was fetched by Core from
	// inside the network. That allowlist exists precisely so a download cannot be
	// steered by a third party's response.
	jar, err := services.DownloadModrinthJar(ctx, file.URL, file.Hashes["sha1"], file.Hashes["sha512"])
	if err != nil {
		return fmt.Errorf("download from modrinth failed: %w", err)
	}
	fileName := file.Filename
	if fileName == "" {
		fileName = mv.TargetPath[strings.LastIndex(mv.TargetPath, "/")+1:]
	}
	zipBytes, err := modpack.WrapJarAsContentZip(fileName, jar)
	if err != nil {
		return fmt.Errorf("wrap failed: %w", err)
	}
	md5hex, _, _ := modpack.Hashes(zipBytes)
	prov, err := h.state.buildModpackStorageProvider()
	if err != nil || prov == nil {
		return fmt.Errorf("no pack storage configured")
	}
	slug := slugify(strings.TrimSuffix(fileName, ".jar"))
	// Remote text must never reach a storage key. version_number is whatever the
	// project author typed on Modrinth, and it went into the key raw. It is
	// slugified for the key and kept raw only for the display field (mv.Version
	// below). The md5 suffix is what keeps two
	// different artifacts from colliding once the slug has flattened them.
	keyVersion := slugify(v.VersionNum)
	if keyVersion == "" {
		keyVersion = "v"
	}
	keyVersion = keyVersion + "-" + md5hex[:8]
	newKey := "packs/" + ownerID + "/mods/" + slug + "/" + slug + "-" + keyVersion + ".zip"
	if err := prov.Put(ctx, newKey, zipBytes); err != nil {
		return fmt.Errorf("storage put failed: %w", err)
	}
	oldKey := mv.StorageKey

	now := time.Now()
	mv.Version = v.VersionNum
	mv.Filesize = int64(len(zipBytes))
	mv.StorageKey = newKey
	mv.MD5 = md5hex
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
	if oldKey != "" && oldKey != newKey {
		h.deleteIfUnreferenced(ctx, prov, oldKey)
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
