package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Cross-version availability for a set of Modrinth-linked content: "if this
// pack (or this server) moved from 1.20.2 to 1.21.11, which mods would still
// have a version, and which would be lost".
//
// The existing update path (ModpackUpdateChecker, /update-mods) only ever asks
// for a newer version of the SAME game version. This file is the other axis.
//
// Why search instead of per-project version lists: a project's game_versions
// and loaders arrays are unions, so a mod with 1.16.5-for-forge and
// 1.21-for-fabric would read as supporting 1.16.5 on fabric. Modrinth's SEARCH
// index is per-version accurate (verified against ground truth: sodium with
// versions:1.16.5 + categories:neoforge returns nothing, and its neoforge
// support does start at 1.21.1), and it answers for up to a chunk of projects
// at once. That turns the cost from one request per mod into one request per
// chunk per target version, which is what makes this check synchronous instead
// of a background job with progress polling.

// Side buckets. A modpack build supplies the side its author already declared;
// a server derives it from Modrinth's client_side/server_side.
const (
	CompatSideClient = "client"
	CompatSideServer = "server"
	CompatSideBoth   = "both"
)

// Per-bucket and per-version status values.
const (
	CompatStatusGreen  = "green"
	CompatStatusOrange = "orange"
	CompatStatusRed    = "red"
	CompatStatusEmpty  = "empty"
)

// Reasons an item is not available on a target version.
const (
	// CompatReasonNoVersion: the project exists and is searchable, but has no
	// version for this game version + loader pair.
	CompatReasonNoVersion = "no-version"
	// CompatReasonUnresolvable: the project is not in the search index at all
	// (archived, unlisted, withdrawn). "We cannot tell" is a different claim
	// from "it is not available", and conflating them would report an archived
	// mod as missing on every single version.
	CompatReasonUnresolvable = "unresolvable"
)

// Target selection modes.
const (
	CompatModeSpecific   = "specific"
	CompatModeAllNewer   = "all-newer"
	CompatModeNewerLines = "newer-lines"
)

// CompatItem is one piece of content to check. Key is the caller's own handle
// (a modversion id for a pack, a server-mod id for a server) and is echoed back
// untouched so the caller can map results onto its own rows.
type CompatItem struct {
	Key                  string `json:"key"`
	ProjectID            string `json:"projectId"`
	Title                string `json:"title"`
	Slug                 string `json:"slug"`
	Side                 string `json:"side"`
	CurrentVersionID     string `json:"currentVersionId"`
	CurrentVersionNumber string `json:"currentVersionNumber"`
}

// CompatBucket is one side's tally on one target version.
type CompatBucket struct {
	Total     int    `json:"total"`
	Available int    `json:"available"`
	Status    string `json:"status"`
}

// CompatMissing is one item that would be lost on a target version.
type CompatMissing struct {
	Key            string `json:"key"`
	ProjectID      string `json:"projectId"`
	Title          string `json:"title"`
	Slug           string `json:"slug"`
	Side           string `json:"side"`
	CurrentVersion string `json:"currentVersion"`
	Reason         string `json:"reason"`
}

// CompatVersion is one target Minecraft version's verdict.
type CompatVersion struct {
	Minecraft string                  `json:"minecraft"`
	Status    string                  `json:"status"`
	Buckets   map[string]CompatBucket `json:"buckets"`
	Missing   []CompatMissing         `json:"missing"`
}

// CompatLine groups target versions by their version line (1.21, 26.1, ...).
type CompatLine struct {
	Line     string          `json:"line"`
	Green    int             `json:"green"`
	Orange   int             `json:"orange"`
	Red      int             `json:"red"`
	Versions []CompatVersion `json:"versions"`
}

// CompatMatrix is what both the pack and the server endpoint return. One shape
// so one panel component can render either.
type CompatMatrix struct {
	Loader   string       `json:"loader"`
	Current  string       `json:"current"`
	Mode     string       `json:"mode"`
	Checked  int          `json:"checked"`
	Unlinked int          `json:"unlinked"`
	Lines    []CompatLine `json:"lines"`
}

