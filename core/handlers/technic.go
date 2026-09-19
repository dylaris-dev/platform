package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"dylaris-core/services"
)

// Technic Platform. Core reads pack metadata here, briefly cached, and decides
// at setup what the node installs (resolveTechnic). No pack file ever passes
// through Core: the node downloads from the author's host.
//
// The API is undocumented. Measured 2026-09-19: every call needs a `build`
// query parameter (401 without, 200 with any non-empty value; we send our own
// name rather than impersonating the launcher), an unknown pack answers 404,
// an empty search 400, and a burst at 1.5 s spacing got 429s - hence the cache
// and the shared cooldown.

const (
	technicAPIBase       = "https://api.technicpack.net"
	technicBuildParam    = "dylaris"
	technicSearchTTL     = 5 * time.Minute
	technicPackTTL       = 1 * time.Hour
	technicMaxBody       = 2 << 20
	technicSolderFetch   = 15 * time.Second
	technicSolderModsMax = 2000
)

var (
	technicSlugRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)
	technicBuildRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

	errTechnicNotFound = errors.New("technic pack not found")

	// One cooldown for every handler instance: the setup path and the proxy
	// routes share Technic's per-IP limit.
	technicCooldown upstreamCooldown
)

// technicUserError is a refusal the panel shows as it is (422).
type technicUserError struct{ msg string }

func (e technicUserError) Error() string { return e.msg }

// technicCoolingDown is returned while Technic's 429 is in force.
type technicCoolingDown struct{ left time.Duration }

func (e technicCoolingDown) Error() string { return "Technic is rate limiting requests" }

type TechnicHandler struct {
	state *AppState
	http  *http.Client
	base  string
	// fetchExternal reads a Solder API. Its host is the pack author's, so it
	// goes through the SSRF-guarded fetch; a variable only for tests.
	fetchExternal func(ctx context.Context, rawURL string, maxBytes int64, timeout time.Duration) ([]byte, error)
}

func NewTechnicHandler(state *AppState) *TechnicHandler {
	return &TechnicHandler{
		state:         state,
		http:          &http.Client{Timeout: 15 * time.Second},
		base:          technicAPIBase,
		fetchExternal: services.SafeFetch,
	}
}

