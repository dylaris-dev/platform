package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"dylaris-core/storage"
)

// maxLibraryUploadBytes bounds one library upload. Matches the file manager's
// admin upload default (getTransferLimit in file.go), which is the closest
// existing expression of "how much an admin may push through Core at once".
const maxLibraryUploadBytes = 2 << 30 // 2 GiB

type LibraryHandler struct {
	state *AppState
}

func NewLibraryHandler(state *AppState) *LibraryHandler {
	return &LibraryHandler{state: state}
}

// GetLibraryHandler GET /api/library?path=
//
// Admins see all entries with their `enabled` flag set per-path so the UI can
// render a toggle. Non-admins:
//   - get an empty list when the requested path itself OR any ancestor is
//     marked disabled (strict AND semantics — "all the way up must be on")
//   - have disabled children silently filtered out of the listing
func (h *LibraryHandler) GetLibraryHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" || path == "/" {
		path = "/"
	} else {
		// The spelling the denylist compares against, and nothing else: the
		// storage backends clean a path themselves, so "./private" listed a
		// disabled folder while the check walked a different string. The
		// mirror has refused such spellings since it was written.
		canon, ok := canonicalLibraryPath(path)
		if !ok {
			sendJSONError(w, "Invalid path", http.StatusBadRequest)
			return
		}
		path = canon
	}

	prov, err := h.state.buildCoreStorageProvider(CoreStoragePrefixLibrary)
	if err != nil {
		coreStorageUnavailableResponse(w, err)
		return
	}

	files, err := prov.ListFiles(r.Context(), path)
	if err != nil {
		sendJSONError(w, "Could not list library files: "+err.Error(), http.StatusInternalServerError)
		return
	}

	disabledSet, err := h.disabledPathSet()
	if err != nil {
		sendJSONError(w, "Could not verify library access", http.StatusServiceUnavailable)
		return
	}
	isAdmin, _ := r.Context().Value("isAdmin").(bool)

	if !isAdmin && isPathBlocked(path, disabledSet) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"files":   []interface{}{},
		})
		return
	}

	out := files[:0]
	for _, f := range files {
		full := joinLibraryPath(path, f.Name)
		_, isDisabled := disabledSet[normalizeLibraryPath(full)]
		if isDisabled {
			if !isAdmin {
				continue
			}
			f.Enabled = false
		}
		out = append(out, f)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"files":   out,
	})
}

