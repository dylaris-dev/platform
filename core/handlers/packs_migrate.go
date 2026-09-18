package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"dylaris-core/models"
	"dylaris-core/services"
)

// Migrating a build to another Minecraft version.
//
// This always produces a NEW build and never touches the source. A published
// build is Frozen and could not be edited in place at all, a pack is versioned
// through builds anyway, and rewriting the source would destroy the very thing
// the new version is being compared against.

type migrateBuildRequest struct {
	Minecraft     string `json:"minecraft"`
	VersionString string `json:"versionString"`
	LoaderVersion string `json:"loaderVersion"`
	// DropUnavailable has to be set explicitly to create a build that is
	// missing content the source had. Without it, a build that cannot be
	// reproduced in full is refused and the missing items are named.
	DropUnavailable bool   `json:"dropUnavailable"`
	Changelog       string `json:"changelog"`
}

type migratedItem struct {
	ModversionID int    `json:"modversionId"`
	Title        string `json:"title"`
	Version      string `json:"version,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// MigrateBuild POST /api/packs/{id}/builds/{buildId}/migrate
func (h *PacksHandler) MigrateBuild(w http.ResponseWriter, r *http.Request) {
	source, ok := h.loadOwnedBuild(r)
	if !ok {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	var req migrateBuildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.Minecraft = strings.TrimSpace(req.Minecraft)
	req.VersionString = strings.TrimSpace(req.VersionString)
	if req.Minecraft == "" || req.VersionString == "" {
		sendJSONError(w, "minecraft and versionString are required", http.StatusBadRequest)
		return
	}
	if req.Minecraft == source.Minecraft {
		sendJSONError(w, "The target Minecraft version is the one this build already uses", http.StatusBadRequest)
		return
	}
	// VersionString feeds the download filename and the node-side install paths:
	// the same constraint CreateBuild enforces.
	if !safeKeyComponent(req.VersionString) {
		sendJSONError(w, "versionString contains invalid path characters", http.StatusBadRequest)
		return
	}
	if source.Loader == "" {
		sendJSONError(w, "This build has no loader set, so its content cannot be resolved against another Minecraft version", http.StatusUnprocessableEntity)
		return
	}

	content, err := h.state.Store.ListBuildContent(source.ID)
	if err != nil {
		sendJSONError(w, "Failed to load build content", http.StatusInternalServerError)
		return
	}

	resolved, unavailable, uploads, err := h.resolveMigrationTargets(content, source.Loader, req.Minecraft)
	if err != nil {
		sendJSONError(w, "Could not resolve target versions from Modrinth: "+err.Error(), http.StatusBadGateway)
		return
	}
	if len(unavailable) > 0 && !req.DropUnavailable {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":     false,
			"message":     fmt.Sprintf("%d of this build's mods have no version for Minecraft %s. Set dropUnavailable to create the build without them.", len(unavailable), req.Minecraft),
			"unavailable": unavailable,
			"uploads":     uploads,
		})
		return
	}

	userID, _ := r.Context().Value("userID").(string)
	// LoaderVersion is deliberately allowed to stay empty: the render path
	// resolves an empty loader version to latest-stable for the target
	// Minecraft version, which is what a migration wants.
	target := &models.PackBuild{
		PackID:        source.PackID,
		VersionString: req.VersionString,
		Minecraft:     req.Minecraft,
		Loader:        source.Loader,
		LoaderVersion: strings.TrimSpace(req.LoaderVersion),
		MinJava:       source.MinJava,
		MinMemory:     source.MinMemory,
		Changelog:     req.Changelog,
		Channel:       models.ChannelDraft,
	}
	newID, err := h.state.Store.CreatePackBuild(target)
	if err != nil {
		sendJSONError(w, "Failed to create the target build (that version string may already exist)", http.StatusConflict)
		return
	}
	target.ID = newID

	migrated := make([]migratedItem, 0, len(resolved))
	failed := make([]migratedItem, 0)
	for _, m := range resolved {
		if err := h.addModrinthVersion(userID, target, m.version, m.entry.Side, m.entry.ContentType); err != nil {
			failed = append(failed, migratedItem{
				ModversionID: m.entry.ID,
				Title:        m.entry.PrettyName,
				Reason:       err.Error(),
			})
			continue
		}
		migrated = append(migrated, migratedItem{
			ModversionID: m.entry.ID,
			Title:        m.entry.PrettyName,
			Version:      m.version.VersionNum,
		})
	}

	// Manual uploads are copied verbatim: nothing is known about whether they
	// work on the target version, and dropping them silently would lose content
	// the author put there deliberately. They stay reported so the UI can repeat
	// the warning against the new build.
	copiedUploads := make([]migratedItem, 0, len(uploads))
	for _, e := range uploads {
		mvID, err := h.copyUploadedContent(userID, e, target)
		if err != nil {
			failed = append(failed, migratedItem{ModversionID: e.ID, Title: e.PrettyName, Reason: err.Error()})
			continue
		}
		copiedUploads = append(copiedUploads, migratedItem{ModversionID: mvID, Title: e.PrettyName, Version: e.Version})
	}

	h.state.Events.Publish(r.Context(), "pack_builds.changed", map[string]interface{}{"packId": source.PackID})

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"build":       target,
		"migrated":    migrated,
		"uploads":     copiedUploads,
		"unavailable": unavailable,
		"failed":      failed,
	})
}

type resolvedMigration struct {
	entry   models.BuildContentEntry
	version *services.ModrinthVersion
}

// resolveMigrationTargets finds, for every linked entry, the Modrinth version
// that matches the target Minecraft version and this build's loader.
//
// Resolution goes through the batch hash endpoint (one request per 100 entries)
// keyed on the file sha1 already stored on each modversion. An entry whose hash
// Modrinth does not know falls back to the project's own version list, which is
// the case for content added before hashes were recorded.
func (h *PacksHandler) resolveMigrationTargets(content []models.BuildContentEntry, loader, targetMC string) (
	resolved []resolvedMigration, unavailable []migratedItem, uploads []models.BuildContentEntry, err error,
) {
	linked := make([]models.BuildContentEntry, 0, len(content))
	for _, e := range content {
		if e.ModrinthProjectID == "" {
			uploads = append(uploads, e)
			continue
		}
		linked = append(linked, e)
	}

	byHash := map[string]services.ModrinthVersion{}
	hashes := make([]string, 0, len(linked))
	for _, e := range linked {
		if e.SHA1 != "" {
			hashes = append(hashes, e.SHA1)
		}
	}
	for start := 0; start < len(hashes); start += 100 {
		end := start + 100
		if end > len(hashes) {
			end = len(hashes)
		}
		res, e := services.CheckLatestVersions(hashes[start:end], "sha1", []string{loader}, []string{targetMC})
		if e != nil {
			return nil, nil, nil, e
		}
		for k, v := range res {
			byHash[k] = v
		}
	}

	for _, entry := range linked {
		if v, ok := byHash[entry.SHA1]; ok && v.ID != "" {
			version := v
			resolved = append(resolved, resolvedMigration{entry: entry, version: &version})
			continue
		}
		// Fallback for an entry whose stored hash Modrinth cannot place.
		v, e := services.LatestProjectVersionFor(entry.ModrinthProjectID, targetMC, loader)
		if e == nil && v != nil {
			resolved = append(resolved, resolvedMigration{entry: entry, version: v})
			continue
		}
		unavailable = append(unavailable, migratedItem{
			ModversionID: entry.ID,
			Title:        entry.PrettyName,
			Version:      entry.Version,
			Reason:       "no version for Minecraft " + targetMC + " on " + loader,
		})
	}
	return resolved, unavailable, uploads, nil
}

// copyUploadedContent attaches a manual upload to the target build.
//
// It creates a NEW modversion row pointing at the same stored object rather
// than re-attaching the source row. swapModversionToModrinth rewrites a
// modversion in place, so a shared row would let a later update of the new
// build silently rewrite the source build's content too.
//
// The storage object itself is shared. Nothing deletes a modpack content object
// except a replace/update, and those now always write to a fresh key first.
func (h *PacksHandler) copyUploadedContent(ownerID string, e models.BuildContentEntry, target *models.PackBuild) (int, error) {
	modID, err := h.state.Store.UpsertMod(&models.Mod{
		OwnerID:     ownerID,
		Slug:        e.ModSlug,
		PrettyName:  e.PrettyName,
		ContentType: e.ContentType,
	})
	if err != nil {
		return 0, err
	}
	mv := e.Modversion
	mv.ID = 0
	mv.ModID = modID
	mvID, err := h.state.Store.CreateModversion(&mv)
	if err != nil {
		return 0, err
	}
	if _, err := h.state.Store.AttachModversionToBuild(target.ID, mvID, e.Side); err != nil {
		return 0, err
	}
	return mvID, nil
}
