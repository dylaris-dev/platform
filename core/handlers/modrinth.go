package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"dylaris-core/services"
)

// Modrinth API proxy. Mods/plugins browse, version metadata,
// project details. Cached in Redis to keep panel browse traffic off
// Modrinth's rate limit and to make the UI feel instant.
//
// User-Agent: services.DylarisUserAgent, the one string every outbound call
// carries.

const (
	modrinthBaseURL     = "https://api.modrinth.com/v2"
	modrinthSearchTTL   = 5 * time.Minute
	modrinthProjectTTL  = 1 * time.Hour
	modrinthCategoryTTL = 24 * time.Hour
)

type ModrinthHandler struct {
	state *AppState
	http  *http.Client
	// cool is Modrinth's 429: the limit is per IP and Core is one IP for
	// every customer, so one busy panel must not turn into a ban for all.
	cool upstreamCooldown
}

func NewModrinthHandler(state *AppState) *ModrinthHandler {
	return &ModrinthHandler{
		state: state,
		http:  &http.Client{Timeout: 15 * time.Second},
	}
}

// maxCachedResponseBytes caps what one cache entry may weigh.
//
// Measured against the live API: a project's version list runs 290 KB for
// Sodium filtered to one loader, 494 KB unfiltered, and 1.19 MB for Fabric API.
// The proxy keys those per filter combination for an hour and the shipped Redis
// runs with no maxmemory, so without a cap the tail of that distribution is what
// decides how much memory the control plane has left. Over the cap the response
// is served straight through: one extra upstream call, no stored megabyte.
const maxCachedResponseBytes = 512 << 10

// proxyJSON fetches the URL with caching. The cache key is a sha256 of the
// full URL so we don't accidentally collide between similar queries.
func (h *ModrinthHandler) proxyJSON(ctx context.Context, ttl time.Duration, urlStr string, w http.ResponseWriter) {
	h.proxyJSONWith(ctx, ttl, urlStr, w, nil)
}

