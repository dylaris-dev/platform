package packs

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

type PacksHandler struct {
	state *handlers.AppState
	pat   PATLoader

	// teamScans remembers when a user's Modrinth projects were last listed,
	// so the pack list asks Modrinth at most every few minutes per user.
	teamScanMu sync.Mutex
	teamScans  map[string]time.Time
}

func NewPacksHandler(state *handlers.AppState) *PacksHandler {
	return &PacksHandler{state: state}
}

// SetPATLoader wires the Modrinth PAT loader (set from main after both handlers exist).
func (h *PacksHandler) SetPATLoader(p PATLoader) { h.pat = p }

var packSlugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]{1,62}[a-z0-9])?$`)

// packNeed is what a request wants to do with a pack.
type packNeed int

const (
	packRead packNeed = iota
	packEdit
	packDelete
	// packEditPage: the pack's Modrinth project page (texts, tags, icon, submit).
	packEditPage
	// packDeletePack: removing the pack itself.
	packDeletePack
)

// packAccess loads the pack and reports whether the caller may do need on it.
//
// A pack bound to a Modrinth project follows that project's Modrinth team and
// nothing else - not even who created it here, so someone the team removed
// on Modrinth loses the pack here too (modrinth_access.go). An unbound
// (legacy) pack is its creator's alone. An admin may read any pack. Every pack
// and build handler asks this one function.
func (h *PacksHandler) packAccess(r *http.Request, packID int, need packNeed) (*models.Pack, bool) {
	p, err := h.state.Store.GetPack(packID)
	if err != nil || p == nil {
		return nil, false
	}
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		return nil, false
	}
	if isAdmin, _ := r.Context().Value("isAdmin").(bool); isAdmin && need == packRead {
		return p, true
	}
	if p.ModrinthProjectID == "" {
		return p, p.OwnerID == userID
	}
	return p, allows(h.membershipFor(r.Context(), userID, p.ModrinthProjectID), need)
}

// mayEditPack is packAccess's answer for an already loaded pack.
func (h *PacksHandler) mayEditPack(r *http.Request, p *models.Pack) bool {
	_, ok := h.packAccess(r, p.ID, packEdit)
	return ok
}

// ownsPack is packAccess for reading.
func (h *PacksHandler) ownsPack(r *http.Request, packID int) (*models.Pack, bool) {
	return h.packAccess(r, packID, packRead)
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

// sharedPack is a pack someone else created that the caller reaches through
// its Modrinth project's team, with what Modrinth allows them.
type sharedPack struct {
	models.Pack
	CanEdit      bool `json:"canEdit"`
	CanDelete    bool `json:"canDelete"`
	TokenInvalid bool `json:"tokenInvalid"`
}

// List GET /api/me/packs - the packs the caller created ("packs") and, under
// "shared", the packs they reach as a member of the pack's Modrinth team.
// "modrinth" says whether the caller's token works, so the panel can ask for
// a renewal instead of showing packs that silently stopped working.
func (h *PacksHandler) List(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		handlers.SendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	h.discoverTeamPacks(r.Context(), userID)
	own, err := h.state.Store.ListPacksByOwner(userID)
	if err != nil {
		handlers.SendJSONError(w, "Failed to list packs", http.StatusInternalServerError)
		return
	}
	packs := []models.Pack{}
	listed := map[int]bool{}
	tokenInvalid := false
	for _, p := range own {
		if p.ModrinthProjectID == "" {
			packs = append(packs, p)
			listed[p.ID] = true
		}
	}
	shared := []sharedPack{}
	memberships, err := h.state.Store.ListModrinthMemberships(userID)
	if err != nil {
		handlers.SendJSONError(w, "Failed to list packs", http.StatusInternalServerError)
		return
	}
	for _, cached := range memberships {
		p, err := h.state.Store.GetPackByModrinthProject(cached.ProjectID)
		if err != nil || p == nil || listed[p.ID] {
			continue
		}
		m := h.membershipFor(r.Context(), userID, cached.ProjectID)
		// A lapsed token stays listed, greyed out with the renew prompt, even
		// once its read grace is over; opening it then needs the renewal.
		lapsed := m != nil && m.State == stateTokenInvalid
		if !allows(m, packRead) && !lapsed {
			continue
		}
		listed[p.ID] = true
		if lapsed {
			tokenInvalid = true
		}
		if p.OwnerID == userID && !lapsed {
			packs = append(packs, *p)
			continue
		}
		shared = append(shared, sharedPack{Pack: *p, CanEdit: allows(m, packEdit), CanDelete: allows(m, packDelete), TokenInvalid: m.State == stateTokenInvalid})
	}
	connected := false
	if h.pat != nil {
		_, _, perr := h.pat.LoadPAT(userID)
		connected = perr == nil
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "packs": packs, "shared": shared,
		"modrinth": map[string]bool{"connected": connected, "tokenInvalid": tokenInvalid}})
}

// Create POST /api/me/packs - creates a pack together with its Modrinth
// project (a draft until submitted), with the caller's own token, so the
// project's team decides who works on it from the start.
func (h *PacksHandler) Create(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		handlers.SendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var req packRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		handlers.SendJSONError(w, "Name is required", http.StatusBadRequest)
		return
	}
	slug := strings.TrimSpace(req.Slug)
	if slug == "" {
		slug = handlers.Slugify(req.Name)
	}
	if !packSlugRe.MatchString(slug) {
		handlers.SendJSONError(w, "Invalid slug", http.StatusBadRequest)
		return
	}
	_, mc, ok := h.callerModrinth(w, r)
	if !ok {
		return
	}
	p := &models.Pack{
		OwnerID:            userID,
		InternalName:       req.Name,
		InternalSlug:       slug,
		Summary:            strings.TrimSpace(strVal(req.Summary)),
		ModrinthVisibility: "unlisted",
	}
	created, err := h.createModrinthProject(r, mc, p)
	if err != nil {
		if services.ModrinthTokenRejected(err) {
			handlers.SendJSONError(w, "Modrinth rejected your token. Renew it under Account, Modrinth.", http.StatusPreconditionRequired)
			return
		}
		handlers.SendJSONError(w, "Modrinth did not create the project (is the slug taken on Modrinth?): "+err.Error(), http.StatusBadGateway)
		return
	}
	p.ModrinthProjectID = created.ID
	p.ModrinthProjectName = created.Slug
	id, err := h.state.Store.CreatePack(p)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "duplicate") || strings.Contains(msg, "unique") {
			handlers.SendJSONError(w, "A pack with that slug already exists; the Modrinth draft "+created.ID+" was created and can be imported or deleted on Modrinth.", http.StatusConflict)
		} else {
			handlers.SendJSONError(w, "Failed to create pack", http.StatusInternalServerError)
		}
		return
	}
	p.ID = id
	h.grantMembership(userID, created.ID, services.ModrinthPermAll)
	h.audit(r, userID, id, "pack.created", map[string]any{"slug": slug, "modrinthProject": created.ID})
	h.state.Events.Publish(r.Context(), "packs.changed", map[string]interface{}{"ownerId": userID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "pack": p})
}

// Get GET /api/packs/{id} - one modpack. A pack the caller does not own is
// 403, not 404.
func (h *PacksHandler) Get(w http.ResponseWriter, r *http.Request) {
	packID, _ := strconv.Atoi(mux.Vars(r)["id"])
	p, ok := h.ownsPack(r, packID)
	if !ok {
		handlers.SendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	_, canEdit := h.packAccess(r, packID, packEdit)
	_, canDelete := h.packAccess(r, packID, packDelete)
	_, canEditPage := h.packAccess(r, packID, packEditPage)
	_, canDeletePack := h.packAccess(r, packID, packDeletePack)
	tokenInvalid := false
	if p.ModrinthProjectID != "" {
		caller, _ := r.Context().Value("userID").(string)
		if m, _ := h.state.Store.GetModrinthMembership(caller, p.ModrinthProjectID); m != nil && m.State == stateTokenInvalid {
			tokenInvalid = true
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "pack": p,
		// isOwner: may manage the pack as a whole (its Modrinth project's
		// delete right, or the creator of an unbound pack).
		"permissions": map[string]bool{"isOwner": canDeletePack, "canEdit": canEdit, "canDelete": canDelete, "canEditPage": canEditPage, "canDeletePack": canDeletePack, "tokenInvalid": tokenInvalid},
	})
}

// Update PATCH /api/packs/{id} - edits a modpack the caller owns.
func (h *PacksHandler) Update(w http.ResponseWriter, r *http.Request) {
	packID, _ := strconv.Atoi(mux.Vars(r)["id"])
	p, ok := h.packAccess(r, packID, packEdit)
	if !ok {
		handlers.SendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	var req packRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if n := strings.TrimSpace(req.Name); n != "" {
		p.InternalName = n
	}
	if req.Summary != nil {
		p.Summary = strings.TrimSpace(*req.Summary)
	}
	if err := h.state.Store.UpdatePack(p); err != nil {
		handlers.SendJSONError(w, "Failed to update", http.StatusInternalServerError)
		return
	}
	h.state.Events.Publish(r.Context(), "packs.changed", map[string]interface{}{"ownerId": p.OwnerID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "pack": p})
}

// Delete DELETE /api/packs/{id} - deletes a modpack the caller owns.
func (h *PacksHandler) Delete(w http.ResponseWriter, r *http.Request) {
	packID, _ := strconv.Atoi(mux.Vars(r)["id"])
	p, ok := h.packAccess(r, packID, packDeletePack)
	if !ok {
		handlers.SendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	dirs := h.state.PackDirsOfPack(p)
	if err := h.state.Store.DeletePack(packID, p.OwnerID); err != nil {
		handlers.SendJSONError(w, "Failed to delete", http.StatusInternalServerError)
		return
	}
	h.state.DropPackDirs(dirs)
	h.state.Events.Publish(r.Context(), "packs.changed", map[string]interface{}{"ownerId": p.OwnerID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

type buildRequest struct {
	SourceBuildID int    `json:"sourceBuildId"`
	VersionString string `json:"versionString"`
	Minecraft     string `json:"minecraft"`
	Loader        string `json:"loader"`
	LoaderVersion string `json:"loaderVersion"`
	Changelog     string `json:"changelog"`
}

type buildPatchRequest struct {
	VersionString *string `json:"versionString"`
	Minecraft     *string `json:"minecraft"`
	Loader        *string `json:"loader"`
	LoaderVersion *string `json:"loaderVersion"`
	Changelog     *string `json:"changelog"`
}

// ListBuilds GET /api/packs/{id}/builds - the builds of one modpack.
func (h *PacksHandler) ListBuilds(w http.ResponseWriter, r *http.Request) {
	packID, _ := strconv.Atoi(mux.Vars(r)["id"])
	if _, ok := h.ownsPack(r, packID); !ok {
		handlers.SendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	builds, err := h.state.Store.ListPackBuilds(packID)
	if err != nil {
		handlers.SendJSONError(w, "Failed to list builds", http.StatusInternalServerError)
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
	_, ok := h.packAccess(r, packID, packEdit)
	if !ok {
		handlers.SendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	var req buildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.VersionString) == "" {
		handlers.SendJSONError(w, "versionString is required", http.StatusBadRequest)
		return
	}
	// reject path chars: VersionString feeds the download filename and the node-side install paths
	if !safeKeyComponent(strings.TrimSpace(req.VersionString)) {
		handlers.SendJSONError(w, "versionString contains invalid path characters", http.StatusBadRequest)
		return
	}
	if bad := validateBuildKeyComponents(strings.TrimSpace(req.Minecraft), strings.TrimSpace(req.Loader), strings.TrimSpace(req.LoaderVersion)); bad != "" {
		handlers.SendJSONError(w, bad+" contains invalid path characters", http.StatusBadRequest)
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
	if req.SourceBuildID != 0 {
		source, err := h.state.Store.GetPackBuild(req.SourceBuildID)
		if err != nil || source == nil || source.PackID != packID {
			handlers.SendJSONError(w, "Source build not found", http.StatusNotFound)
			return
		}
		if source.Minecraft != b.Minecraft || source.Loader != b.Loader {
			handlers.SendJSONError(w, "Copying requires the same Minecraft version and loader; use Minecraft migration for another environment", http.StatusUnprocessableEntity)
			return
		}
		b.MinJava, b.MinMemory = source.MinJava, source.MinMemory
	}
	var id int
	var err error
	if req.SourceBuildID != 0 {
		id, err = h.state.Store.ClonePackBuild(r.Context(), b, req.SourceBuildID)
	} else {
		id, err = h.state.Store.CreatePackBuild(b)
	}
	if err != nil {
		handlers.SendJSONError(w, "Failed to create build (version may exist)", http.StatusConflict)
		return
	}
	b.ID = id
	publishPackEvent(r, h.state, "pack_builds.changed", packID, map[string]interface{}{"packId": packID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "build": b})
}

// UpdateBuild PATCH /api/packs/{id}/builds/{buildId} - edits a build. The
// build must belong to the pack in the path (404 otherwise).
func (h *PacksHandler) UpdateBuild(w http.ResponseWriter, r *http.Request) {
	packID, _ := strconv.Atoi(mux.Vars(r)["id"])
	buildID, _ := strconv.Atoi(mux.Vars(r)["buildId"])
	p, ok := h.packAccess(r, packID, packEdit)
	if !ok {
		handlers.SendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	b, err := h.state.Store.GetPackBuild(buildID)
	if err != nil || b == nil || b.PackID != packID {
		handlers.SendJSONError(w, "Build not found", http.StatusNotFound)
		return
	}
	if b.Frozen || b.ModrinthPublished {
		handlers.SendJSONError(w, "Build is published and frozen", http.StatusConflict)
		return
	}
	var patch buildPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	// PATCH must distinguish an omitted setting from intentionally clearing it.
	req := buildRequest{VersionString: b.VersionString, Minecraft: b.Minecraft, Loader: b.Loader, LoaderVersion: b.LoaderVersion, Changelog: b.Changelog}
	if patch.VersionString != nil {
		req.VersionString = *patch.VersionString
		if strings.TrimSpace(req.VersionString) == "" {
			handlers.SendJSONError(w, "versionString is required", http.StatusBadRequest)
			return
		}
	}
	if patch.Minecraft != nil {
		req.Minecraft = *patch.Minecraft
	}
	if patch.Loader != nil {
		req.Loader = *patch.Loader
	}
	if patch.LoaderVersion != nil {
		req.LoaderVersion = *patch.LoaderVersion
	}
	if patch.Changelog != nil {
		req.Changelog = *patch.Changelog
	}
	// The version string is part of the derived directory: a rename moves
	// where the next render lands and strands the objects of the old one.
	oldDirs := h.state.PackBuildDirs(p, b)
	if v := strings.TrimSpace(req.VersionString); v != "" {
		if !safeKeyComponent(v) {
			handlers.SendJSONError(w, "versionString contains invalid path characters", http.StatusBadRequest)
			return
		}
		b.VersionString = v
	}
	if bad := validateBuildKeyComponents(strings.TrimSpace(req.Minecraft), strings.TrimSpace(req.Loader), strings.TrimSpace(req.LoaderVersion)); bad != "" {
		handlers.SendJSONError(w, bad+" contains invalid path characters", http.StatusBadRequest)
		return
	}
	b.Minecraft = strings.TrimSpace(req.Minecraft)
	b.Loader = strings.TrimSpace(req.Loader)
	b.LoaderVersion = strings.TrimSpace(req.LoaderVersion)
	b.Changelog = req.Changelog
	if err := h.state.Store.UpdatePackBuild(b); err != nil {
		handlers.SendJSONError(w, "Failed to update build", http.StatusInternalServerError)
		return
	}
	if newDirs := h.state.PackBuildDirs(p, b); newDirs[0] != oldDirs[0] {
		h.state.DropPackDirs(oldDirs[:1])
	}
	publishPackEvent(r, h.state, "pack_builds.changed", packID, map[string]interface{}{"packId": packID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "build": b})
}

// DeleteBuild DELETE /api/packs/{id}/builds/{buildId} - deletes a build that
// belongs to the pack in the path. Frozen/published builds require a JSON
// confirmVersion matching the exact version. External Modrinth versions remain.
func (h *PacksHandler) DeleteBuild(w http.ResponseWriter, r *http.Request) {
	packID, _ := strconv.Atoi(mux.Vars(r)["id"])
	buildID, _ := strconv.Atoi(mux.Vars(r)["buildId"])
	p, ok := h.packAccess(r, packID, packDelete)
	if !ok {
		handlers.SendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	b, err := h.state.Store.GetPackBuild(buildID)
	if err != nil || b == nil || b.PackID != packID {
		handlers.SendJSONError(w, "Build not found", http.StatusNotFound)
		return
	}
	if b.Frozen || b.ModrinthPublished {
		var req struct {
			ConfirmVersion string `json:"confirmVersion"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.ConfirmVersion != b.VersionString {
			handlers.SendJSONError(w, "Confirm the exact build version to delete a frozen or published build", http.StatusConflict)
			return
		}
	}
	dirs := h.state.PackBuildDirs(p, b)
	if err := h.state.Store.DeletePackBuild(buildID, packID); err != nil {
		handlers.SendJSONError(w, "Failed to delete build", http.StatusInternalServerError)
		return
	}
	h.state.DropPackDirs(dirs)
	publishPackEvent(r, h.state, "pack_builds.changed", packID, map[string]interface{}{"packId": packID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}
