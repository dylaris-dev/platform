package packs

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/storage/modpack"

	"github.com/gorilla/mux"
)

// maxPackContentUploadBytes bounds one uploaded pack artifact. A mod jar, a
// plugin or a resource pack is comfortably under this; the number exists to put
// a ceiling on what an ordinary pack owner can write to Core's disk, not to
// express a product limit. Deliberately a constant rather than a new setting -
// there is no modpack upload setting today, and reusing the file manager's
// (fm.user_upload_limit) would couple two unrelated features.
const maxPackContentUploadBytes = 1 << 30 // 1 GiB

// loadBuild returns the build in the path if the caller may do need on its
// pack, and the pack's owner: content added by a collaborator belongs to the
// pack, so it is stored under the owner. Frozen state is left for callers to
// gate on (list/read is allowed on frozen builds; writes are not).
func (h *PacksHandler) loadBuild(r *http.Request, need packNeed) (*models.PackBuild, string, bool) {
	packID, _ := strconv.Atoi(mux.Vars(r)["id"])
	buildID, _ := strconv.Atoi(mux.Vars(r)["buildId"])
	p, ok := h.packAccess(r, packID, need)
	if !ok {
		return nil, "", false
	}
	b, err := h.state.Store.GetPackBuild(buildID)
	if err != nil || b == nil || b.PackID != packID {
		return nil, "", false
	}
	return b, p.OwnerID, true
}

