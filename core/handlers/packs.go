package handlers

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"dylaris-core/models"

	"github.com/gorilla/mux"
)

type PacksHandler struct {
	state *AppState
	pat   PATLoader
}

func NewPacksHandler(state *AppState) *PacksHandler {
	return &PacksHandler{state: state}
}

// SetPATLoader wires the Modrinth PAT loader (set from main after both handlers exist).
func (h *PacksHandler) SetPATLoader(p PATLoader) { h.pat = p }

var packSlugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]{1,62}[a-z0-9])?$`)

// ownsPack loads the pack and checks the caller owns it (admins may read).
func (h *PacksHandler) ownsPack(r *http.Request, packID int) (*models.Pack, bool) {
	p, err := h.state.Store.GetPack(packID)
	if err != nil || p == nil {
		return nil, false
	}
	userID, _ := r.Context().Value("userID").(string)
	if p.OwnerID == userID {
		return p, true
	}
	if isAdmin, _ := r.Context().Value("isAdmin").(bool); isAdmin {
		return p, true // read-only elsewhere; write handlers re-check ownership
	}
	return nil, false
}

// packRequest is the body for both Create and the Update PATCH.
//
// Summary is a POINTER because Update is a PATCH: as a plain string, a body
// that simply left it out decoded to "" and Update wrote that over the stored
// value. Absent means "leave it alone", the same rule serverTabRequest applies
// with *bool.
type packRequest struct {
	Name    string  `json:"name"`
	Slug    string  `json:"slug"`
	Summary *string `json:"summary"`
}

// strVal dereferences an optional request string, treating absent as empty.
// Used on the Create path, where every field is being set for the first time
// and "absent" and "empty" genuinely mean the same thing.
func strVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// List GET /api/me/packs - the modpacks the calling user owns.
func (h *PacksHandler) List(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	packs, err := h.state.Store.ListPacksByOwner(userID)
	if err != nil {
		sendJSONError(w, "Failed to list packs", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "packs": packs})
}

// Create POST /api/me/packs - creates a modpack owned by the caller. A
// duplicate slug is 409.
func (h *PacksHandler) Create(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var req packRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		sendJSONError(w, "Name is required", http.StatusBadRequest)
		return
	}
	slug := strings.TrimSpace(req.Slug)
	if slug == "" {
		slug = slugify(req.Name)
	}
	if !packSlugRe.MatchString(slug) {
		sendJSONError(w, "Invalid slug", http.StatusBadRequest)
		return
	}
	p := &models.Pack{
		OwnerID:            userID,
		InternalName:       req.Name,
		InternalSlug:       slug,
		Summary:            strings.TrimSpace(strVal(req.Summary)),
		ModrinthVisibility: "unlisted",
	}
	id, err := h.state.Store.CreatePack(p)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "duplicate") || strings.Contains(msg, "unique") {
			sendJSONError(w, "A pack with that slug already exists", http.StatusConflict)
		} else {
			sendJSONError(w, "Failed to create pack", http.StatusInternalServerError)
		}
		return
	}
	p.ID = id
	h.state.Events.Publish(r.Context(), "packs.changed", map[string]interface{}{"ownerId": userID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "pack": p})
}

// Get GET /api/packs/{id} - one modpack. A pack the caller does not own is
// 403, not 404.
func (h *PacksHandler) Get(w http.ResponseWriter, r *http.Request) {
	packID, _ := strconv.Atoi(mux.Vars(r)["id"])
	p, ok := h.ownsPack(r, packID)
	if !ok {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "pack": p})
}

// Update PATCH /api/packs/{id} - edits a modpack the caller owns.
func (h *PacksHandler) Update(w http.ResponseWriter, r *http.Request) {
	packID, _ := strconv.Atoi(mux.Vars(r)["id"])
	userID, _ := r.Context().Value("userID").(string)
	p, err := h.state.Store.GetPack(packID)
	if err != nil || p == nil || p.OwnerID != userID {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	var req packRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if n := strings.TrimSpace(req.Name); n != "" {
		p.InternalName = n
	}
	if req.Summary != nil {
		p.Summary = strings.TrimSpace(*req.Summary)
	}
	if err := h.state.Store.UpdatePack(p); err != nil {
		sendJSONError(w, "Failed to update", http.StatusInternalServerError)
		return
	}
	h.state.Events.Publish(r.Context(), "packs.changed", map[string]interface{}{"ownerId": userID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "pack": p})
}

// Delete DELETE /api/packs/{id} - deletes a modpack the caller owns.
func (h *PacksHandler) Delete(w http.ResponseWriter, r *http.Request) {
	packID, _ := strconv.Atoi(mux.Vars(r)["id"])
	userID, _ := r.Context().Value("userID").(string)
	p, err := h.state.Store.GetPack(packID)
	if err != nil || p == nil || p.OwnerID != userID {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	if err := h.state.Store.DeletePack(packID, userID); err != nil {
		sendJSONError(w, "Failed to delete", http.StatusInternalServerError)
		return
	}
	h.state.Events.Publish(r.Context(), "packs.changed", map[string]interface{}{"ownerId": userID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

type buildRequest struct {
	VersionString string `json:"versionString"`
	Minecraft     string `json:"minecraft"`
	Loader        string `json:"loader"`
	LoaderVersion string `json:"loaderVersion"`
	Changelog     string `json:"changelog"`
}

// ListBuilds GET /api/packs/{id}/builds - the builds of one modpack.
func (h *PacksHandler) ListBuilds(w http.ResponseWriter, r *http.Request) {
	packID, _ := strconv.Atoi(mux.Vars(r)["id"])
	if _, ok := h.ownsPack(r, packID); !ok {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	builds, err := h.state.Store.ListPackBuilds(packID)
	if err != nil {
		sendJSONError(w, "Failed to list builds", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "builds": builds})
}

// validateBuildKeyComponents reports the first build field that must not carry a
// path separator or a traversal, or "" when all three are fine.
//
// VersionString has been checked since this handler was written; these three
// travel with the build into the .mrpack's dependencies, and a node installing
// the pack formats them straight into a meta.fabricmc.net path with no escaping
// (node/installer.go). Unescaped request text deciding which upstream path is
// fetched is not theoretical, so the values are checked where they arrive rather
// than trusted wherever they end up.
//
// Empty passes: a vanilla build has no loader and no loader version.
// safeKeyComponent rejects "" on purpose (an empty key COMPONENT is a different
// question from an absent field), so emptiness is handled here.
// safeKeyComponent rejects a value that could escape the namespace it is placed
// in, as a storage-key segment or an upstream URL path segment. Modrinth version
// numbers reach us verbatim (no charset validation), so a crafted "../.." could
// otherwise address another tenant's objects - and no read-time guard can undo a
// bad write.
func safeKeyComponent(s string) bool {
	return s != "" && !strings.ContainsAny(s, `/\`) && !strings.Contains(s, "..")
}

func validateBuildKeyComponents(minecraft, loader, loaderVersion string) string {
	for _, f := range []struct{ name, value string }{
		{"minecraft", minecraft},
		{"loader", loader},
		{"loaderVersion", loaderVersion},
	} {
		if f.value != "" && !safeKeyComponent(f.value) {
			return f.name
		}
	}
	return ""
}

// CreateBuild POST /api/packs/{id}/builds - adds a build. A duplicate version
// is 409.
func (h *PacksHandler) CreateBuild(w http.ResponseWriter, r *http.Request) {
	packID, _ := strconv.Atoi(mux.Vars(r)["id"])
	userID, _ := r.Context().Value("userID").(string)
	p, err := h.state.Store.GetPack(packID)
	if err != nil || p == nil || p.OwnerID != userID {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	var req buildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.VersionString) == "" {
		sendJSONError(w, "versionString is required", http.StatusBadRequest)
		return
	}
	// reject path chars: VersionString feeds the download filename and the node-side install paths
	if !safeKeyComponent(strings.TrimSpace(req.VersionString)) {
		sendJSONError(w, "versionString contains invalid path characters", http.StatusBadRequest)
		return
	}
	if bad := validateBuildKeyComponents(strings.TrimSpace(req.Minecraft), strings.TrimSpace(req.Loader), strings.TrimSpace(req.LoaderVersion)); bad != "" {
		sendJSONError(w, bad+" contains invalid path characters", http.StatusBadRequest)
		return
	}
	b := &models.PackBuild{
		PackID:        packID,
		VersionString: strings.TrimSpace(req.VersionString),
		Minecraft:     strings.TrimSpace(req.Minecraft),
		Loader:        strings.TrimSpace(req.Loader),
		LoaderVersion: strings.TrimSpace(req.LoaderVersion),
		Changelog:     req.Changelog,
		Channel:       models.ChannelDraft,
	}
	id, err := h.state.Store.CreatePackBuild(b)
	if err != nil {
		sendJSONError(w, "Failed to create build (version may exist)", http.StatusConflict)
		return
	}
	b.ID = id
	h.state.Events.Publish(r.Context(), "pack_builds.changed", map[string]interface{}{"packId": packID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "build": b})
}

// UpdateBuild PATCH /api/packs/{id}/builds/{buildId} - edits a build. The
// build must belong to the pack in the path (404 otherwise).
func (h *PacksHandler) UpdateBuild(w http.ResponseWriter, r *http.Request) {
	packID, _ := strconv.Atoi(mux.Vars(r)["id"])
	buildID, _ := strconv.Atoi(mux.Vars(r)["buildId"])
	userID, _ := r.Context().Value("userID").(string)
	p, err := h.state.Store.GetPack(packID)
	if err != nil || p == nil || p.OwnerID != userID {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	b, err := h.state.Store.GetPackBuild(buildID)
	if err != nil || b == nil || b.PackID != packID {
		sendJSONError(w, "Build not found", http.StatusNotFound)
		return
	}
	if b.Frozen {
		sendJSONError(w, "Build is published and frozen", http.StatusConflict)
		return
	}
	var req buildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if v := strings.TrimSpace(req.VersionString); v != "" {
		if !safeKeyComponent(v) {
			sendJSONError(w, "versionString contains invalid path characters", http.StatusBadRequest)
			return
		}
		b.VersionString = v
	}
	if bad := validateBuildKeyComponents(strings.TrimSpace(req.Minecraft), strings.TrimSpace(req.Loader), strings.TrimSpace(req.LoaderVersion)); bad != "" {
		sendJSONError(w, bad+" contains invalid path characters", http.StatusBadRequest)
		return
	}
	b.Minecraft = strings.TrimSpace(req.Minecraft)
	b.Loader = strings.TrimSpace(req.Loader)
	b.LoaderVersion = strings.TrimSpace(req.LoaderVersion)
	b.Changelog = req.Changelog
	if err := h.state.Store.UpdatePackBuild(b); err != nil {
		sendJSONError(w, "Failed to update build", http.StatusInternalServerError)
		return
	}
	h.state.Events.Publish(r.Context(), "pack_builds.changed", map[string]interface{}{"packId": packID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "build": b})
}

// DeleteBuild DELETE /api/packs/{id}/builds/{buildId} - deletes a build that
// belongs to the pack in the path.
func (h *PacksHandler) DeleteBuild(w http.ResponseWriter, r *http.Request) {
	packID, _ := strconv.Atoi(mux.Vars(r)["id"])
	buildID, _ := strconv.Atoi(mux.Vars(r)["buildId"])
	userID, _ := r.Context().Value("userID").(string)
	p, err := h.state.Store.GetPack(packID)
	if err != nil || p == nil || p.OwnerID != userID {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	b, err := h.state.Store.GetPackBuild(buildID)
	if err != nil || b == nil || b.PackID != packID {
		sendJSONError(w, "Build not found", http.StatusNotFound)
		return
	}
	if b.Frozen {
		sendJSONError(w, "Build is published and frozen", http.StatusConflict)
		return
	}
	if err := h.state.Store.DeletePackBuild(buildID, packID); err != nil {
		sendJSONError(w, "Failed to delete build", http.StatusInternalServerError)
		return
	}
	h.state.Events.Publish(r.Context(), "pack_builds.changed", map[string]interface{}{"packId": packID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}