// GameVersion is one entry of Modrinth's /tag/game_version list.
type GameVersion struct {
	Version     string `json:"version"`
	VersionType string `json:"version_type"`
	Date        string `json:"date"`
	Major       bool   `json:"major"`
}

// VersionLine is the grouping key for a Minecraft version: the first two
// dot-separated components.
//
// This holds across BOTH schemes Minecraft now uses: the classic "1.21.11" ->
// "1.21" and the calendar "26.1.2" -> "26.1". A version with only two
// components ("1.21", "26.2") is its own line head.
//
// Modrinth's own `major` flag is deliberately NOT used here: it marks 1.20.6,
// 1.20.4, 1.20.2 and 1.20.1 all as major, and marks nothing in 26.x, so it
// describes notability rather than a line.
func VersionLine(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) <= 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}

// SelectCompatTargets picks the game versions to check.
//
// releases must be Modrinth's release list in its own order (newest first);
// that ordering is the authority, so no version comparator is written here. A
// hand-rolled one would have to rank 1.21.11 above 1.21.2 and 26.2 above
// 1.21.11, and getting that wrong is silent.
//
// current is excluded from every result: it is where we already are.
func SelectCompatTargets(releases []string, current, mode, specific string) []string {
	switch mode {
	case CompatModeSpecific:
		if specific == "" || specific == current {
			return nil
		}
		for _, r := range releases {
			if r == specific {
				return []string{specific}
			}
		}
		return nil
	case CompatModeAllNewer, CompatModeNewerLines:
	default:
		return nil
	}

	idx := -1
	for i, r := range releases {
		if r == current {
			idx = i
			break
		}
	}
	if idx < 0 {
		// The current version is not a known release (a snapshot, or a server
		// whose version was never declared). Nothing can be called "newer" than
		// an unplaceable version, so offer nothing rather than guess.
		return nil
	}
	currentLine := VersionLine(current)
	out := []string{}
	for i := idx - 1; i >= 0; i-- { // newest-first list, so newer = lower index
		v := releases[i]
		if mode == CompatModeNewerLines && VersionLine(v) == currentLine {
			continue
		}
		out = append(out, v)
	}
	return out
}

// BucketStatus applies the severity rule for one side bucket.
//
// A miss in the "both" bucket is red: content needed on the server AND the
// client takes the whole thing down. A miss on a single-sided bucket is orange:
// one side degrades, the other still runs. An empty bucket is neither and must
// not colour the row.
func BucketStatus(side string, total, available int) string {
	if total == 0 {
		return CompatStatusEmpty
	}
	if available >= total {
		return CompatStatusGreen
	}
	if side == CompatSideBoth {
		return CompatStatusRed
	}
	return CompatStatusOrange
}

// worstStatus reduces a version row's buckets to one status. empty never wins.
func worstStatus(statuses []string) string {
	rank := map[string]int{CompatStatusEmpty: 0, CompatStatusGreen: 1, CompatStatusOrange: 2, CompatStatusRed: 3}
	worst := CompatStatusEmpty
	for _, s := range statuses {
		if rank[s] > rank[worst] {
			worst = s
		}
	}
	if worst == CompatStatusEmpty {
		// Nothing to check at all (no linked content): report green rather than
		// a fourth state, because there is nothing that would be lost.
		return CompatStatusGreen
	}
	return worst
}

// --- Modrinth access -------------------------------------------------------

// compatLimiter paces outgoing search calls process-wide. Modrinth allows 300
// requests per minute per IP and Core is the IP for every tenant at once, so
// the burst is what needs bounding, not the individual caller.
var compatLimiter = newTokenBucket(20, 5)

type tokenBucket struct {
	mu     sync.Mutex
	tokens float64
	burst  float64
	rate   float64 // tokens per second
	last   time.Time
}

func newTokenBucket(burst, ratePerSecond float64) *tokenBucket {
	return &tokenBucket{tokens: burst, burst: burst, rate: ratePerSecond, last: time.Now()}
}

// wait blocks until a token is available or ctx is done.
func (b *tokenBucket) wait(ctx context.Context) error {
	for {
		b.mu.Lock()
		now := time.Now()
		b.tokens += now.Sub(b.last).Seconds() * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
		if b.tokens >= 1 {
			b.tokens--
			b.mu.Unlock()
			return nil
		}
		deficit := (1 - b.tokens) / b.rate
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(deficit * float64(time.Second))):
		}
	}
}

