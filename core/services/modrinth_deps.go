package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The panel already has a Modrinth *proxy* in handlers/modrinth.go, but that one
// writes straight to an http.ResponseWriter (cache passthrough). Content
// ingestion needs parsed version objects to resolve dependencies and auto-link
// uploads, so this file carries its own small parse-and-return client.

// ModrinthFile is one downloadable artifact of a Modrinth version.
type ModrinthFile struct {
	Filename string            `json:"filename"`
	URL      string            `json:"url"`
	Primary  bool              `json:"primary"`
	Size     int64             `json:"size"`
	Hashes   map[string]string `json:"hashes"`
}

// modrinthVersion is the subset of a Modrinth /version object we need.
type modrinthVersion struct {
	ID           string         `json:"id"`
	ProjectID    string         `json:"project_id"`
	Name         string         `json:"name"`
	VersionNum   string         `json:"version_number"`
	VersionType  string         `json:"version_type"`
	GameVersions []string       `json:"game_versions"`
	Loaders      []string       `json:"loaders"`
	DatePub      string         `json:"date_published"`
	Files        []ModrinthFile `json:"files"`
	Dependencies []struct {
		VersionID      string `json:"version_id"`
		ProjectID      string `json:"project_id"`
		DependencyType string `json:"dependency_type"`
	} `json:"dependencies"`
}

// ModrinthVersion is the exported alias handlers use.
type ModrinthVersion = modrinthVersion

const modrinthAPI = "https://api.modrinth.com/v2"

var modrinthHTTP = &http.Client{Timeout: 15 * time.Second}

// ModrinthHTTPError is a non-200 answer from Modrinth, with the status kept.
//
// It used to be a formatted string, which erased the one distinction that
// matters to anything showing the result to a person: 404 means "Modrinth does
// not know this", and everything else means "Modrinth did not answer". Both
// arrived as a nil and were reported as the former.
type ModrinthHTTPError struct {
	Path   string
	Status int
	Body   string
}

func (e *ModrinthHTTPError) Error() string {
	return fmt.Sprintf("modrinth %s: %d %s", e.Path, e.Status, e.Body)
}

// ModrinthNotFound reports whether err is Modrinth saying it does not know the
// thing that was asked for, as opposed to not answering at all.
func ModrinthNotFound(err error) bool {
	var httpErr *ModrinthHTTPError
	return errors.As(err, &httpErr) && httpErr.Status == http.StatusNotFound
}

func modrinthGet(path string, out interface{}) error {
	return modrinthGetCtx(context.Background(), path, out)
}

