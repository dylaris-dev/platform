package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"sort"

	"dylaris-core/models"

	"github.com/gorilla/mux"
)

// SolderHandler serves the public, unauthenticated Technic-Launcher-compatible
// Solder REST API + the file mirror. It lives on the /solder subrouter, which has
// NO middleware — the modpacks feature is gated in-handler with a Solder-shaped
// {"error":...} JSON body (not the 503 feature middleware).
type SolderHandler struct {
	state *AppState
}

func NewSolderHandler(state *AppState) *SolderHandler {
	return &SolderHandler{state: state}
}

// solderJSON writes v as application/json with the given status.
func solderJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// solderJSONError writes the Solder-contract {"error": msg} body with a status.
func solderJSONError(w http.ResponseWriter, msg string, code int) {
	solderJSON(w, code, map[string]string{"error": msg})
}

// modpacksEnabled gates the feature-sensitive endpoints with a Solder-shaped body.
func (h *SolderHandler) modpacksEnabled(w http.ResponseWriter, r *http.Request) bool {
	if !h.state.FeatureFlags.IsModpacksEnabled(r.Context()) {
		solderJSONError(w, "Modpacks are disabled", http.StatusForbidden)
		return false
	}
	return true
}

// solderKeyHash hashes a plaintext Solder API key the same way the store hashes
// it at rest (sha256 hex), so GetSolderKeyByHash matches. Kept local to avoid
// exporting the store's unexported hashAuthToken.
func solderKeyHash(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// solderAuth is the resolved access context for one public read request.
type solderAuth struct {
	hasKey   bool   // a valid ?k= was supplied → sees packs owned by ownerID
	ownerID  string // the key's own owner; only meaningful when hasKey is true
	clientID int    // >0 when a valid ?cid= was supplied → sees whitelisted packs
}

// resolveSolderAuth reads ?k= and ?cid= and resolves them to an access context.
// Verification is an indexed DB lookup (hash the key; look up the uuid) — no
// in-Go secret comparison. Unknown values resolve to no access (never an error
// to the caller): a bad key/cid simply grants nothing, matching the launcher
// contract (never 403 on the read path).
func (h *SolderHandler) resolveSolderAuth(r *http.Request) solderAuth {
	var a solderAuth
	if k := r.URL.Query().Get("k"); k != "" {
		if key, err := h.state.Store.GetSolderKeyByHash(solderKeyHash(k)); err == nil && key != nil {
			a.hasKey = true
			a.ownerID = key.OwnerID
		}
	}
	if cid := r.URL.Query().Get("cid"); cid != "" {
		if c, err := h.state.Store.GetSolderClientByUUID(cid); err == nil && c != nil {
			a.clientID = c.ID
		}
	}
	return a
}

// canAccessPack reports whether this auth context unlocks a gated (private or
// hidden) pack owned by packOwnerID. A valid key unlocks ONLY packs owned by
// that SAME key owner (BC5: keys are owner-scoped, never global); a client
// unlocks a pack only if it is on that pack's whitelist. A non-gated pack
// should not be routed here.
//
// The two credentials are asked in turn, not as an either/or. The key branch
// used to RETURN its answer, so a launcher carrying both a key and a client id
// - which is the ordinary Technic setup - got a flat "no" for every pack
// outside its key owner, and the client whitelist was never consulted at all.
// A pack that client was deliberately granted answered 404 "Modpack does not
// exist", and only for callers who also had a key of their own.
func (h *SolderHandler) canAccessPack(a solderAuth, packID int, packOwnerID string) bool {
	if a.hasKey && a.ownerID == packOwnerID {
		return true
	}
	if a.clientID > 0 {
		ok, err := h.state.Store.IsPackClient(packID, a.clientID)
		return err == nil && ok
	}
	return false
}

// solderVisiblePacks is every pack this auth context may list: the public
// catalogue, PLUS the gated packs the key or the client unlocks.
//
// A union, not a switch. Classic TechnicSolder's fetchModpacks walks every pack
// and only asks the key/client question about the gated ones, so presenting a
// credential ADDS packs - it never takes the public catalogue away. Written as
// a three-way switch it did exactly that: a launcher configured with a Solder
// key listed only that key owner's packs and every other owner's PUBLIC pack
// vanished from it, and a launcher with only a client id saw its whitelist and
// nothing else. Both are silent - the launcher shows a shorter list, with
// nothing anywhere saying why.
func (h *SolderHandler) solderVisiblePacks(a solderAuth) ([]models.Pack, error) {
	packs, err := h.state.Store.ListPublicSolderPacks()
	if err != nil {
		return nil, err
	}
	seen := make(map[int]bool, len(packs))
	for i := range packs {
		seen[packs[i].ID] = true
	}
	add := func(extra []models.Pack, err error) error {
		if err != nil {
			return err
		}
		for i := range extra {
			if !seen[extra[i].ID] {
				seen[extra[i].ID] = true
				packs = append(packs, extra[i])
			}
		}
		return nil
	}
	if a.hasKey {
		if err := add(h.state.Store.ListAllSolderPacks(a.ownerID)); err != nil {
			return nil, err
		}
	}
	if a.clientID > 0 {
		if err := add(h.state.Store.ListSolderPacksForClient(a.clientID)); err != nil {
			return nil, err
		}
	}
	// Each query orders by internal_name on its own; the merged list has to be
	// re-sorted or the unlocked packs simply trail the public ones.
	sort.Slice(packs, func(i, j int) bool { return packs[i].InternalName < packs[j].InternalName })
	return packs, nil
}

// Info is GET /solder/api/ — the root probe. version/stream are our own values;
// only the KEY NAMES (api/version/stream) are contractual. Not feature-gated.
func (h *SolderHandler) Info(w http.ResponseWriter, r *http.Request) {
	solderJSON(w, http.StatusOK, map[string]string{
		"api":     "TechnicSolder",
		"version": "1.0.0",
		"stream":  "stable",
	})
}

// VerifyKey is GET /solder/api/verify/{key}. Validates a Solder API key by hash
// lookup. 200 {"valid":..,"name":..} on match, 403 {"error":..} otherwise.
func (h *SolderHandler) VerifyKey(w http.ResponseWriter, r *http.Request) {
	if !h.modpacksEnabled(w, r) {
		return
	}
	key := mux.Vars(r)["key"]
	if key == "" {
		solderJSONError(w, "Invalid key provided.", http.StatusForbidden)
		return
	}
	k, err := h.state.Store.GetSolderKeyByHash(solderKeyHash(key))
	if err != nil {
		log.Printf("solder VerifyKey: store error: %v", err)
		solderJSONError(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if k == nil {
		solderJSONError(w, "Invalid key provided.", http.StatusForbidden)
		return
	}
	// created_at is part of the shape upstream TechnicSolder has answered with
	// for years, and it was missing here. Whether the Technic Platform reads it
	// is not something this side can verify, so it is emitted rather than
	// argued about: a field nobody reads costs nothing, and a missing field a
	// client does read rejects a key that is perfectly valid.
	//
	// "2006-01-02 15:04:05" UTC, which is what the reference implementation
	// emitted for most of its life (Laravel's pre-7 Carbon serialisation).
	// Newer Solder builds emit ISO-8601 instead, so no client can be parsing
	// this strictly - and where one is, it was written against the older shape.
	solderJSON(w, http.StatusOK, map[string]string{
		"valid":      "Key validated.",
		"name":       k.Name,
		"created_at": k.CreatedAt.UTC().Format("2006-01-02 15:04:05"),
	})
}

// solderModpackObject is the single-modpack object shape (shared by GetModpack
// and include=full listing). name = SLUG, display_name = human name.
type solderModpackObject struct {
	Name          string   `json:"name"`
	DisplayName   string   `json:"display_name"`
	URL           string   `json:"url"`
	Icon          string   `json:"icon"`
	IconMD5       string   `json:"icon_md5"`
	Logo          string   `json:"logo"`
	LogoMD5       string   `json:"logo_md5"`
	Background    string   `json:"background"`
	BackgroundMD5 string   `json:"background_md5"`
	Recommended   string   `json:"recommended"`
	Latest        string   `json:"latest"`
	Builds        []string `json:"builds"`
}

// ListModpacks is GET /solder/api/modpack. Default: {modpacks:{slug:displayName},
// mirror_url}. ?include=full: each value is the full modpack object. mirror_url
// ALWAYS ends with a trailing slash.
func (h *SolderHandler) ListModpacks(w http.ResponseWriter, r *http.Request) {
	if !h.modpacksEnabled(w, r) {
		return
	}
	packs, err := h.solderVisiblePacks(h.resolveSolderAuth(r))
	if err != nil {
		solderJSONError(w, "Failed to list modpacks", http.StatusInternalServerError)
		return
	}
	base, err := solderMirrorBase(h.state.Store.GetSetting)
	if err != nil {
		solderJSONError(w, "Mirror not configured", http.StatusInternalServerError)
		return
	}
	if r.URL.Query().Get("include") == "full" {
		full := make(map[string]interface{}, len(packs))
		for i := range packs {
			obj, err := h.modpackObject(&packs[i])
			if err != nil {
				solderJSONError(w, "Failed to build modpack", http.StatusInternalServerError)
				return
			}
			full[packs[i].SolderSlug] = obj
		}
		solderJSON(w, http.StatusOK, map[string]interface{}{"modpacks": full, "mirror_url": base})
		return
	}
	names := make(map[string]string, len(packs))
	for i := range packs {
		names[packs[i].SolderSlug] = packs[i].SolderDisplayName
	}
	solderJSON(w, http.StatusOK, map[string]interface{}{"modpacks": names, "mirror_url": base})
}

// modpackObject builds one solderModpackObject (name = slug, display_name = human
// name) with its published build version strings.
func (h *SolderHandler) modpackObject(p *models.Pack) (solderModpackObject, error) {
	builds, err := h.state.Store.ListSolderPublishedBuilds(p.ID)
	if err != nil {
		return solderModpackObject{}, err
	}
	versions := make([]string, 0, len(builds))
	for _, b := range builds {
		versions = append(versions, b.VersionString)
	}
	return solderModpackObject{
		Name:          p.SolderSlug,
		DisplayName:   p.SolderDisplayName,
		URL:           "",
		Icon:          p.IconURL,
		IconMD5:       p.IconMD5,
		Logo:          p.LogoURL,
		LogoMD5:       p.LogoMD5,
		Background:    p.BackgroundURL,
		BackgroundMD5: p.BackgroundMD5,
		Recommended:   p.RecommendedBuild,
		Latest:        p.LatestBuild,
		Builds:        versions,
	}, nil
}

// GetModpack is GET /solder/api/modpack/{slug}. 404 (Solder-shaped) when the pack
// does not exist or is private/hidden without valid auth.
func (h *SolderHandler) GetModpack(w http.ResponseWriter, r *http.Request) {
	if !h.modpacksEnabled(w, r) {
		return
	}
	slug := mux.Vars(r)["slug"]
	p, err := h.state.Store.GetPackBySolderSlug(slug)
	if err != nil {
		solderJSONError(w, "Failed to load modpack", http.StatusInternalServerError)
		return
	}
	if p == nil {
		solderJSONError(w, "Modpack does not exist", http.StatusNotFound)
		return
	}
	if p.Private || p.Hidden {
		if !h.canAccessPack(h.resolveSolderAuth(r), p.ID, p.OwnerID) {
			solderJSONError(w, "Modpack does not exist", http.StatusNotFound)
			return
		}
	}
	obj, err := h.modpackObject(p)
	if err != nil {
		solderJSONError(w, "Failed to build modpack", http.StatusInternalServerError)
		return
	}
	solderJSON(w, http.StatusOK, obj)
}

// solderBuildMod is one launcher-facing mods[] entry. url = mirrorBase + solderKey.
type solderBuildMod struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	MD5      string `json:"md5"`
	URL      string `json:"url"`
	Filesize int64  `json:"filesize"`
}

// GetBuild is GET /solder/api/modpack/{slug}/{build} — the differential-update
// payload. Reads the deterministic manifest and projects it into the launcher's
// build shape. java omitted when empty, memory omitted when 0, mods never null.
func (h *SolderHandler) GetBuild(w http.ResponseWriter, r *http.Request) {
	if !h.modpacksEnabled(w, r) {
		return
	}
	vars := mux.Vars(r)
	p, err := h.state.Store.GetPackBySolderSlug(vars["slug"])
	if err != nil {
		solderJSONError(w, "Failed to load modpack", http.StatusInternalServerError)
		return
	}
	if p == nil {
		solderJSONError(w, "Modpack does not exist", http.StatusNotFound)
		return
	}
	if p.Private || p.Hidden {
		if !h.canAccessPack(h.resolveSolderAuth(r), p.ID, p.OwnerID) {
			solderJSONError(w, "Modpack does not exist", http.StatusNotFound)
			return
		}
	}
	b, err := h.state.Store.GetPackBuildByVersion(p.ID, vars["build"])
	if err != nil {
		solderJSONError(w, "Failed to load build", http.StatusInternalServerError)
		return
	}
	if b == nil || !b.SolderPublished {
		solderJSONError(w, "Build does not exist", http.StatusNotFound)
		return
	}

	prov, err := h.state.buildModpackStorageProvider()
	if err != nil || prov == nil {
		solderJSONError(w, "Storage not configured", http.StatusInternalServerError)
		return
	}
	manifestJSON, err := prov.Get(r.Context(), solderManifestKey(p.OwnerID, p.SolderSlug, b.VersionString))
	if err != nil {
		solderJSONError(w, "Build does not exist", http.StatusNotFound)
		return
	}
	var m solderManifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		solderJSONError(w, "Corrupt build manifest", http.StatusInternalServerError)
		return
	}
	gated := p.Private || p.Hidden
	mods := make([]solderBuildMod, 0, len(m.Mods)) // never null => []
	for _, md := range m.Mods {
		u, uerr := solderModURL(r.Context(), h.state.Store.GetSetting, prov, md.SolderKey, gated)
		if uerr != nil {
			solderJSONError(w, "Storage delivery not configured", http.StatusInternalServerError)
			return
		}
		mods = append(mods, solderBuildMod{
			Name:     md.Name,
			Version:  md.Version,
			MD5:      md.MD5,
			URL:      u,
			Filesize: md.Filesize,
		})
	}

	out := map[string]interface{}{
		"minecraft": m.Minecraft,
		"mods":      mods,
	}
	if m.Java != "" {
		out["java"] = m.Java
	}
	if m.Memory > 0 {
		out["memory"] = m.Memory
	}
	solderJSON(w, http.StatusOK, out)
}
