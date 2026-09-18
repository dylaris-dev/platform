package handlers

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

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

// loadOwnedBuild returns the build if the caller owns its pack. Frozen state is
// left for callers to gate on (list/read is allowed on frozen builds; writes
// are not).
func (h *PacksHandler) loadOwnedBuild(r *http.Request) (*models.PackBuild, bool) {
	packID, _ := strconv.Atoi(mux.Vars(r)["id"])
	buildID, _ := strconv.Atoi(mux.Vars(r)["buildId"])
	userID, _ := r.Context().Value("userID").(string)
	p, err := h.state.Store.GetPack(packID)
	if err != nil || p == nil || p.OwnerID != userID {
		return nil, false
	}
	b, err := h.state.Store.GetPackBuild(buildID)
	if err != nil || b == nil || b.PackID != packID {
		return nil, false
	}
	return b, true
}

// ListContent GET /api/packs/{id}/builds/{buildId}/content - the mods and
// files in one build. Owning the pack in the path says nothing about the build
// in it, so the build is bound to the pack here too: without that a caller
// could pair their own packId with another tenant's buildId and read that
// build's content, storage keys included.
func (h *PacksHandler) ListContent(w http.ResponseWriter, r *http.Request) {
	packID := atoiVar(r, "id")
	if _, ok := h.ownsPack(r, packID); !ok {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
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
		sendJSONError(w, "Build not found", http.StatusNotFound)
		return
	}
	entries, err := h.state.Store.ListBuildContent(buildID)
	if err != nil {
		sendJSONError(w, "Failed to list content", http.StatusInternalServerError)
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
	b, ok := h.loadOwnedBuild(r)
	if !ok {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	if b.Frozen {
		sendJSONError(w, "Build is frozen", http.StatusConflict)
		return
	}
	var req addModrinthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.VersionID == "" {
		sendJSONError(w, "versionId required", http.StatusBadRequest)
		return
	}
	userID, _ := r.Context().Value("userID").(string)

	v, err := services.FetchModrinthVersion(req.VersionID)
	if err != nil {
		sendJSONError(w, "Modrinth version lookup failed", http.StatusBadGateway)
		return
	}
	if err := h.addModrinthVersion(userID, b, v, req.Side, req.ContentType); err != nil {
		sendJSONError(w, "Failed to add mod", http.StatusInternalServerError)
		return
	}

	added := 1
	if req.ResolveDeps {
		have := map[string]bool{v.ProjectID: true}
		deps, _ := services.ResolveModrinthDependencies(v, b.Minecraft, b.Loader, have)
		for _, d := range deps {
			if err := h.addModrinthVersion(userID, b, d.Version, models.SideBoth, models.ContentTypeMod); err == nil {
				added++
			}
		}
	}
	h.state.Events.Publish(r.Context(), "pack_content.changed", map[string]interface{}{"buildId": b.ID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "added": added})
}

// addModrinthVersion upserts the mod catalog row + a linked modversion and
// attaches it to the build. Only the Modrinth reference is stored; the jar is
// fetched from cdn.modrinth.com when a pack is rendered.
func (h *PacksHandler) addModrinthVersion(ownerID string, b *models.PackBuild, v *services.ModrinthVersion, side, contentType string) error {
	if contentType == "" {
		contentType = models.ContentTypeMod
	}
	slug := slugify(v.ProjectSlugOrID())
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
	for _, f := range zr.File {
		if modpack.IsUnsafeEntryPath(f.Name) {
			return fmt.Errorf("zip contains unsafe entry paths")
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
	b, ok := h.loadOwnedBuild(r)
	if !ok || b.Frozen {
		sendJSONError(w, "Forbidden or frozen", http.StatusForbidden)
		return
	}
	userID, _ := r.Context().Value("userID").(string)

	// Bound the body before parsing it. The small memory budget below decides how
	// much is held in RAM, NOT how much is accepted: ParseMultipartForm reads the
	// whole request either way and spills the rest to a temp file. Nothing further
	// down this path checks a size at all, so without this an ordinary user who
	// owns a pack could write an unbounded file to Core's disk and then have it
	// streamed on into modpack storage. The ticket upload had the same gap.
	if !capBody(w, r, maxPackContentUploadBytes) {
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
			sendJSONError(w, "Upload exceeds the 1 GiB per-file limit", http.StatusRequestEntityTooLarge)
			return
		}
		sendJSONError(w, "Upload too large or malformed", http.StatusBadRequest)
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		sendJSONError(w, "file field required", http.StatusBadRequest)
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
	// Sanitize the client-supplied filename: normalize backslashes, take the
	// base name, and reject traversal so it cannot escape the zip/target path.
	fileName := path.Base(strings.ReplaceAll(hdr.Filename, `\`, "/"))
	if fileName == "." || fileName == "/" || fileName == "" || strings.Contains(fileName, "..") {
		sendJSONError(w, "Invalid file name", http.StatusBadRequest)
		return
	}

	prov, err := h.state.buildModpackStorageProvider()
	if err != nil || prov == nil {
		sendJSONError(w, "No pack storage configured (Settings -> Modpacks)", http.StatusFailedDependency)
		return
	}
	slug := slugify(strings.TrimSuffix(fileName, ".jar"))

	meta, herr := h.storeUploadedContent(r.Context(), prov, f, hdr, fileName, contentType, userID, slug)
	if herr != nil {
		sendJSONError(w, herr.msg, herr.status)
		return
	}
	md5hex, innerSha1, innerSha512 := meta.md5, meta.innerSha1, meta.innerSha512
	key, version := meta.key, meta.version

	modID, err := h.state.Store.UpsertMod(&models.Mod{
		OwnerID: userID, Slug: slug, PrettyName: fileName, ContentType: contentType,
	})
	if err != nil {
		sendJSONError(w, "Failed to save mod", http.StatusInternalServerError)
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
		TargetPath: targetPathFor(contentType, fileName),
	}
	// Auto-link: does this jar's sha1 match a Modrinth project?
	if innerSha1 != "" {
		if v := services.ModrinthByHash(innerSha1); v != nil {
			mv.Source = models.SourceModrinth
			mv.ModrinthProjectID = v.ProjectID
			mv.ModrinthVersionID = v.ID
			mv.ModrinthVersionNumber = v.VersionNum
			mv.ModrinthGameVersions = strings.Join(v.GameVersions, ",")
			mv.ModrinthDownloadURL = v.PrimaryFile().URL
		}
	}
	mvID, err := h.state.Store.CreateModversion(mv)
	if err != nil {
		sendJSONError(w, "Failed to save version", http.StatusInternalServerError)
		return
	}
	if _, err := h.state.Store.AttachModversionToBuild(b.ID, mvID, side); err != nil {
		sendJSONError(w, "Failed to attach", http.StatusInternalServerError)
		return
	}
	h.state.Events.Publish(r.Context(), "pack_content.changed", map[string]interface{}{"buildId": b.ID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "modversionId": mvID, "linked": mv.ModrinthProjectID != ""})
}

// RemoveContent DELETE /api/packs/{id}/builds/{buildId}/content/{modversionId}
// - detaches one entry from a build. A frozen build is refused.
func (h *PacksHandler) RemoveContent(w http.ResponseWriter, r *http.Request) {
	b, ok := h.loadOwnedBuild(r)
	if !ok || b.Frozen {
		sendJSONError(w, "Forbidden or frozen", http.StatusForbidden)
		return
	}
	mvID, _ := strconv.Atoi(mux.Vars(r)["modversionId"])
	if err := h.state.Store.DetachFromBuild(b.ID, mvID); err != nil {
		sendJSONError(w, "Failed to remove", http.StatusInternalServerError)
		return
	}
	h.state.Events.Publish(r.Context(), "pack_content.changed", map[string]interface{}{"buildId": b.ID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

type setSideRequest struct {
	Side string `json:"side"`
}

// SetSide PATCH /api/packs/{id}/builds/{buildId}/content/{modversionId}/side -
// sets whether an entry ships to the client, the server or both. A frozen
// build is refused.
func (h *PacksHandler) SetSide(w http.ResponseWriter, r *http.Request) {
	b, ok := h.loadOwnedBuild(r)
	if !ok || b.Frozen {
		sendJSONError(w, "Forbidden or frozen", http.StatusForbidden)
		return
	}
	mvID, _ := strconv.Atoi(mux.Vars(r)["modversionId"])
	var req setSideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Side != models.SideClient && req.Side != models.SideServer && req.Side != models.SideBoth {
		sendJSONError(w, "Invalid side", http.StatusBadRequest)
		return
	}
	// AttachModversionToBuild is an upsert: without this check a caller could pass
	// another tenant's modversionId and pull it into their own build.
	if ok, _ := h.state.Store.IsModversionInBuild(b.ID, mvID); !ok {
		sendJSONError(w, "Content not found", http.StatusNotFound)
		return
	}
	if _, err := h.state.Store.AttachModversionToBuild(b.ID, mvID, req.Side); err != nil {
		sendJSONError(w, "Failed to set side", http.StatusInternalServerError)
		return
	}
	h.state.Events.Publish(r.Context(), "pack_content.changed", map[string]interface{}{"buildId": b.ID})
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