// proxyJSONWith is proxyJSON with an optional transform applied to the upstream
// body BEFORE it is cached and before it is written, so a cache hit and a cache
// miss return the same document.
func (h *ModrinthHandler) proxyJSONWith(ctx context.Context, ttl time.Duration, urlStr string, w http.ResponseWriter, transform func([]byte) []byte) {
	cacheKey := "dylaris:modrinth:" + hashURL(urlStr)
	if h.state.Cache != nil {
		if cached, ok := h.state.Cache.Get(ctx, cacheKey); ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			w.Write([]byte(cached))
			return
		}
	}

	if left, ok := h.cool.blocked(time.Now()); ok {
		sendCooldown(w, "Modrinth", left)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		sendJSONError(w, "Bad upstream request", http.StatusBadGateway)
		return
	}
	req.Header.Set("User-Agent", services.DylarisUserAgent())
	req.Header.Set("Accept", "application/json")

	resp, err := h.http.Do(req)
	if err != nil {
		sendJSONError(w, "Upstream call failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		sendCooldown(w, "Modrinth", h.cool.trip(resp.Header, time.Now()))
		return
	}
	body, _ := io.ReadAll(resp.Body)

	// Transform only a successful body: an error response has a different shape
	// and must reach the caller as it arrived.
	if transform != nil && resp.StatusCode == http.StatusOK {
		body = transform(body)
	}

	// Cache only successful 200s — error responses change shape and
	// shouldn't poison the cache for 5+ minutes.
	if resp.StatusCode == http.StatusOK && h.state.Cache != nil && len(body) <= maxCachedResponseBytes {
		h.state.Cache.Set(ctx, cacheKey, string(body), ttl)
	}

	w.Header().Set("Content-Type", "application/json")
	if resp.StatusCode == http.StatusOK {
		w.Header().Set("X-Cache", "MISS")
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

func hashURL(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

// Search GET /api/modrinth/search
// Query params (Modrinth-compatible): query, limit, offset, facets (JSON
// array of arrays), loaders (comma list), versions (comma list), categories
// (comma list), project_type.
//
// We re-translate loaders/versions/categories into facets for Modrinth's API
// since the facets shape is the actual filter mechanism; the comma params
// are panel-friendly aliases.
func (h *ModrinthHandler) Search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	values := url.Values{}
	if v := strings.TrimSpace(q.Get("query")); v != "" {
		if len(v) > 200 { // bound the cache-key / upstream query size
			v = v[:200]
		}
		values.Set("query", v)
	}
	if v := q.Get("limit"); v != "" {
		values.Set("limit", v)
	}
	if v := q.Get("offset"); v != "" {
		values.Set("offset", v)
	}
	if v := q.Get("index"); v != "" {
		values.Set("index", v)
	}

	// Facets: [["categories:fabric"],["versions:1.20.2"],["project_type:mod"]]
	type facet = []string
	facets := []facet{}
	addFacet := func(prefix, csv string) {
		csv = strings.TrimSpace(csv)
		if csv == "" {
			return
		}
		for _, item := range strings.Split(csv, ",") {
			item = strings.TrimSpace(item)
			if item != "" {
				facets = append(facets, facet{prefix + ":" + item})
			}
		}
	}
	addFacet("categories", q.Get("loaders"))
	addFacet("versions", q.Get("versions"))
	addFacet("categories", q.Get("categories"))
	addFacet("project_type", q.Get("project_type"))

	if len(facets) > 0 {
		raw, _ := json.Marshal(facets)
		values.Set("facets", string(raw))
	}

	upstream := modrinthBaseURL + "/search"
	if encoded := values.Encode(); encoded != "" {
		upstream += "?" + encoded
	}
	h.proxyJSON(r.Context(), modrinthSearchTTL, upstream, w)
}

// Project GET /api/modrinth/project/{slug} — full project metadata.
func (h *ModrinthHandler) Project(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.URL.Query().Get("slug"))
	if slug == "" {
		// Path-style: /api/modrinth/project/<slug>
		slug = strings.TrimPrefix(r.URL.Path, "/api/modrinth/project/")
	}
	if slug == "" {
		sendJSONError(w, "slug required", http.StatusBadRequest)
		return
	}
	upstream := fmt.Sprintf("%s/project/%s", modrinthBaseURL, url.PathEscape(slug))
	h.proxyJSON(r.Context(), modrinthProjectTTL, upstream, w)
}

// ProjectVersions GET /api/modrinth/project/{slug}/versions - a cached proxy
// to Modrinth's version list. The slug may arrive in the path or as ?slug=,
// and the ?loaders and ?versions filters are passed upstream.
func (h *ModrinthHandler) ProjectVersions(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.URL.Query().Get("slug"))
	if slug == "" {
		path := strings.TrimPrefix(r.URL.Path, "/api/modrinth/project/")
		path = strings.TrimSuffix(path, "/versions")
		slug = path
	}
	if slug == "" {
		sendJSONError(w, "slug required", http.StatusBadRequest)
		return
	}
	values := url.Values{}
	if loaders := r.URL.Query().Get("loaders"); loaders != "" {
		raw, _ := json.Marshal(strings.Split(loaders, ","))
		values.Set("loaders", string(raw))
	}
	if versions := r.URL.Query().Get("versions"); versions != "" {
		raw, _ := json.Marshal(strings.Split(versions, ","))
		values.Set("game_versions", string(raw))
	}
	upstream := fmt.Sprintf("%s/project/%s/version", modrinthBaseURL, url.PathEscape(slug))
	if enc := values.Encode(); enc != "" {
		upstream += "?" + enc
	}
	// Every build is kept; only the changelog goes. See modrinth_slim.go for why
	// that is the bigger win than dropping older builds, and why dropping them
	// would cost a capability.
	h.proxyJSONWith(r.Context(), modrinthProjectTTL, upstream, w, stripVersionChangelogs)
}

// Version GET /api/modrinth/version/{id} — single version metadata.
// Used by the install path to pull the download URL + sha512 before sending
// the install command to the node.
func (h *ModrinthHandler) Version(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/modrinth/version/")
	if id == "" {
		sendJSONError(w, "version id required", http.StatusBadRequest)
		return
	}
	upstream := fmt.Sprintf("%s/version/%s", modrinthBaseURL, url.PathEscape(id))
	h.proxyJSONWith(r.Context(), modrinthProjectTTL, upstream, w, stripVersionChangelogs)
}

// GameVersions GET /api/modrinth/game-versions - Modrinth's Minecraft version
// tag list, newest first. The panel needs it to offer a specific migration
// target; the ORDER is the reason it comes from here rather than being sorted
// client-side, since a comparator would have to rank 1.21.11 above 1.21.2 and
// 26.2 above both. Cached for a day like the other tag lists.
func (h *ModrinthHandler) GameVersions(w http.ResponseWriter, r *http.Request) {
	h.proxyJSON(r.Context(), modrinthCategoryTTL, modrinthBaseURL+"/tag/game_version", w)
}

// Categories GET /api/modrinth/categories — the Modrinth category tag list
// (each entry: name, project_type, header, icon SVG). Very stable, so it is
// cached for a day; the panel uses it to build the always-visible category
// sidebar in the Content tab.
func (h *ModrinthHandler) Categories(w http.ResponseWriter, r *http.Request) {
	upstream := modrinthBaseURL + "/tag/category"
	h.proxyJSON(r.Context(), modrinthCategoryTTL, upstream, w)
}