// ToggleLibraryPathHandler POST /api/library/toggle
// Body: { "path": "...", "enabled": false }
//
// Route-gated by RequireCap("settings.write") (Phase 4 Task 20); the former
// in-handler `if !isAdmin` block is now the chokepoint's job.
func (h *LibraryHandler) ToggleLibraryPathHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Path    string `json:"path"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.Path = normalizeLibraryPath(req.Path)
	if req.Path == "" || req.Path == "/" {
		sendJSONError(w, "Cannot toggle the library root", http.StatusBadRequest)
		return
	}

	if err := h.state.Store.SetLibraryPathDisabled(req.Path, !req.Enabled); err != nil {
		sendJSONError(w, "Toggle failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// disabledPathSet loads all disabled library paths into a lookup set,
// keyed by the normalized path string.
//
// Returns an error rather than an empty set when the denylist cannot be read:
// an empty set means "nothing is disabled", so swallowing the error handed a
// non-admin every path an admin had explicitly turned off. Callers must fail
// closed - same choice AuthMiddleware makes for a failed user lookup.
func (h *LibraryHandler) disabledPathSet() (map[string]struct{}, error) {
	set := map[string]struct{}{}
	if h.state.Store == nil {
		return nil, fmt.Errorf("no store configured")
	}
	paths, err := h.state.Store.ListDisabledLibraryPaths()
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		set[normalizeLibraryPath(p)] = struct{}{}
	}
	return set, nil
}

// normalizeLibraryPath strips leading/trailing slashes so paths from
// different sources (admin clicks, listing joins, DB rows) compare equal.
func normalizeLibraryPath(p string) string {
	p = strings.TrimSpace(p)
	for strings.HasPrefix(p, "/") {
		p = p[1:]
	}
	for strings.HasSuffix(p, "/") {
		p = p[:len(p)-1]
	}
	return p
}

// joinLibraryPath joins a parent listing path with a child entry name,
// producing a normalized library-relative path.
func joinLibraryPath(parent, name string) string {
	parent = normalizeLibraryPath(parent)
	if parent == "" {
		return name
	}
	return parent + "/" + name
}

// isPathBlocked returns true when `path` itself or any ancestor (excluding
// root) is in the disabled set — meaning a non-admin should see nothing here.
func isPathBlocked(path string, disabled map[string]struct{}) bool {
	cur := normalizeLibraryPath(path)
	if cur == "" {
		return false // root itself is never blockable (toggle handler refuses it)
	}
	for cur != "" {
		if _, ok := disabled[cur]; ok {
			return true
		}
		idx := strings.LastIndex(cur, "/")
		if idx < 0 {
			break
		}
		cur = cur[:idx]
	}
	return false
}

// DeleteLibraryHandler POST /api/library/delete
//
// Route-gated by RequireCap("settings.write") (Phase 4 Task 20); the former
// in-handler `if !isAdmin` block is now the chokepoint's job.
func (h *LibraryHandler) DeleteLibraryHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	// The provider refuses this too, but that surfaces as a 500 - and a client
	// that omits the field is sending a bad request, not hitting a server
	// fault. ToggleLibraryPathHandler has always rejected the same value.
	if strings.TrimSpace(req.Path) == "" {
		sendJSONError(w, "Cannot delete the library root", http.StatusBadRequest)
		return
	}

	prov, err := h.state.buildCoreStorageProvider(CoreStoragePrefixLibrary)
	if err != nil {
		coreStorageUnavailableResponse(w, err)
		return
	}
	if err := prov.DeletePath(r.Context(), req.Path); err != nil {
		if errors.Is(err, storage.ErrDeleteRoot) {
			sendJSONError(w, "Cannot delete the library root", http.StatusBadRequest)
			return
		}
		sendJSONError(w, "Delete failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// MkdirLibraryHandler POST /api/library/mkdir
//
// Route-gated by RequireCap("settings.write") (Phase 4 Task 20); the former
// in-handler `if !isAdmin` block is now the chokepoint's job.
func (h *LibraryHandler) MkdirLibraryHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	prov, err := h.state.buildCoreStorageProvider(CoreStoragePrefixLibrary)
	if err != nil {
		coreStorageUnavailableResponse(w, err)
		return
	}
	if err := prov.CreateDir(r.Context(), req.Path); err != nil {
		sendJSONError(w, "Create dir failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// UploadLibraryHandler POST /api/library/upload
//
// Route-gated by RequireCap("settings.write") (Phase 4 Task 20); the former
// in-handler `if !isAdmin` block is now the chokepoint's job.
func (h *LibraryHandler) UploadLibraryHandler(w http.ResponseWriter, r *http.Request) {
	// The memory budget below is not a limit on what is ACCEPTED - the rest
	// spills to a temp file and the request is read in full either way - so
	// bound the body first. Admin-only, so this is a footgun rather than a
	// privilege issue; the ceiling matches the file manager's admin upload default.
	if !capBody(w, r, maxLibraryUploadBytes) {
		return
	}

	// 32MiB in memory; larger files spill to a temp file on disk. Passing 2GiB
	// here would buffer the whole upload in RAM.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		// The old message said "File too large" for every parse failure,
		// including a malformed body. Now it only says that when it is true.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			sendJSONError(w, "File too large", http.StatusRequestEntityTooLarge)
			return
		}
		sendJSONError(w, "Malformed upload", http.StatusBadRequest)
		return
	}

	path := r.FormValue("path")
	if path == "" {
		path = "/"
	}
	if strings.Contains(path, "..") {
		sendJSONError(w, "Invalid path", http.StatusBadRequest)
		return
	}

	prov, err := h.state.buildCoreStorageProvider(CoreStoragePrefixLibrary)
	if err != nil {
		coreStorageUnavailableResponse(w, err)
		return
	}

	files := r.MultipartForm.File["files"]
	for _, fh := range files {
		cleanName := sanitizeFilename(fh.Filename)
		if cleanName == "" {
			continue
		}
		destPath := strings.TrimSuffix(path, "/") + "/" + cleanName

		f, err := fh.Open()
		if err != nil {
			sendJSONError(w, "Could not open file", http.StatusInternalServerError)
			return
		}
		// Closed here, per FILE, rather than deferred. ParseMultipartForm spills
		// anything over its in-memory threshold to a temp file, so those parts
		// each hold an OS file descriptor - and a defer inside this loop would
		// keep every one of them open until the whole upload finished. Nothing
		// caps how many files one request may carry.
		werr := prov.WriteFile(r.Context(), destPath, f)
		f.Close()
		if werr != nil {
			sendJSONError(w, fmt.Sprintf("Upload failed for %s: %v", cleanName, werr), http.StatusInternalServerError)
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("%d files uploaded", len(files)),
	})
}

// DownloadLibraryHandler GET /api/library/download - streams the library file
// named by ?path. Any path containing .. is refused, and the access model
// mirrors the browse handler so a non-admin cannot download what they cannot
// list.
func (h *LibraryHandler) DownloadLibraryHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		sendJSONError(w, "Path required", http.StatusBadRequest)
		return
	}
	// Canonical or refused, as on the mirror and in browse: "./private/x.jar"
	// passed a ".." check, walked past the denylist and was served, because
	// the backend cleaned it into the disabled path.
	canon, ok := canonicalLibraryPath(path)
	if !ok {
		sendJSONError(w, "Invalid path", http.StatusBadRequest)
		return
	}
	path = canon

	// Mirror the browse handler's access model: a non-admin must not download
	// a file that lives under an admin-disabled path (the browse listing hides
	// these, but download took the path verbatim).
	if isAdmin, _ := r.Context().Value("isAdmin").(bool); !isAdmin {
		disabledSet, err := h.disabledPathSet()
		if err != nil {
			sendJSONError(w, "Could not verify library access", http.StatusServiceUnavailable)
			return
		}
		if _, blocked := disabledSet[normalizeLibraryPath(path)]; blocked || isPathBlocked(path, disabledSet) {
			sendJSONError(w, "Access denied", http.StatusForbidden)
			return
		}
	}

	prov, err := h.state.buildCoreStorageProvider(CoreStoragePrefixLibrary)
	if err != nil {
		coreStorageUnavailableResponse(w, err)
		return
	}

	// Prefer a short-lived pre-signed URL when the backend supports it (S3):
	// redirect the browser straight to object storage instead of streaming
	// every byte through Core.
	if url, err := prov.DownloadURL(r.Context(), path, 5*time.Minute); err == nil && url != "" {
		http.Redirect(w, r, url, http.StatusFound)
		return
	}

	rc, err := prov.GetFile(r.Context(), path)
	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	defer rc.Close()

	filename := filepath.Base(path)
	setAttachmentDisposition(w, filename)
	w.Header().Set("Content-Type", "application/octet-stream")

	buf := make([]byte, 32*1024)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
}
