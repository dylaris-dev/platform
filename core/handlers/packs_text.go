package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"path"
	"strconv"
	"unicode/utf8"

	"dylaris-core/models"
	"dylaris-core/storage/modpack"

	"github.com/gorilla/mux"
)

// maxEditableTextBytes bounds how much text the in-panel editor will load/save.
const maxEditableTextBytes = 1 << 20 // 1 MiB

// loadOwnedContentModversion resolves the ownership-checked build, the
// modversion (verified attached to that build, not a foreign id), and the
// storage provider. It does NOT check Frozen (callers gate writes on it).
func (h *PacksHandler) loadOwnedContentModversion(r *http.Request) (*models.PackBuild, *models.Modversion, modpack.ModpackStorageProvider, bool) {
	b, ok := h.loadOwnedBuild(r)
	if !ok {
		return nil, nil, nil, false
	}
	mvID, _ := strconv.Atoi(mux.Vars(r)["modversionId"])
	if ok, _ := h.state.Store.IsModversionInBuild(b.ID, mvID); !ok {
		return nil, nil, nil, false
	}
	mv, err := h.state.Store.GetModversion(mvID)
	if err != nil || mv == nil {
		return nil, nil, nil, false
	}
	prov, err := h.state.buildModpackStorageProvider()
	if err != nil || prov == nil {
		return nil, nil, nil, false
	}
	return b, mv, prov, true
}

// GetContentText returns the UTF-8 text of a single stored content entry (e.g.
// a config file) so the panel can edit it in place. Modrinth-reference /
// non-text / oversized entries are refused.
func (h *PacksHandler) GetContentText(w http.ResponseWriter, r *http.Request) {
	_, mv, prov, ok := h.loadOwnedContentModversion(r)
	if !ok {
		sendJSONError(w, "Content not found", http.StatusNotFound)
		return
	}
	if mv.StorageKey == "" {
		sendJSONError(w, "This content has no editable file (Modrinth reference)", http.StatusUnprocessableEntity)
		return
	}
	raw, err := prov.Get(r.Context(), mv.StorageKey)
	if err != nil {
		sendJSONError(w, "Failed to read content", http.StatusInternalServerError)
		return
	}
	data, found := modpack.ReadZipEntry(raw, mv.TargetPath, maxEditableTextBytes)
	if !found {
		sendJSONError(w, "No editable file at the content path", http.StatusUnprocessableEntity)
		return
	}
	if len(data) > maxEditableTextBytes || !utf8.Valid(data) {
		sendJSONError(w, "Content is not editable text", http.StatusUnsupportedMediaType)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "text": string(data), "targetPath": mv.TargetPath})
}

type setContentTextRequest struct {
	Text string `json:"text"`
}

// SetContentText overwrites a stored text content entry with edited bytes,
// re-wrapping into the content zip at the same key and re-hashing. Editing an
// entry that was Modrinth-linked unlinks it (the file now diverges from the
// upstream version).
func (h *PacksHandler) SetContentText(w http.ResponseWriter, r *http.Request) {
	b, mv, prov, ok := h.loadOwnedContentModversion(r)
	if !ok {
		sendJSONError(w, "Content not found", http.StatusNotFound)
		return
	}
	if b.Frozen {
		sendJSONError(w, "Build is frozen", http.StatusConflict)
		return
	}
	if mv.StorageKey == "" {
		sendJSONError(w, "This content has no editable file (Modrinth reference)", http.StatusUnprocessableEntity)
		return
	}
	curRaw, err := prov.Get(r.Context(), mv.StorageKey)
	if err != nil {
		sendJSONError(w, "Failed to read content", http.StatusInternalServerError)
		return
	}
	cur, found := modpack.ReadZipEntry(curRaw, mv.TargetPath, maxEditableTextBytes)
	if !found || !utf8.Valid(cur) {
		sendJSONError(w, "This content is not an editable text file", http.StatusUnprocessableEntity)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxEditableTextBytes+1))
	if err != nil {
		sendJSONError(w, "Failed to read body", http.StatusBadRequest)
		return
	}
	var req setContentTextRequest
	if err := json.Unmarshal(body, &req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	newBytes := []byte(req.Text)
	if len(newBytes) > maxEditableTextBytes {
		sendJSONError(w, "Text too large", http.StatusRequestEntityTooLarge)
		return
	}
	zipBytes, err := modpack.BuildContentZip(mv.TargetPath, newBytes)
	if err != nil {
		sendJSONError(w, "Failed to wrap content", http.StatusInternalServerError)
		return
	}
	md5hex, _, _ := modpack.Hashes(zipBytes)
	// A FRESH key, not an overwrite of mv.StorageKey.
	//
	// The object can have more than one modversion row pointing at it:
	// MigrateBuild's copyUploadedContent creates a new row for the same key on
	// purpose, so that updating one build cannot rewrite the other's row. This
	// handler wrote the edited bytes straight back to the shared key, which
	// rewrote the source build's file anyway - the exact thing the copy was
	// built to prevent, reached through the storage layer instead of the DB.
	//
	// Derived from the existing key, so it stays inside the same
	// packs/<owner>/mods/<slug>/ directory and cannot be steered anywhere by
	// the request. Keyed by the content md5, so re-saving the same text is
	// idempotent instead of growing a chain of keys.
	oldKey := mv.StorageKey
	newKey := editedContentKey(oldKey, md5hex)
	if err := prov.Put(r.Context(), newKey, zipBytes); err != nil {
		sendJSONError(w, "Storage put failed", http.StatusInternalServerError)
		return
	}
	mv.StorageKey = newKey
	_, innerSha1, innerSha512 := modpack.Hashes(newBytes)
	mv.Filesize = int64(len(zipBytes))
	mv.MD5 = md5hex
	mv.SHA1 = innerSha1
	mv.SHA512 = innerSha512
	// An edited file diverges from its upstream Modrinth version -> unlink.
	mv.Source = models.SourceUpload
	mv.ModrinthProjectID = ""
	mv.ModrinthVersionID = ""
	mv.ModrinthVersionNumber = ""
	mv.ModrinthGameVersions = ""
	mv.ModrinthDownloadURL = ""
	mv.ModrinthLatestVersionID = ""
	mv.ModrinthLastChecked = nil
	if err := h.state.Store.UpdateModversion(mv); err != nil {
		sendJSONError(w, "Failed to save content", http.StatusInternalServerError)
		return
	}
	// Safe-cutover ordering, exactly like swapModversionToModrinth: the row is
	// already off the old key before anything is removed, and the removal only
	// happens once no row points at it.
	if oldKey != newKey {
		h.deleteIfUnreferenced(r.Context(), prov, oldKey)
	}
	h.state.Events.Publish(r.Context(), "pack_content.changed", map[string]interface{}{"buildId": b.ID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// editedContentKey is where an edited content zip is stored.
//
// Derived from the existing key so it stays inside the same
// packs/<owner>/mods/<slug>/ directory: nothing in the request steers it, and
// an edit cannot land in another owner's prefix. Named by the content md5, so
// saving the same text twice is idempotent rather than growing a chain of keys,
// and two different edits never collide on one object.
func editedContentKey(oldKey, md5hex string) string {
	return path.Dir(oldKey) + "/edit-" + md5hex + ".zip"
}
