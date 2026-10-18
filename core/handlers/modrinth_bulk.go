package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Bulk Modrinth lookups for the Content tab's Update all.
//
// Checking a server's mods one project at a time costs one request per mod, and
// Modrinth's limit (300/min) is per IP while Core is ONE IP for every customer:
// a 343-mod server alone was more than a minute of the whole platform's budget.
// These three routes answer the same question in a handful of calls. They
// forward to the fixed Modrinth endpoints only, with inputs re-encoded from
// validated values rather than passed through.

const (
	// modrinthBulkMaxIDs bounds one ids= list. The panel chunks above it; the
	// cap keeps the upstream URL far below any proxy's line limit.
	modrinthBulkMaxIDs = 100
	// modrinthBulkMaxHashes bounds one version-files lookup. 1000 sha512 hex
	// strings are ~130 KB, inside modrinthBulkBodyLimit.
	modrinthBulkMaxHashes = 1000
	modrinthBulkBodyLimit = 256 << 10
	modrinthBulkMaxFilter = 10
)

var (
	// Modrinth ids are base62.
	modrinthIDRe = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)
	// Loader tags: "fabric", "neoforge", "bungeecord", ...
	modrinthLoaderRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	// Game versions: "1.21.1", "24w14a", "1.20.5-pre1", "b1.7.3".
	modrinthGameVersionRe = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,32}$`)
	hexRe                 = regexp.MustCompile(`^[0-9a-f]+$`)
)

// parseModrinthIDs reads a comma list of 1..modrinthBulkMaxIDs base62 ids.
func parseModrinthIDs(raw string) ([]string, bool) {
	var ids []string
	seen := map[string]bool{}
	for _, id := range strings.Split(raw, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !modrinthIDRe.MatchString(id) {
			return nil, false
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 || len(ids) > modrinthBulkMaxIDs {
		return nil, false
	}
	return ids, true
}

func modrinthIDsQuery(ids []string) string {
	raw, _ := json.Marshal(ids)
	return "?ids=" + url.QueryEscape(string(raw))
}

// Versions GET /api/modrinth/versions?ids=a,b,c - several versions in one call.
func (h *ModrinthHandler) Versions(w http.ResponseWriter, r *http.Request) {
	ids, ok := parseModrinthIDs(r.URL.Query().Get("ids"))
	if !ok {
		sendJSONError(w, "ids must be 1 to 100 comma-separated Modrinth ids", http.StatusBadRequest)
		return
	}
	h.proxyJSONWith(r.Context(), modrinthProjectTTL, modrinthBaseURL+"/versions"+modrinthIDsQuery(ids), w, stripVersionChangelogs)
}

// Projects GET /api/modrinth/projects?ids=a,b,c - several projects in one call,
// slimmed to what names a project: the description body is most of a project's
// bytes and nothing reading this renders it.
func (h *ModrinthHandler) Projects(w http.ResponseWriter, r *http.Request) {
	ids, ok := parseModrinthIDs(r.URL.Query().Get("ids"))
	if !ok {
		sendJSONError(w, "ids must be 1 to 100 comma-separated Modrinth ids", http.StatusBadRequest)
		return
	}
	h.proxyJSONWith(r.Context(), modrinthProjectTTL, modrinthBaseURL+"/projects"+modrinthIDsQuery(ids), w, slimProjects)
}

type latestFromHashesRequest struct {
	Hashes       []string `json:"hashes"`
	Algorithm    string   `json:"algorithm"`
	Loaders      []string `json:"loaders"`
	GameVersions []string `json:"game_versions"`
	VersionTypes []string `json:"version_types,omitempty"`
}

func (req *latestFromHashesRequest) validate() string {
	want := map[string]int{"sha512": 128, "sha1": 40}[req.Algorithm]
	if want == 0 {
		return "algorithm must be sha512 or sha1"
	}
	if len(req.Hashes) == 0 || len(req.Hashes) > modrinthBulkMaxHashes {
		return "hashes must hold 1 to 1000 entries"
	}
	for _, h := range req.Hashes {
		if len(h) != want || !hexRe.MatchString(h) {
			return "every hash must be a lowercase hex " + req.Algorithm
		}
	}
	if len(req.Loaders) == 0 || len(req.Loaders) > modrinthBulkMaxFilter {
		return "loaders must hold 1 to 10 entries"
	}
	for _, l := range req.Loaders {
		if !modrinthLoaderRe.MatchString(l) {
			return "invalid loader"
		}
	}
	if len(req.GameVersions) == 0 || len(req.GameVersions) > modrinthBulkMaxFilter {
		return "game_versions must hold 1 to 10 entries"
	}
	for _, v := range req.GameVersions {
		if !modrinthGameVersionRe.MatchString(v) {
			return "invalid game version"
		}
	}
	if len(req.VersionTypes) > 3 {
		return "too many version_types"
	}
	for _, t := range req.VersionTypes {
		if t != "release" && t != "beta" && t != "alpha" {
			return "version_types must be release, beta or alpha"
		}
	}
	return ""
}

// LatestFromHashes POST /api/modrinth/version-files/update - for each installed
// file hash, the newest version of its project that matches the loaders, game
// versions and channels. A hash missing from the answer has no such version.
func (h *ModrinthHandler) LatestFromHashes(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, modrinthBulkBodyLimit)
	var req latestFromHashesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if msg := req.validate(); msg != "" {
		sendJSONError(w, msg, http.StatusBadRequest)
		return
	}
	body, _ := json.Marshal(req)
	h.proxyRequest(r.Context(), modrinthSearchTTL, http.MethodPost, modrinthBaseURL+"/version_files/update", body, w, stripVersionMapChangelogs)
}

// stripVersionMapChangelogs is stripVersionChangelogs for Modrinth's
// hash -> version map. Unparseable input is returned unchanged.
func stripVersionMapChangelogs(body []byte) []byte {
	var m map[string]map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(body), &m); err != nil {
		return body
	}
	for _, v := range m {
		delete(v, "changelog")
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// slimProjects keeps the naming fields of each project in a list, plus
// client_side: the Content tab warns before an update of a mod players need on
// their own client too.
func slimProjects(body []byte) []byte {
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(body), &list); err != nil {
		return body
	}
	out := make([]map[string]json.RawMessage, 0, len(list))
	for _, p := range list {
		slim := map[string]json.RawMessage{}
		for _, k := range []string{"id", "slug", "title", "icon_url", "project_type", "client_side"} {
			if v, ok := p[k]; ok {
				slim[k] = v
			}
		}
		out = append(out, slim)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return body
	}
	return b
}