var compatHTTP = &http.Client{Timeout: 20 * time.Second}

func compatGet(ctx context.Context, rawURL string, out interface{}) error {
	if err := compatLimiter.wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", DylarisUserAgent())
	res, err := compatHTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("modrinth: %d %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// compatSearchChunkSize bounds project ids per search call. 60 keeps the
// request URL at roughly 2.1 KB, well inside what any proxy in front of
// Modrinth will accept, while still collapsing a large pack into a handful of
// calls.
const compatSearchChunkSize = 60

// searchProjectIDs returns which of the given project ids have at least one
// version matching mc + loader. An empty mc or loader drops that facet, which
// is how the baseline ("is this project searchable at all") query is made.
func searchProjectIDs(ctx context.Context, ids []string, mc, loader string) (map[string]bool, error) {
	facets := [][]string{}
	group := make([]string, 0, len(ids))
	for _, id := range ids {
		group = append(group, "project_id:"+id)
	}
	facets = append(facets, group)
	if mc != "" {
		facets = append(facets, []string{"versions:" + mc})
	}
	if loader != "" {
		facets = append(facets, []string{"categories:" + loader})
	}
	raw, err := json.Marshal(facets)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("limit", "100")
	q.Set("facets", string(raw))

	var res struct {
		Hits []struct {
			ProjectID string `json:"project_id"`
		} `json:"hits"`
	}
	if err := compatGet(ctx, modrinthAPI+"/search?"+q.Encode(), &res); err != nil {
		return nil, err
	}
	found := make(map[string]bool, len(res.Hits))
	for _, h := range res.Hits {
		found[h.ProjectID] = true
	}
	return found, nil
}

// FetchGameVersions returns Modrinth's game version tag list, newest first.
func FetchGameVersions(ctx context.Context) ([]GameVersion, error) {
	var out []GameVersion
	if err := compatGet(ctx, modrinthAPI+"/tag/game_version", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ReleaseVersions filters a tag list down to releases, preserving order.
// Snapshots are never offered as a migration target.
func ReleaseVersions(all []GameVersion) []string {
	out := make([]string, 0, len(all))
	for _, v := range all {
		if v.VersionType == "release" {
			out = append(out, v.Version)
		}
	}
	return out
}

// FetchProjects returns Modrinth project metadata for the given ids, keyed by
// id. Used for titles, slugs and the client_side/server_side declaration a
// server needs to bucket its mods.
func FetchProjects(ctx context.Context, ids []string) (map[string]ModrinthProjectMeta, error) {
	out := map[string]ModrinthProjectMeta{}
	for start := 0; start < len(ids); start += compatSearchChunkSize {
		end := start + compatSearchChunkSize
		if end > len(ids) {
			end = len(ids)
		}
		raw, err := json.Marshal(ids[start:end])
		if err != nil {
			return nil, err
		}
		q := url.Values{}
		q.Set("ids", string(raw))
		var batch []ModrinthProjectMeta
		if err := compatGet(ctx, modrinthAPI+"/projects?"+q.Encode(), &batch); err != nil {
			return nil, err
		}
		for _, p := range batch {
			out[p.ID] = p
		}
	}
	return out, nil
}

// ModrinthProjectMeta is the subset of a Modrinth project object this feature
// needs.
type ModrinthProjectMeta struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	Title      string `json:"title"`
	ClientSide string `json:"client_side"`
	ServerSide string `json:"server_side"`
}

// SideFromModrinth maps Modrinth's per-project support declaration onto a side
// bucket. "unsupported" on one side makes the mod single-sided; anything else
// counts as needed on both, which is the safe reading: treating a mod as
// single-sided when it is not would downgrade a breaking miss from red to
// orange.
//
// Not to be confused with sideFromEnv (mrpack_manifest.go), which inverts an
// mrpack manifest env block and deliberately insists on the exact
// required/unsupported pair envForSide writes. This one reads a live project
// declaration, where "optional" is common and still means the side is served.
func SideFromModrinth(clientSide, serverSide string) string {
	client := clientSide != "unsupported"
	server := serverSide != "unsupported"
	switch {
	case client && !server:
		return CompatSideClient
	case server && !client:
		return CompatSideServer
	default:
		return CompatSideBoth
	}
}

// --- Availability cache ----------------------------------------------------

// AvailabilityCache is the small slice of Redis this service needs. It is an
// interface so the matrix builder can be tested without a Redis.
type AvailabilityCache interface {
	GetMany(ctx context.Context, keys []string) []string
	SetMany(ctx context.Context, values map[string]string, ttl time.Duration)
}

const compatCacheTTL = 6 * time.Hour

// availabilityKey caches per (project, version, loader) rather than per
// request, so two packs that share a popular mod share the answer.
func availabilityKey(projectID, mc, loader string) string {
	return "dylaris:modcompat:" + projectID + ":" + mc + ":" + loader
}

// --- Matrix ----------------------------------------------------------------

// CompatRequest is one call to BuildMatrix.
type CompatRequest struct {
	Items   []CompatItem
	Loader  string
	Current string
	Mode    string
	Targets []string
	Cache   AvailabilityCache
}

// BuildMatrix answers availability for every item across every target version.
//
// Items with no ProjectID (manual uploads) are counted as Unlinked and take no
// part in the verdict: nothing can be known about a file Modrinth has never
// seen, and folding them in as failures would make every row red for a reason
// the operator cannot act on from here.
func BuildMatrix(ctx context.Context, req CompatRequest) (*CompatMatrix, error) {
	m := &CompatMatrix{
		Loader:  req.Loader,
		Current: req.Current,
		Mode:    req.Mode,
		Lines:   []CompatLine{},
	}

	linked := make([]CompatItem, 0, len(req.Items))
	for _, it := range req.Items {
		if strings.TrimSpace(it.ProjectID) == "" {
			m.Unlinked++
			continue
		}
		linked = append(linked, it)
	}
	m.Checked = len(linked)
	if len(linked) == 0 || len(req.Targets) == 0 {
		return m, nil
	}

	ids := uniqueProjectIDs(linked)

	// One baseline pass tells us which projects the search index can answer for
	// at all. A project missing here is archived / unlisted / withdrawn, and
	// reporting that as "no version for 1.21.11" would be a claim we did not
	// check.
	searchable := map[string]bool{}
	for _, chunk := range chunkStrings(ids, compatSearchChunkSize) {
		found, err := searchProjectIDs(ctx, chunk, "", "")
		if err != nil {
			return nil, fmt.Errorf("modrinth baseline search: %w", err)
		}
		for id := range found {
			searchable[id] = true
		}
	}

	byLine := map[string]*CompatLine{}
	lineOrder := []string{}

	for _, mc := range req.Targets {
		avail, err := availabilityFor(ctx, req.Cache, ids, mc, req.Loader)
		if err != nil {
			return nil, err
		}
		row := buildVersionRow(mc, linked, avail, searchable)

		line := VersionLine(mc)
		l := byLine[line]
		if l == nil {
			l = &CompatLine{Line: line, Versions: []CompatVersion{}}
			byLine[line] = l
			lineOrder = append(lineOrder, line)
		}
		switch row.Status {
		case CompatStatusGreen:
			l.Green++
		case CompatStatusOrange:
			l.Orange++
		case CompatStatusRed:
			l.Red++
		}
		l.Versions = append(l.Versions, row)
	}

	for _, name := range lineOrder {
		m.Lines = append(m.Lines, *byLine[name])
	}
	return m, nil
}

// buildVersionRow tallies one target version. Pure: no I/O, so the status rules
// are testable on their own.
func buildVersionRow(mc string, items []CompatItem, avail, searchable map[string]bool) CompatVersion {
	row := CompatVersion{
		Minecraft: mc,
		Buckets:   map[string]CompatBucket{},
		Missing:   []CompatMissing{},
	}
	totals := map[string]int{}
	haves := map[string]int{}
	for _, it := range items {
		side := it.Side
		if side != CompatSideClient && side != CompatSideServer {
			side = CompatSideBoth
		}
		totals[side]++
		if avail[it.ProjectID] {
			haves[side]++
			continue
		}
		reason := CompatReasonNoVersion
		if !searchable[it.ProjectID] {
			reason = CompatReasonUnresolvable
		}
		row.Missing = append(row.Missing, CompatMissing{
			Key:            it.Key,
			ProjectID:      it.ProjectID,
			Title:          it.Title,
			Slug:           it.Slug,
			Side:           side,
			CurrentVersion: it.CurrentVersionNumber,
			Reason:         reason,
		})
	}
	statuses := []string{}
	for _, side := range []string{CompatSideBoth, CompatSideServer, CompatSideClient} {
		st := BucketStatus(side, totals[side], haves[side])
		row.Buckets[side] = CompatBucket{Total: totals[side], Available: haves[side], Status: st}
		statuses = append(statuses, st)
	}
	row.Status = worstStatus(statuses)
	return row
}

// availabilityFor resolves project availability for one target version, reading
// what the cache already knows and searching only for the rest.
func availabilityFor(ctx context.Context, cache AvailabilityCache, ids []string, mc, loader string) (map[string]bool, error) {
	avail := make(map[string]bool, len(ids))
	unknown := ids

	if cache != nil {
		keys := make([]string, len(ids))
		for i, id := range ids {
			keys[i] = availabilityKey(id, mc, loader)
		}
		avail, unknown = splitCachedAvailability(ids, cache.GetMany(ctx, keys))
	}
	if len(unknown) == 0 {
		return avail, nil
	}

	fresh := map[string]string{}
	for _, chunk := range chunkStrings(unknown, compatSearchChunkSize) {
		found, err := searchProjectIDs(ctx, chunk, mc, loader)
		if err != nil {
			return nil, fmt.Errorf("modrinth search (%s/%s): %w", mc, loader, err)
		}
		for _, id := range chunk {
			ok := found[id]
			avail[id] = ok
			if ok {
				fresh[availabilityKey(id, mc, loader)] = "1"
			} else {
				fresh[availabilityKey(id, mc, loader)] = "0"
			}
		}
	}
	if cache != nil && len(fresh) > 0 {
		cache.SetMany(ctx, fresh, compatCacheTTL)
	}
	return avail, nil
}

// splitCachedAvailability partitions ids into what the cache already answered
// and what still has to be searched. cached is positional: cached[i] belongs to
// ids[i], and anything that is neither "1" nor "0" (including a short slice)
// counts as unknown.
//
// Pure, and it MUST NOT touch ids. It used to filter in place with `ids[:0]`,
// which is the correct idiom when the source is finished with - and ids is not:
// BuildMatrix reuses the same slice for every target version. Reusing the
// backing array compacted the uncached ids over the front of the caller's list
// and left duplicates in the tail, so from the SECOND target version onwards
// some projects were never queried at all. A project missing from the map reads
// as false, which renders as "no version for this Minecraft version" - a
// confident wrong answer about a mod that is perfectly available.
//
// It only bit when the cache was PARTIALLY warm, which is the ordinary state as
// soon as two packs share a mod or one mod is added to a checked build.
func splitCachedAvailability(ids, cached []string) (map[string]bool, []string) {
	avail := make(map[string]bool, len(ids))
	unknown := make([]string, 0, len(ids))
	for i, id := range ids {
		if i < len(cached) {
			switch cached[i] {
			case "1":
				avail[id] = true
				continue
			case "0":
				avail[id] = false
				continue
			}
		}
		unknown = append(unknown, id)
	}
	return avail, unknown
}

func uniqueProjectIDs(items []CompatItem) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, it := range items {
		if seen[it.ProjectID] {
			continue
		}
		seen[it.ProjectID] = true
		out = append(out, it.ProjectID)
	}
	sort.Strings(out) // deterministic chunking -> deterministic cache keys per chunk
	return out
}

func chunkStrings(in []string, size int) [][]string {
	if size < 1 {
		size = 1
	}
	out := [][]string{}
	for start := 0; start < len(in); start += size {
		end := start + size
		if end > len(in) {
			end = len(in)
		}
		out = append(out, in[start:end])
	}
	return out
}