// ListContent GET /api/packs/{id}/builds/{buildId}/content - the mods and
// files in one build. Owning the pack in the path says nothing about the build
// in it, so the build is bound to the pack here too: without that a caller
// could pair their own packId with another tenant's buildId and read that
// build's content, storage keys included.
func (h *PacksHandler) ListContent(w http.ResponseWriter, r *http.Request) {
	packID := atoiVar(r, "id")
	if _, ok := h.ownsPack(r, packID); !ok {
		handlers.SendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	// Owning the pack in the path says nothing about the build in it. Without
	// this binding a caller could pair their own packId with any other tenant's
	// buildId and read that build's content, storage keys included. Every other
	// build-scoped route already carries it, via loadOwnedBuild or explicitly
	// like ExportMrpack; this one was the exception.
	buildID, _ := strconv.Atoi(mux.Vars(r)["buildId"])
	b, err := h.state.Store.GetPackBuild(buildID)
	if err != nil || b == nil || b.PackID != packID {
		handlers.SendJSONError(w, "Build not found", http.StatusNotFound)
		return
	}
	entries, err := h.state.Store.ListBuildContent(buildID)
	if err != nil {
		handlers.SendJSONError(w, "Failed to list content", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "content": entries})
}

type addModrinthRequest struct {
	ProjectID   string `json:"projectId"`
	VersionID   string `json:"versionId"`
	Side        string `json:"side"`
	ResolveDeps bool   `json:"resolveDeps"`
	ContentType string `json:"contentType"` // mod|resourcepack|shaderpack
}

// AddModrinth POST /api/packs/{id}/builds/{buildId}/content/modrinth - adds a
// Modrinth version to a build and, on request, its dependencies. A frozen
// build is 409, and the reply counts what was actually added, since a
// dependency that fails is skipped rather than aborting the whole call.
func (h *PacksHandler) AddModrinth(w http.ResponseWriter, r *http.Request) {
	b, owner, ok := h.loadBuild(r, packEdit)
	if !ok {
		handlers.SendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	if b.Frozen {
		handlers.SendJSONError(w, "Build is frozen", http.StatusConflict)
		return
	}
	var req addModrinthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.VersionID == "" {
		handlers.SendJSONError(w, "versionId required", http.StatusBadRequest)
		return
	}
	if req.Side == "" {
		req.Side = models.SideBoth
	}
	if req.ContentType == "" {
		req.ContentType = models.ContentTypeMod
	}
	if !validContentSide(req.Side) {
		handlers.SendJSONError(w, "Invalid side", http.StatusBadRequest)
		return
	}

	v, err := services.FetchModrinthVersion(req.VersionID)
	if err != nil {
		handlers.SendJSONError(w, "Modrinth version lookup failed", http.StatusBadGateway)
		return
	}
	if req.ProjectID != "" && req.ProjectID != v.ProjectID {
		handlers.SendJSONError(w, "Version does not belong to the selected Modrinth project", http.StatusBadRequest)
		return
	}
	if err := validateModrinthContent(b, v, req.ContentType); err != nil {
		handlers.SendJSONError(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	existing, err := h.state.Store.ListBuildContent(b.ID)
	if err != nil {
		handlers.SendJSONError(w, "Failed to read existing build content", http.StatusInternalServerError)
		return
	}
	have := map[string]bool{v.ProjectID: true}
	projectSides := map[string]uint8{}
	for _, entry := range existing {
		if entry.ModrinthProjectID != "" {
			projectSides[entry.ModrinthProjectID] |= contentSideMask(entry.Side)
		}
		if contentSideMask(entry.Side)&contentSideMask(req.Side) != 0 && (entry.ModrinthProjectID == v.ProjectID || strings.EqualFold(entry.TargetPath, targetPathFor(req.ContentType, v.PrimaryFile().Filename))) {
			handlers.SendJSONError(w, "This build already contains that project or target path. Update or remove its existing entry first.", http.StatusConflict)
			return
		}
	}
	for project, sides := range projectSides {
		if sides&contentSideMask(req.Side) == contentSideMask(req.Side) {
			have[project] = true
		}
	}
	if err := h.addModrinthVersion(owner, b, v, req.Side, req.ContentType); err != nil {
		handlers.SendJSONError(w, "Failed to add mod", http.StatusInternalServerError)
		return
	}

	added := 1
	resp := map[string]interface{}{"success": true}
	if req.ResolveDeps {
		deps, unresolved, err := services.ResolveModrinthDependencies(v, b.Minecraft, b.Loader, have)
		if err != nil {
			log.Printf("packs: dependencies of %s for build %d not resolved: %v", v.ID, b.ID, err)
			resp["warning"] = "Dependencies were not added: the build needs a loader and a Minecraft version."
		}
		if len(unresolved) > 0 {
			log.Printf("packs: %d dependencies of %s for build %d unresolved", len(unresolved), v.ID, b.ID)
			resp["unresolved"] = unresolved
		}
		for _, d := range deps {
			// Both sides, whatever side the project itself went on: a library a
			// client-only mod pulls in (Fabric API) is usually needed by server
			// mods too, and a client-only copy would be added a second time then.
			if err := h.addModrinthVersion(owner, b, d.Version, models.SideBoth, models.ContentTypeMod); err == nil {
				added++
			} else {
				resp["warning"] = "Some required dependencies could not be added. Review the build's content before exporting."
			}
		}
	}
	resp["added"] = added
	publishPackEvent(r, h.state, "pack_content.changed", b.PackID, map[string]interface{}{"buildId": b.ID})
	json.NewEncoder(w).Encode(resp)
}

// addModrinthVersion upserts the mod catalog row + a linked modversion and
// attaches it to the build. Only the Modrinth reference is stored; the jar is
// fetched from cdn.modrinth.com when a pack is rendered.
func (h *PacksHandler) addModrinthVersion(ownerID string, b *models.PackBuild, v *services.ModrinthVersion, side, contentType string) error {
	if contentType == "" {
		contentType = models.ContentTypeMod
	}
	if err := validateModrinthContent(b, v, contentType); err != nil {
		return err
	}
	if side != "" && !validContentSide(side) {
		return fmt.Errorf("invalid content side")
	}
	slug := handlers.Slugify(v.ProjectSlugOrID())
	modID, err := h.state.Store.UpsertMod(&models.Mod{
		OwnerID: ownerID, Slug: slug, PrettyName: v.Name, ContentType: contentType,
	})
	if err != nil {
		return err
	}
	file := v.PrimaryFile()
	mvID, err := h.state.Store.CreateModversion(&models.Modversion{
		ModID:                 modID,
		Version:               v.VersionNum,
		Filesize:              file.Size,
		SHA1:                  file.Hashes["sha1"],
		SHA512:                file.Hashes["sha512"],
		ModrinthDownloadURL:   file.URL,
		Source:                models.SourceModrinth,
		TargetPath:            targetPathFor(contentType, file.Filename),
		ModrinthProjectID:     v.ProjectID,
		ModrinthVersionID:     v.ID,
		ModrinthVersionNumber: v.VersionNum,
		ModrinthGameVersions:  strings.Join(v.GameVersions, ","),
	})
	if err != nil {
		return err
	}
	if side == "" {
		side = sideFromModrinthDefault()
	}
	_, err = h.state.Store.AttachModversionToBuild(b.ID, mvID, side)
	return err
}

// validateStoredModZip runs the store-time zip checks over a seekable upload
// (an io.ReaderAt) instead of a []byte, so a large pre-built content bundle
// never has to be read into memory just to be validated. It rejects a
// traversal-bearing entry name (the render re-serves these verbatim) and any
// entry declaring a size over the render cap (a decompression bomb must not be
// persisted). An unreadable zip is rejected.
func validateStoredModZip(ra io.ReaderAt, size int64) error {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return fmt.Errorf("zip is unreadable or malformed")
	}
	seen := packOutputPaths{}
	for _, f := range zr.File {
		if modpack.IsUnsafeEntryPath(f.Name) {
			return fmt.Errorf("zip contains unsafe entry paths")
		}
		if !f.FileInfo().IsDir() && !validContentTarget(f.Name) {
			return fmt.Errorf("zip contains invalid entry paths")
		}
		if !f.FileInfo().IsDir() {
			if err := seen.add(f.Name); err != nil {
				return err
			}
		}
		if f.UncompressedSize64 > maxServerPackEntryBytes {
			return fmt.Errorf("zip entry exceeds the size cap")
		}
	}
	return nil
}

// UploadContent POST /api/packs/{id}/builds/{buildId}/content/upload - uploads
// a file into a build. The body is bounded before parsing: the multipart
// memory budget only decides how much is held in RAM, not how much is
// accepted, and nothing further down this path checks a size at all.
func (h *PacksHandler) UploadContent(w http.ResponseWriter, r *http.Request) {
	b, owner, ok := h.loadBuild(r, packEdit)
	if !ok || b.Frozen {
		handlers.SendJSONError(w, "Forbidden or frozen", http.StatusForbidden)
		return
	}

	// Bound the body before parsing it. The small memory budget below decides how
	// much is held in RAM, NOT how much is accepted: ParseMultipartForm reads the
	// whole request either way and spills the rest to a temp file. Nothing further
	// down this path checks a size at all, so without this an ordinary user who
	// owns a pack could write an unbounded file to Core's disk and then have it
	// streamed on into modpack storage. The ticket upload had the same gap.
	if !handlers.CapBody(w, r, maxPackContentUploadBytes) {
		return
	}

	// multipart: field "file", plus form fields side + contentType. The memory
	// budget is small on purpose: it is how much of the file part is held in RAM
	// before the rest spills to a temp file, so a large upload costs a few MiB
	// of heap plus disk rather than its whole size in memory. The stored .zip
	// path then reads that temp-backed part by streaming.
	if err := r.ParseMultipartForm(8 << 20); err != nil { // 8 MiB in RAM, rest to disk
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			handlers.SendJSONError(w, "Upload exceeds the 1 GiB per-file limit", http.StatusRequestEntityTooLarge)
			return
		}
		handlers.SendJSONError(w, "Upload too large or malformed", http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()
	f, hdr, err := r.FormFile("file")
	if err != nil {
		handlers.SendJSONError(w, "file field required", http.StatusBadRequest)
		return
	}
	defer f.Close()
	side := r.FormValue("side")
	if side == "" {
		side = models.SideBoth
	}
	contentType := r.FormValue("contentType")
	if contentType == "" {
		contentType = models.ContentTypeMod
	}
	if !validContentSide(side) || !validContentType(contentType) {
		handlers.SendJSONError(w, "Invalid content type or side", http.StatusBadRequest)
		return
	}
	// Sanitize the client-supplied filename: normalize backslashes, take the
	// base name, and reject traversal so it cannot escape the zip/target path.
	fileName := path.Base(strings.ReplaceAll(hdr.Filename, `\`, "/"))
	if fileName == "." || fileName == "/" || fileName == "" || strings.Contains(fileName, "..") {
		handlers.SendJSONError(w, "Invalid file name", http.StatusBadRequest)
		return
	}
	targetPath := r.FormValue("targetPath")
	archive := r.FormValue("archive") == "true"
	if targetPath != "" && !validContentTarget(targetPath) || archive && (targetPath != "" || !strings.HasSuffix(strings.ToLower(fileName), ".zip")) {
		handlers.SendJSONError(w, "Invalid target path or archive options", http.StatusBadRequest)
		return
	}
	if targetPath == "" {
		targetPath = targetPathFor(contentType, fileName)
	}
	if !validContentTarget(targetPath) {
		handlers.SendJSONError(w, "Invalid target path", http.StatusBadRequest)
		return
	}
	isBundle := archive || contentType == models.ContentTypeMod && strings.HasSuffix(strings.ToLower(fileName), ".zip")
	if !isBundle {
		entries, err := h.state.Store.ListBuildContent(b.ID)
		if err != nil {
			handlers.SendJSONError(w, "Failed to check existing content", http.StatusInternalServerError)
			return
		}
		for _, entry := range entries {
			if strings.EqualFold(entry.TargetPath, targetPath) && contentSideMask(entry.Side)&contentSideMask(side) != 0 {
				handlers.SendJSONError(w, "This build already contains that target path. Edit or remove the existing entry first.", http.StatusConflict)
				return
			}
		}
	}

	prov, err := h.state.BuildModpackStorageProvider()
	if err != nil || prov == nil {
		handlers.SendJSONError(w, "No pack storage configured (Settings -> Modpacks)", http.StatusFailedDependency)
		return
	}
	slug := handlers.Slugify(strings.TrimSuffix(fileName, ".jar"))
	if contentType == models.ContentTypeConfig || contentType == models.ContentTypeOther || targetPath != targetPathFor(contentType, fileName) {
		// Equal basenames in config/defaultconfigs/KubeJS must not overwrite
		// each other's owner-scoped catalog metadata.
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%d/%s/%s", b.PackID, b.ID, contentType, targetPath)))
		if len(slug) > 100 {
			slug = slug[:100]
		}
		slug = fmt.Sprintf("%s-%x", slug, sum[:8])
	}

	meta, herr := h.storeUploadedContent(r.Context(), prov, f, hdr, fileName, contentType, owner, slug, uploadContentOptions{targetPath: targetPath, archive: archive})
	if herr != nil {
		handlers.SendJSONError(w, herr.msg, herr.status)
		return
	}
	md5hex, innerSha1, innerSha512 := meta.md5, meta.innerSha1, meta.innerSha512
	key, version := meta.key, meta.version

	modID, err := h.state.Store.UpsertMod(&models.Mod{
		OwnerID: owner, Slug: slug, PrettyName: fileName, ContentType: contentType,
	})
	if err != nil {
		// The object is stored and nothing points at it yet.
		h.deleteIfUnreferenced(context.WithoutCancel(r.Context()), prov, key)
		handlers.SendJSONError(w, "Failed to save mod", http.StatusInternalServerError)
		return
	}
	mv := &models.Modversion{
		ModID:      modID,
		Version:    version,
		Filesize:   meta.size,
		StorageKey: key,
		MD5:        md5hex,
		SHA1:       innerSha1,
		SHA512:     innerSha512,
		Source:     models.SourceUpload,
		TargetPath: targetPath,
	}
	// Auto-link: does this jar's sha1 match a Modrinth project?
	if innerSha1 != "" && !archive && (contentType == models.ContentTypeMod || contentType == models.ContentTypeResourcepack || contentType == models.ContentTypeShaderpack) {
		if v := services.ModrinthByHash(innerSha1); v != nil {
			if validateModrinthContent(b, v, contentType) == nil {
				mv.Source = models.SourceModrinth
				mv.ModrinthProjectID = v.ProjectID
				mv.ModrinthVersionID = v.ID
				mv.ModrinthVersionNumber = v.VersionNum
				mv.ModrinthGameVersions = strings.Join(v.GameVersions, ",")
				mv.ModrinthDownloadURL = v.PrimaryFile().URL
			}
		}
	}
	mvID, err := h.state.Store.CreateModversion(mv)
	if err != nil {
		h.deleteIfUnreferenced(context.WithoutCancel(r.Context()), prov, key)
		handlers.SendJSONError(w, "Failed to save version", http.StatusInternalServerError)
		return
	}
	if _, err := h.state.Store.AttachModversionToBuild(b.ID, mvID, side); err != nil {
		handlers.SendJSONError(w, "Failed to attach", http.StatusInternalServerError)
		return
	}
	publishPackEvent(r, h.state, "pack_content.changed", b.PackID, map[string]interface{}{"buildId": b.ID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "modversionId": mvID, "linked": mv.ModrinthProjectID != ""})
}

// RemoveContent DELETE /api/packs/{id}/builds/{buildId}/content/{modversionId}
// - detaches one entry from a build. A frozen build is refused.
func (h *PacksHandler) RemoveContent(w http.ResponseWriter, r *http.Request) {
	b, _, ok := h.loadBuild(r, packDelete)
	if !ok || b.Frozen {
		handlers.SendJSONError(w, "Forbidden or frozen", http.StatusForbidden)
		return
	}
	mvID, _ := strconv.Atoi(mux.Vars(r)["modversionId"])
	if err := h.state.Store.DetachFromBuild(b.ID, mvID); err != nil {
		handlers.SendJSONError(w, "Failed to remove", http.StatusInternalServerError)
		return
	}
	publishPackEvent(r, h.state, "pack_content.changed", b.PackID, map[string]interface{}{"buildId": b.ID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

type setSideRequest struct {
	Side string `json:"side"`
}

// SetSide PATCH /api/packs/{id}/builds/{buildId}/content/{modversionId}/side -
// sets whether an entry ships to the client, the server or both. A frozen
// build is refused.
func (h *PacksHandler) SetSide(w http.ResponseWriter, r *http.Request) {
	b, _, ok := h.loadBuild(r, packEdit)
	if !ok || b.Frozen {
		handlers.SendJSONError(w, "Forbidden or frozen", http.StatusForbidden)
		return
	}
	mvID, _ := strconv.Atoi(mux.Vars(r)["modversionId"])
	var req setSideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Side != models.SideClient && req.Side != models.SideServer && req.Side != models.SideBoth {
		handlers.SendJSONError(w, "Invalid side", http.StatusBadRequest)
		return
	}
	// AttachModversionToBuild is an upsert: without this check a caller could pass
	// another tenant's modversionId and pull it into their own build.
	if ok, _ := h.state.Store.IsModversionInBuild(b.ID, mvID); !ok {
		handlers.SendJSONError(w, "Content not found", http.StatusNotFound)
		return
	}
	if _, err := h.state.Store.AttachModversionToBuild(b.ID, mvID, req.Side); err != nil {
		handlers.SendJSONError(w, "Failed to set side", http.StatusInternalServerError)
		return
	}
	publishPackEvent(r, h.state, "pack_content.changed", b.PackID, map[string]interface{}{"buildId": b.ID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// --- helpers ---

func atoiVar(r *http.Request, key string) int { n, _ := strconv.Atoi(mux.Vars(r)[key]); return n }

// targetPathFor derives the in-.minecraft path for a content type.
// targetPathFor builds the .minecraft-relative path a piece of content installs
// to. It is the single writer of modversions.target_path, and that column is
// read back as a path by three different renderers, so the sanitizing happens
// here rather than in each caller.
//
// The upload caller already hands over a base name. The Modrinth one does not: addModrinthVersion passes the filename the MODRINTH
// API reported, which is third-party text that never touched a check on the way
// in.
func targetPathFor(contentType, fileName string) string {
	fileName = path.Base(strings.ReplaceAll(fileName, `\`, "/"))
	if fileName == "." || fileName == "/" || fileName == "" || strings.Contains(fileName, "..") {
		fileName = "unnamed"
	}
	switch contentType {
	case models.ContentTypeResourcepack:
		return "resourcepacks/" + fileName
	case models.ContentTypeShaderpack:
		return "shaderpacks/" + fileName
	case models.ContentTypeConfig:
		return "config/" + fileName
	default:
		return "mods/" + fileName
	}
}

func sideFromModrinthDefault() string { return models.SideBoth }