// technicPack is the subset of /modpack/<slug> we use.
type technicPack struct {
	Name          string  `json:"name"`
	DisplayName   string  `json:"displayName"`
	User          string  `json:"user"`
	URL           *string `json:"url"`
	PlatformURL   string  `json:"platformUrl"`
	Minecraft     string  `json:"minecraft"`
	Version       string  `json:"version"`
	Solder        *string `json:"solder"`
	ServerPackURL *string `json:"serverPackUrl"`
	Icon          *struct {
		URL string `json:"url"`
	} `json:"icon"`
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// variant is what the node will install, in order of preference.
func (p *technicPack) variant() string {
	switch {
	case str(p.ServerPackURL) != "":
		return "server"
	case str(p.URL) != "":
		return "client-zip"
	case str(p.Solder) != "":
		return "client-solder"
	}
	return "none"
}

type solderModpack struct {
	Recommended string   `json:"recommended"`
	Latest      string   `json:"latest"`
	Builds      []string `json:"builds"`
}

type solderBuild struct {
	Minecraft string `json:"minecraft"`
	Mods      []struct {
		URL string `json:"url"`
		MD5 string `json:"md5"`
	} `json:"mods"`
}

// apiGet reads one Technic API document through the cache and the cooldown.
func (h *TechnicHandler) apiGet(ctx context.Context, path string, q url.Values, ttl time.Duration) ([]byte, error) {
	q.Set("build", technicBuildParam)
	u := h.base + path + "?" + q.Encode()
	key := "dylaris:technic:" + hashURL(u)
	if h.state != nil && h.state.Cache != nil {
		if cached, ok := h.state.Cache.Get(ctx, key); ok {
			return []byte(cached), nil
		}
	}
	if left, ok := technicCooldown.blocked(time.Now()); ok {
		return nil, technicCoolingDown{left}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", services.DylarisUserAgent())
	req.Header.Set("Accept", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("technic: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return nil, technicCoolingDown{technicCooldown.trip(resp.Header, time.Now())}
	case http.StatusNotFound:
		return nil, errTechnicNotFound
	default:
		return nil, fmt.Errorf("technic: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, technicMaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("technic: %w", err)
	}
	if len(body) > technicMaxBody {
		return nil, errors.New("technic: response too large")
	}
	if h.state != nil && h.state.Cache != nil && len(body) <= maxCachedResponseBytes {
		h.state.Cache.Set(ctx, key, string(body), ttl)
	}
	return body, nil
}

// solderGet reads a Solder API document, cached like the API.
func (h *TechnicHandler) solderGet(ctx context.Context, rawURL string, out any) error {
	key := "dylaris:technic:solder:" + hashURL(rawURL)
	if h.state != nil && h.state.Cache != nil {
		if cached, ok := h.state.Cache.Get(ctx, key); ok {
			return json.Unmarshal([]byte(cached), out)
		}
	}
	body, err := h.fetchExternal(ctx, rawURL, technicMaxBody, technicSolderFetch)
	if err != nil {
		return fmt.Errorf("the pack's Solder server did not answer: %w", err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("the pack's Solder server sent something unreadable: %w", err)
	}
	if h.state != nil && h.state.Cache != nil && len(body) <= maxCachedResponseBytes {
		h.state.Cache.Set(ctx, key, string(body), technicPackTTL)
	}
	return nil
}

func (h *TechnicHandler) fetchPack(ctx context.Context, slug string) (*technicPack, error) {
	body, err := h.apiGet(ctx, "/modpack/"+slug, url.Values{}, technicPackTTL)
	if err != nil {
		return nil, err
	}
	var p technicPack
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("technic: unreadable pack: %w", err)
	}
	if p.Name == "" {
		p.Name = slug
	}
	return &p, nil
}

func solderURL(base string, parts ...string) string {
	u := strings.TrimRight(base, "/") + "/modpack"
	for _, p := range parts {
		u += "/" + url.PathEscape(p)
	}
	return u
}

func (h *TechnicHandler) solderBuilds(ctx context.Context, p *technicPack) (*solderModpack, error) {
	var m solderModpack
	if err := h.solderGet(ctx, solderURL(str(p.Solder), p.Name), &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// TechnicResolution is what the node is told to install.
type TechnicResolution struct {
	Variant   string
	URL       string
	MCVersion string
	Build     string
	Mods      []map[string]string
}

func checkHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return technicUserError{"This Technic pack's download address is not an http(s) URL."}
	}
	return nil
}

// resolveTechnic fetches the pack NOW and decides what to install. The browser
// only names a slug and a build; every URL the node gets comes from here.
func (h *TechnicHandler) resolveTechnic(ctx context.Context, slug, build string) (*TechnicResolution, error) {
	if !technicSlugRe.MatchString(slug) {
		return nil, technicUserError{"Invalid Technic pack name."}
	}
	p, err := h.fetchPack(ctx, slug)
	if err != nil {
		return nil, err
	}
	res := &TechnicResolution{Variant: p.variant(), MCVersion: p.Minecraft, Build: p.Version}
	switch res.Variant {
	case "server":
		res.URL = str(p.ServerPackURL)
	case "client-zip":
		res.URL = str(p.URL)
	case "client-solder":
		builds, err := h.solderBuilds(ctx, p)
		if err != nil {
			return nil, err
		}
		if build == "" {
			build = builds.Recommended
		}
		if !technicBuildRe.MatchString(build) || !containsString(builds.Builds, build) {
			return nil, technicUserError{"That build does not exist for this Technic pack."}
		}
		var b solderBuild
		if err := h.solderGet(ctx, solderURL(str(p.Solder), p.Name, build), &b); err != nil {
			return nil, err
		}
		if len(b.Mods) == 0 {
			return nil, technicUserError{"This Technic build lists no files."}
		}
		// The node refuses more than this too (installer_technic.go); refusing
		// here answers the operator instead of failing on the node.
		if len(b.Mods) > technicSolderModsMax {
			return nil, technicUserError{"This Technic build lists more files than a server install allows."}
		}
		for _, m := range b.Mods {
			if err := checkHTTPURL(m.URL); err != nil {
				return nil, err
			}
			res.Mods = append(res.Mods, map[string]string{"url": m.URL, "md5": m.MD5})
		}
		res.Build = build
		if b.Minecraft != "" {
			res.MCVersion = b.Minecraft
		}
		return res, nil
	default:
		return nil, technicUserError{"This Technic pack offers no download that can be installed."}
	}
	if err := checkHTTPURL(res.URL); err != nil {
		return nil, err
	}
	return res, nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// sendTechnicError maps the errors above to what the panel shows.
func sendTechnicError(w http.ResponseWriter, err error) {
	var cool technicCoolingDown
	var user technicUserError
	switch {
	case errors.As(err, &cool):
		sendCooldown(w, "Technic", cool.left)
	case errors.As(err, &user):
		sendJSONError(w, user.msg, http.StatusUnprocessableEntity)
	case errors.Is(err, errTechnicNotFound):
		sendJSONError(w, "Technic pack not found.", http.StatusNotFound)
	default:
		sendJSONError(w, "Technic could not be reached: "+err.Error(), http.StatusBadGateway)
	}
}

// Search GET /api/technic/search?q= - searches Technic Platform modpacks by
// name through Core's cache.
func (h *TechnicHandler) Search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) > 100 {
		q = q[:100]
	}
	type hit struct {
		Slug    string `json:"slug"`
		Name    string `json:"name"`
		IconURL string `json:"iconUrl"`
	}
	out := struct {
		Packs []hit `json:"packs"`
	}{Packs: []hit{}}
	// Technic answers an empty query with 400; there is nothing to list.
	if q == "" {
		writeTechnicJSON(w, out)
		return
	}
	body, err := h.apiGet(r.Context(), "/search", url.Values{"q": {q}}, technicSearchTTL)
	if err != nil {
		sendTechnicError(w, err)
		return
	}
	var up struct {
		Modpacks []struct {
			Name    string `json:"name"`
			Slug    string `json:"slug"`
			IconURL string `json:"iconUrl"`
		} `json:"modpacks"`
	}
	if err := json.Unmarshal(body, &up); err != nil {
		sendTechnicError(w, fmt.Errorf("unreadable search result: %w", err))
		return
	}
	for _, m := range up.Modpacks {
		if technicSlugRe.MatchString(m.Slug) {
			out.Packs = append(out.Packs, hit{Slug: m.Slug, Name: m.Name, IconURL: m.IconURL})
		}
	}
	writeTechnicJSON(w, out)
}

type technicBuildsOut struct {
	Recommended string   `json:"recommended"`
	Latest      string   `json:"latest"`
	List        []string `json:"list"`
}

type technicPackOut struct {
	Slug        string            `json:"slug"`
	DisplayName string            `json:"displayName"`
	Author      string            `json:"author"`
	Minecraft   string            `json:"minecraft"`
	Version     string            `json:"version"`
	IconURL     string            `json:"iconUrl"`
	PlatformURL string            `json:"platformUrl"`
	Variant     string            `json:"variant"`
	Builds      *technicBuildsOut `json:"builds,omitempty"`
}

// Pack GET /api/technic/pack/{slug} - one Technic pack and what an install
// would use: its server pack, its client zip, or a Solder build.
func (h *TechnicHandler) Pack(w http.ResponseWriter, r *http.Request) {
	slug := mux.Vars(r)["slug"]
	if !technicSlugRe.MatchString(slug) {
		sendJSONError(w, "Invalid Technic pack name.", http.StatusBadRequest)
		return
	}
	p, err := h.fetchPack(r.Context(), slug)
	if err != nil {
		sendTechnicError(w, err)
		return
	}
	out := technicPackOut{
		Slug:        slug,
		DisplayName: p.DisplayName,
		Author:      p.User,
		Minecraft:   p.Minecraft,
		Version:     p.Version,
		PlatformURL: p.PlatformURL,
		Variant:     p.variant(),
	}
	if p.Icon != nil {
		out.IconURL = p.Icon.URL
	}
	if out.Variant == "client-solder" {
		b, err := h.solderBuilds(r.Context(), p)
		if err != nil {
			sendTechnicError(w, err)
			return
		}
		out.Builds = &technicBuildsOut{Recommended: b.Recommended, Latest: b.Latest, List: b.Builds}
	}
	writeTechnicJSON(w, out)
}

func writeTechnicJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