func modrinthGetCtx(ctx context.Context, path string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, "GET", modrinthAPI+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", DylarisUserAgent())
	res, err := modrinthHTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		return &ModrinthHTTPError{Path: path, Status: res.StatusCode, Body: string(b)}
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// FetchModrinthVersion returns a single version by id.
func FetchModrinthVersion(versionID string) (*modrinthVersion, error) {
	var v modrinthVersion
	if err := modrinthGet("/version/"+url.PathEscape(versionID), &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// FetchModrinthVersionPaced is FetchModrinthVersion behind the shared
// Modrinth pacing, for calls a user can repeat at will.
func FetchModrinthVersionPaced(ctx context.Context, versionID string) (*modrinthVersion, error) {
	var v modrinthVersion
	if err := modrinthGetPaced(ctx, "/version/"+url.PathEscape(versionID), &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// ModrinthByHash returns the version matching an sha1 hash, or nil.
func ModrinthByHash(sha1hex string) *modrinthVersion {
	v, _ := ModrinthByHashErr(sha1hex)
	return v
}

// ModrinthByHashErr is ModrinthByHash with the reason kept.
//
// The nil-on-anything version above is right for the two import paths, where a
// miss just leaves a file unlinked. It is wrong wherever the answer is shown to
// someone: "no Modrinth version has this hash" and "Modrinth did not answer"
// look identical from a nil, so an outage told every operator their jars were
// not on Modrinth - advice they might act on by deleting them.
func ModrinthByHashErr(sha1hex string) (*modrinthVersion, error) {
	var v modrinthVersion
	if err := modrinthGet("/version_file/"+url.PathEscape(sha1hex)+"?algorithm=sha1", &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// ModrinthByHashes looks many sha1 hashes up in one request. A hash missing
// from the map is not on Modrinth; an error means Modrinth answered for none of
// them. One request instead of one per jar: identifying a modpack's few
// hundred jars spent Core's whole shared Modrinth budget at once.
func ModrinthByHashes(sha1s []string) (map[string]ModrinthVersion, error) {
	out := map[string]ModrinthVersion{}
	// ponytail: 500 per request is our own bound, Modrinth documents none;
	// lower it if they start refusing big bodies.
	for start := 0; start < len(sha1s); start += modrinthHashBatch {
		end := min(start+modrinthHashBatch, len(sha1s))
		if err := modrinthByHashesBatch(sha1s[start:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

const modrinthHashBatch = 500

func modrinthByHashesBatch(sha1s []string, out map[string]ModrinthVersion) error {
	buf, err := json.Marshal(map[string]interface{}{"hashes": sha1s, "algorithm": "sha1"})
	if err != nil {
		return err
	}
	req, _ := http.NewRequest("POST", modrinthAPI+"/version_files", bytes.NewReader(buf))
	req.Header.Set("User-Agent", DylarisUserAgent())
	req.Header.Set("Content-Type", "application/json")
	res, err := modrinthHTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		return &ModrinthHTTPError{Path: "/version_files", Status: res.StatusCode, Body: string(b)}
	}
	return json.NewDecoder(res.Body).Decode(&out)
}

// CheckLatestVersions batch-queries Modrinth for the latest version matching the
// given loaders + game versions for each file hash. loaders and game_versions are
// REQUIRED by the endpoint. The result map is keyed by the ORIGINAL input hash.
// Unauthenticated (public read).
func CheckLatestVersions(hashes []string, algorithm string, loaders, gameVersions []string) (map[string]ModrinthVersion, error) {
	payload := map[string]interface{}{
		"hashes":        hashes,
		"algorithm":     algorithm,
		"loaders":       loaders,
		"game_versions": gameVersions,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequest("POST", modrinthAPI+"/version_files/update", bytes.NewReader(buf))
	req.Header.Set("User-Agent", DylarisUserAgent())
	req.Header.Set("Content-Type", "application/json")
	res, err := modrinthHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("modrinth version_files/update: %d %s", res.StatusCode, string(b))
	}
	out := map[string]ModrinthVersion{}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// ProjectSlugOrID returns a slug source (Modrinth version carries project_id;
// use it as the catalog slug basis when no better name exists).
func (v *modrinthVersion) ProjectSlugOrID() string {
	if v.ProjectID != "" {
		return v.ProjectID
	}
	return v.Name
}

// PrimaryFile returns the primary file (or the first) of the version. When the
// version has no files it returns a zero ModrinthFile with an empty Hashes map
// so callers can index Hashes without a nil check.
func (v *modrinthVersion) PrimaryFile() ModrinthFile {
	for _, f := range v.Files {
		if f.Primary {
			return f
		}
	}
	if len(v.Files) > 0 {
		return v.Files[0]
	}
	return ModrinthFile{Hashes: map[string]string{}}
}

// LatestProjectVersionFor picks the version of a project that fits mc+loader:
// a release over a beta over an alpha, the newest within that channel. Exported
// for the migration paths, which need a fallback for content whose stored hash
// Modrinth cannot place. nil, nil means nothing fits.
//
// Without a loader and Minecraft version it refuses. It used to drop the filter
// instead, which handed back whatever was newest for any game version.
func LatestProjectVersionFor(projectID, mc, loader string) (*modrinthVersion, error) {
	return latestFitting(context.Background(), projectID, mc, loader)
}

var errFitUnknown = errors.New("modrinth: a loader and a Minecraft version are needed to pick a version")

func latestFitting(ctx context.Context, projectID, mc, loader string) (*modrinthVersion, error) {
	loader = strings.ToLower(strings.TrimSpace(loader))
	mc = strings.TrimSpace(mc)
	if !ModFitKnown(loader, mc) {
		return nil, errFitUnknown
	}
	// A plugin fits any patch of its line, which an exact game_versions filter
	// would hide, so plugins are filtered by pickPreferred alone.
	var games []string
	if !isProxyLoader(loader) && !isPluginLoader(loader) {
		games = []string{mc}
	}
	list, err := fetchProjectVersions(ctx, projectID, modLoadersFor[loader], games)
	if err != nil {
		return nil, err
	}
	return pickPreferred(list, loader, mc), nil
}

func channelRank(versionType string) int {
	switch versionType {
	case "release":
		return 0
	case "beta":
		return 1
	case "alpha":
		return 2
	}
	return 3
}

// pickPreferred re-checks the fit rather than trusting the query filter, and
// prefers the channel before the date: a beta published after the last release
// is not what "the build for this server" means. The list order is not
// contractual, so newest is by date_published.
func pickPreferred(list []modrinthVersion, loader, mc string) *modrinthVersion {
	var best *modrinthVersion
	for i := range list {
		v := &list[i]
		if ok, _ := ModVersionFits(loader, mc, v.Loaders, v.GameVersions); !ok {
			continue
		}
		if best == nil {
			best = v
			continue
		}
		vr, br := channelRank(v.VersionType), channelRank(best.VersionType)
		if vr < br || vr == br && v.DatePub > best.DatePub {
			best = v
		}
	}
	return best
}

// The Modrinth calls the resolver makes. Variables so tests can stand in for
// Modrinth.
var (
	fetchVersionsByID    = modrinthVersionsByID
	fetchProjectVersions = modrinthProjectVersions
	fetchPlanProjects    = FetchProjects
)

// modrinthVersionsByID is the bulk /versions lookup: one call per chunk, not
// one per id. An id Modrinth does not know is missing from the map.
func modrinthVersionsByID(ctx context.Context, ids []string) (map[string]*modrinthVersion, error) {
	out := map[string]*modrinthVersion{}
	for start := 0; start < len(ids); start += compatSearchChunkSize {
		end := min(start+compatSearchChunkSize, len(ids))
		raw, err := json.Marshal(ids[start:end])
		if err != nil {
			return nil, err
		}
		var batch []modrinthVersion
		if err := modrinthGetPaced(ctx, "/versions?ids="+url.QueryEscape(string(raw)), &batch); err != nil {
			return nil, err
		}
		for i := range batch {
			out[batch[i].ID] = &batch[i]
		}
	}
	return out, nil
}

func modrinthProjectVersions(ctx context.Context, projectID string, loaders, games []string) ([]modrinthVersion, error) {
	q := url.Values{}
	l, err := json.Marshal(loaders)
	if err != nil {
		return nil, err
	}
	q.Set("loaders", string(l))
	if len(games) > 0 {
		g, err := json.Marshal(games)
		if err != nil {
			return nil, err
		}
		q.Set("game_versions", string(g))
	}
	var list []modrinthVersion
	if err := modrinthGetPaced(ctx, "/project/"+url.PathEscape(projectID)+"/version?"+q.Encode(), &list); err != nil {
		return nil, err
	}
	return list, nil
}

// modrinthGetPaced shares compatLimiter: Modrinth limits per IP and Core is
// that IP for every tenant, so a dependency walk must queue behind the same
// budget as the compat checks rather than burst past it.
func modrinthGetPaced(ctx context.Context, path string, out interface{}) error {
	if err := compatLimiter.wait(ctx); err != nil {
		return err
	}
	return modrinthGetCtx(ctx, path, out)
}

// DepProblem is a dependency the walk could not satisfy, or an installed
// project a planned version says it is incompatible with.
type DepProblem struct {
	ProjectID string `json:"projectId"`
	Title     string `json:"title"`
	Reason    string `json:"reason"`
	// by is the project whose version declared the incompatibility.
	by string
}

type depResolution struct {
	versions     []*modrinthVersion // dependency-first, the root last
	unresolved   []DepProblem
	incompatible []DepProblem
}

// maxDepNodes bounds one walk, and with it the Modrinth calls it can make: at
// most one project lookup per node plus one bulk lookup per level. Real mods
// need a handful; fifty is a broken or hostile graph.
const maxDepNodes = 50

// resolveDeps walks the required dependencies of root breadth-first. A
// dependency in have (already satisfied) or already seen (cycles, diamonds) is
// not resolved again. present is what is on the server at all, fitting or not:
// an incompatible project is a conflict whichever build of it is installed. A pinned version is used only when it fits the server;
// otherwise, and for an unpinned dependency, the preferred fitting version of
// the project is taken. A version_id-only dependency learns its project from
// that version.
func resolveDeps(ctx context.Context, root *modrinthVersion, loader, mc string, have, present map[string]bool) (*depResolution, error) {
	loader = strings.ToLower(strings.TrimSpace(loader))
	mc = strings.TrimSpace(mc)
	if !ModFitKnown(loader, mc) {
		return nil, errFitUnknown
	}
	res := &depResolution{}
	picked := map[string]*modrinthVersion{root.ProjectID: root}
	children := map[string][]string{}
	seen := map[string]bool{root.ProjectID: true}
	reportedIncompat := map[string]bool{}
	nodes := 0
	frontier := []*modrinthVersion{root}
	for len(frontier) > 0 {
		var ids []string
		asked := map[string]bool{}
		for _, v := range frontier {
			for _, d := range v.Dependencies {
				if d.DependencyType != "required" || d.VersionID == "" || asked[d.VersionID] {
					continue
				}
				if d.ProjectID != "" && (seen[d.ProjectID] || have[d.ProjectID]) {
					continue
				}
				asked[d.VersionID] = true
				ids = append(ids, d.VersionID)
			}
		}
		// A failed lookup costs the dependencies it was for, not the walk: an
		// unpinned or project-known one still gets its project lookup below, and
		// the rest land in unresolved. Only a cancelled request stops the walk.
		pinned := map[string]*modrinthVersion{}
		var pinErr error
		if len(ids) > 0 {
			got, err := fetchVersionsByID(ctx, ids)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err != nil {
				pinErr = err
			} else {
				pinned = got
			}
		}
		var next []*modrinthVersion
		for _, v := range frontier {
			for _, d := range v.Dependencies {
				if d.DependencyType == "incompatible" {
					if present[d.ProjectID] && !reportedIncompat[d.ProjectID] {
						reportedIncompat[d.ProjectID] = true
						res.incompatible = append(res.incompatible, DepProblem{ProjectID: d.ProjectID, by: v.ProjectID})
					}
					continue
				}
				if d.DependencyType != "required" {
					continue
				}
				pv := pinned[d.VersionID]
				pid := d.ProjectID
				if pid == "" && pv != nil {
					pid = pv.ProjectID
				}
				if pid == "" {
					reason := "Modrinth has no version " + d.VersionID + "."
					if pinErr != nil {
						reason = modrinthNoAnswer
					}
					res.unresolved = append(res.unresolved, DepProblem{Reason: reason})
					continue
				}
				children[v.ProjectID] = append(children[v.ProjectID], pid)
				if seen[pid] || have[pid] {
					continue
				}
				seen[pid] = true
				if nodes >= maxDepNodes {
					res.unresolved = append(res.unresolved, DepProblem{ProjectID: pid, Reason: "Too many dependencies; stopped after " + strconv.Itoa(maxDepNodes) + "."})
					continue
				}
				nodes++
				pick := pv
				if pick != nil {
					if ok, _ := ModVersionFits(loader, mc, pick.Loaders, pick.GameVersions); !ok {
						pick = nil
					}
				}
				if pick == nil {
					p, err := latestFitting(ctx, pid, mc, loader)
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					if err != nil && !ModrinthNotFound(err) {
						res.unresolved = append(res.unresolved, DepProblem{ProjectID: pid, Reason: modrinthNoAnswer})
						continue
					}
					pick = p
				}
				if pick == nil {
					res.unresolved = append(res.unresolved, DepProblem{ProjectID: pid, Reason: noFitReason(loader, mc)})
					continue
				}
				picked[pid] = pick
				next = append(next, pick)
			}
		}
		frontier = next
	}
	// Post-order over what was picked puts every dependency before the first
	// project that needs it; done stops a cycle.
	done := map[string]bool{}
	var visit func(pid string)
	visit = func(pid string) {
		if done[pid] {
			return
		}
		done[pid] = true
		for _, c := range children[pid] {
			if picked[c] != nil {
				visit(c)
			}
		}
		res.versions = append(res.versions, picked[pid])
	}
	visit(root.ProjectID)
	return res, nil
}

const modrinthNoAnswer = "Modrinth did not answer for this dependency. Try again in a moment."

func noFitReason(loader, mc string) string {
	if isProxyLoader(loader) {
		return "No " + loader + " build on Modrinth."
	}
	return "No " + loader + " build for Minecraft " + mc + " on Modrinth."
}

// ResolvedDep is a required dependency the caller should add to the build.
type ResolvedDep struct {
	Version *modrinthVersion
}

// ResolveModrinthDependencies returns the versions the required dependencies
// of root resolve to, dependency-first, without root itself. alreadyHave holds
// project ids that are already there. Dependencies that could not be resolved
// are left out of the list and returned as unresolved, for the caller to report.
func ResolveModrinthDependencies(root *modrinthVersion, mc, loader string, alreadyHave map[string]bool) ([]ResolvedDep, []DepProblem, error) {
	res, err := resolveDeps(context.Background(), root, loader, mc, alreadyHave, alreadyHave)
	if err != nil {
		return nil, nil, err
	}
	out := []ResolvedDep{}
	for _, v := range res.versions {
		if v != root {
			out = append(out, ResolvedDep{Version: v})
		}
	}
	return out, res.unresolved, nil
}

// PlanEntry is one version the plan would install.
type PlanEntry struct {
	ProjectID     string `json:"projectId"`
	Slug          string `json:"slug"`
	Title         string `json:"title"`
	VersionID     string `json:"versionId"`
	VersionNumber string `json:"versionNumber"`
	FileName      string `json:"fileName"`
	DownloadURL   string `json:"downloadUrl"`
}

// ModInstallPlan is what installing one version takes. Install is
// dependency-first and ends with the requested version.
type ModInstallPlan struct {
	Install      []PlanEntry  `json:"install"`
	Unresolved   []DepProblem `json:"unresolved"`
	Incompatible []DepProblem `json:"incompatible"`
}

// ModFitError is the requested version itself not fitting the server.
type ModFitError struct{ Reason string }

func (e *ModFitError) Error() string { return e.Reason }

// PlanModInstall works out what installing versionID onto a server with this
// loader and Minecraft version takes. installed maps the projects already on
// the server to the version installed. It reads Modrinth only.
//
// An installed build that does not fit the server (a Forge jar on NeoForge, a
// build for another Minecraft version) does not satisfy a dependency: the
// plan installs a fitting one, and the install replaces the jar. A build
// Modrinth does not know is taken as fitting, so nothing is reinstalled on a
// guess.
func PlanModInstall(ctx context.Context, versionID, loader, mc string, installed map[string]string) (*ModInstallPlan, error) {
	lookup := []string{versionID}
	for _, vid := range installed {
		if vid != "" && vid != versionID {
			lookup = append(lookup, vid)
		}
	}
	got, err := fetchVersionsByID(ctx, lookup)
	if err != nil {
		return nil, err
	}
	present, satisfied := map[string]bool{}, map[string]bool{}
	for pid, vid := range installed {
		present[pid] = true
		if b := got[vid]; b != nil {
			if ok, _ := ModVersionFits(loader, mc, b.Loaders, b.GameVersions); !ok {
				continue
			}
		}
		satisfied[pid] = true
	}
	root := got[versionID]
	if root == nil {
		return nil, &ModrinthHTTPError{Path: "/versions", Status: http.StatusNotFound}
	}
	if ok, reason := ModVersionFits(loader, mc, root.Loaders, root.GameVersions); !ok {
		return nil, &ModFitError{Reason: reason}
	}
	res, err := resolveDeps(ctx, root, loader, mc, satisfied, present)
	if err != nil {
		return nil, err
	}

	var ids []string
	listed := map[string]bool{}
	add := func(id string) {
		if id != "" && !listed[id] {
			listed[id] = true
			ids = append(ids, id)
		}
	}
	for _, v := range res.versions {
		add(v.ProjectID)
	}
	for _, p := range res.unresolved {
		add(p.ProjectID)
	}
	for _, p := range res.incompatible {
		add(p.ProjectID)
		add(p.by)
	}
	// Titles are for the confirmation dialog; without them the plan still
	// holds, so a failed lookup falls back to names the versions carry.
	meta, err := fetchPlanProjects(ctx, ids)
	if err != nil {
		meta = nil
	}
	title := func(id, fallback string) string {
		if m, ok := meta[id]; ok && m.Title != "" {
			return m.Title
		}
		if fallback != "" {
			return fallback
		}
		return id
	}

	plan := &ModInstallPlan{Install: []PlanEntry{}, Unresolved: []DepProblem{}, Incompatible: []DepProblem{}}
	for _, v := range res.versions {
		f := v.PrimaryFile()
		plan.Install = append(plan.Install, PlanEntry{
			ProjectID: v.ProjectID, Slug: meta[v.ProjectID].Slug, Title: title(v.ProjectID, v.Name),
			VersionID: v.ID, VersionNumber: v.VersionNum, FileName: f.Filename, DownloadURL: f.URL,
		})
	}
	for _, p := range res.unresolved {
		p.Title = title(p.ProjectID, "")
		plan.Unresolved = append(plan.Unresolved, p)
	}
	for _, p := range res.incompatible {
		p.Title = title(p.ProjectID, "")
		p.Reason = title(p.by, "") + " is marked incompatible with it."
		plan.Incompatible = append(plan.Incompatible, p)
	}
	return plan, nil
}
