package health

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/updates"

	"dylaris-pkg/release"
)

// notesTTL bounds how often Core re-fetches the remote release notes, and so is
// the worst-case delay before a freshly published release reaches the panel.
// The panel refreshes every six hours and on demand; this cache is what stops a
// room full of open panels from polling GitHub.
const notesTTL = 15 * time.Minute

// notesMaxBytes caps a fetched file. Oversize fails open to the embedded copy.
const notesMaxBytes = 1 << 20

// releasesShown caps how many release blocks travel to the panel. Nobody reads
// the fortieth one, and the panel links to the full file.
const releasesShown = 20

// feedPlatform is for people who RUN the platform; feedHosted is for customers,
// who run a node or nothing. An admin gets the first, everyone else the second.
const (
	feedPlatform = "platform"
	feedHosted   = "hosted"
)

type cachedNotes struct {
	releases []release.Release
	at       time.Time
}

// UpdatesHandler answers "what changed, and which of MY components are behind".
//
// Fail-open throughout: an unreachable or malformed remote file falls back to
// the copy embedded in this build, which by construction describes everything
// up to this build and therefore reads as "no updates" - never as an error, and
// never as an empty version that would flag every component.
type UpdatesHandler struct {
	d Deps

	// urls maps a feed name to where its release notes live. An empty URL means
	// "embedded only", which is the correct behaviour for an air-gapped install.
	urls map[string]string

	cacheMu sync.Mutex
	cache   map[string]cachedNotes
}

func NewUpdatesHandler(d Deps, platformURL, hostedURL string) *UpdatesHandler {
	if err := updates.ParseError(); err != nil {
		// CI validates these files, so reaching this means a build slipped
		// through. Log once at startup rather than per request.
		log.Printf("updates: embedded release notes are malformed, serving nothing: %v", err)
	}
	return &UpdatesHandler{
		d:     d,
		urls:  map[string]string{feedPlatform: platformURL, feedHosted: hostedURL},
		cache: map[string]cachedNotes{},
	}
}

// instance is one running copy of a component. Nodes have many; core and panel
// have one each.
//
// Version is empty for "not reporting", which is a real and distinct state: an
// image built before release stamping, or a node that has not checked in. It
// must never render as "behind", because an unknown version cannot be ordered
// and reporting it as behind would flag a fleet nobody has touched.
type instance struct {
	Label    string `json:"label"`
	Version  string `json:"version,omitempty"`
	Outdated bool   `json:"outdated"`
}

// componentBlock answers "is THIS component behind, and by what". Latest is the
// newest release that NAMES this component, which is the whole reason one
// version per repo does not mark everything outdated at once.
type componentBlock struct {
	// Key identifies the ROW and Service identifies the released component.
	// They are usually the same, and are not for nodes: the operator's cluster
	// and their external machines are two rows and one service, because a
	// release names `node` and both of them install it. Without a separate key
	// the panel would draw two rows with the same React key and the requirement
	// scan below would report one mandatory update twice.
	Key string `json:"key"`
	// Label is what the row is CALLED. Empty means the panel falls back to its
	// own name for the service, which is what every non-node row does.
	Label string `json:"label,omitempty"`

	Service  string `json:"service"`
	Latest   string `json:"latest,omitempty"`
	Outdated bool   `json:"outdated"`

	// Countable says whether Instances is the WHOLE set. It is for Cores and
	// nodes, which announce themselves; it is not for the panel, which is a
	// static bundle in a browser and can only ever report the one copy that
	// served this request. Rendering "1/1" for a panel behind two replicas
	// would be a claim Core is in no position to make.
	Countable bool       `json:"countable"`
	Instances []instance `json:"instances"`
}

// requiredBlock is a mandatory update the reader is subject to. Present only
// when at least one of their components is actually behind the floor.
type requiredBlock struct {
	Service    string    `json:"service"`
	MinVersion string    `json:"minVersion"`
	Deadline   time.Time `json:"deadline"`
	Passed     bool      `json:"passed"`
	Note       string    `json:"note,omitempty"`
}

type updatesResponse struct {
	Feed string `json:"feed"`
	// Latest is the newest published release; Seen is the newest release this
	// user has acknowledged. The panel badges on the two differing, which is why
	// Seen is stored server-side rather than in the browser: an operator who
	// dismissed a release at their desk should not be nagged again on a laptop.
	Latest     string            `json:"latest,omitempty"`
	Seen       string            `json:"seen,omitempty"`
	Components []componentBlock  `json:"components"`
	Releases   []release.Release `json:"releases"`
	Required   []requiredBlock   `json:"required,omitempty"`
}

// GetUpdates GET /api/updates.
//
// Two audiences out of one mechanism. An admin runs the platform and sees the
// platform notes plus every component; a customer runs a node or nothing and
// sees the customer notes plus their OWN nodes. The old endpoint was admin-only,
// which left a BYON customer with no way to learn their node needed updating.
func (h *UpdatesHandler) GetUpdates(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	isAdmin, _ := r.Context().Value("isAdmin").(bool)

	feed, releases := h.audienceNotes(r.Context(), isAdmin)

	resp := updatesResponse{Feed: feed, Releases: releases, Components: []componentBlock{}}
	if len(releases) > releasesShown {
		resp.Releases = releases[:releasesShown]
	}
	if latest, ok := release.Latest(releases); ok {
		resp.Latest = latest.Version.String()
	}
	if h.d.Store != nil && userID != "" {
		if seen, err := h.d.Store.GetUserUpdatesSeen(userID); err == nil {
			resp.Seen = seen
		}
	}

	resp.Components = append(resp.Components, h.components(r, releases, userID, isAdmin)...)
	// Required mirrors the connect gate, so it reads the gate's set (both
	// embedded files), not this reader's feed: a requirement in the other file,
	// or one published after this Core was built, would otherwise be shown and
	// enforced differently.
	resp.Required = requiredBlocks(resp.Components, updates.Requirements(), time.Now())

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// requiredBlocks is one mandatory-update line per SERVICE, not per row. The node
// rows are two views of one component, so a mandatory node update would
// otherwise be announced twice with identical text.
//
// Requirement answers for ONE version, and instances on different versions get
// different deadlines. The line takes the earliest deadline and the newest floor
// across every instance of the service, because the connect gate judges each
// node by its own version: the first instance found may be the most lenient one
// while another is already being refused.
func requiredBlocks(comps []componentBlock, releases []release.Release, now time.Time) []requiredBlock {
	type pick struct {
		req *release.Required
		min release.Version
	}
	var order []string
	picks := map[string]*pick{}
	for _, c := range comps {
		for _, inst := range c.Instances {
			v, err := release.ParseVersion(inst.Version)
			if err != nil {
				continue
			}
			req, min, ok := release.Requirement(releases, c.Service, v)
			if !ok {
				continue
			}
			p := picks[c.Service]
			if p == nil {
				order = append(order, c.Service)
				picks[c.Service] = &pick{req: req, min: min}
				continue
			}
			if req.Deadline.Before(p.req.Deadline) {
				p.req = req
			}
			if min.Compare(p.min) > 0 {
				p.min = min
			}
		}
	}
	var out []requiredBlock
	for _, svc := range order {
		p := picks[svc]
		out = append(out, requiredBlock{
			Service:    svc,
			MinVersion: p.min.String(),
			Deadline:   p.req.Deadline,
			Passed:     now.After(p.req.Deadline),
			Note:       p.req.Note,
		})
	}
	return out
}

// components builds the per-component picture for this reader.
//
// Every service NAMED by any release appears, even when nothing reports a
// version for it. Omitting those would hide exactly the case the per-component
// split exists for: a component the operator never updates and is never told
// about. It shows up with no instances and no version rather than silently.
func (h *UpdatesHandler) components(r *http.Request, releases []release.Release, userID string, isAdmin bool) []componentBlock {
	byService := map[string][]instance{}

	if isAdmin {
		// The panel is no longer its own component. It is compiled into this
		// binary, so its version IS Core's - there is nothing left that could
		// drift, and a second row reporting the same number would only invite
		// the question of what to do when they disagree.
		//
		// The ?panelVersion= query the panel used to send is gone with it, and
		// unknown query parameters are ignored, so an older bundle asking is
		// answered correctly rather than refused.
		byService["core"] = h.cores(r.Context(), releases)
	}

	// Node rows are keyed by KIND, so the operator's cluster and their external
	// machines are counted apart. A reader with no external nodes gets no
	// external row at all - see the loop below.
	byKey := map[string][]instance{}
	for _, n := range h.nodes(r.Context(), userID, isAdmin) {
		label := n.DisplayName
		if label == "" {
			label = n.Name
		}
		key := "node"
		if n.kind == models.NodeKindExternal {
			key = nodeExternalKey
		}
		byKey[key] = append(byKey[key], versioned(label, n.version, releases, "node"))
	}

	// Order by the shared service list so the panel renders the same order every
	// time, then anything else the releases mention.
	out := []componentBlock{}
	for _, svc := range release.Services {
		insts, seen := byService[svc]
		named := newestNaming(releases, svc)
		if svc == "node" {
			// The cluster row always appears when a release names the node,
			// even with nothing reporting - that is the case the per-component
			// split exists for. The external row appears only when the operator
			// HAS external machines: an always-present empty row would read as
			// a component that is missing rather than one that does not apply.
			out = append(out, nodeBlock("node", "", named, byKey["node"], true))
			if ext := byKey[nodeExternalKey]; len(ext) > 0 {
				out = append(out, nodeBlock(nodeExternalKey, "External nodes", named, ext, true))
			}
			continue
		}
		if !seen && named == "" {
			continue
		}
		block := componentBlock{
			Key: svc, Service: svc, Latest: named,
			Countable: svc != "panel", Instances: insts,
		}
		if block.Instances == nil {
			block.Instances = []instance{}
		}
		for _, i := range insts {
			if i.Outdated {
				block.Outdated = true
			}
		}
		out = append(out, block)
	}
	return out
}

// nodeExternalKey is the row id for machines the operator registered but does
// not run in the cluster. Not a service: a release names `node` for both.
const nodeExternalKey = "node-external"

// nodeBlock builds one node row. Both rows report against the `node` service,
// so "what should it be on" is one answer given twice rather than two answers
// that could drift.
func nodeBlock(key, label, latest string, insts []instance, countable bool) componentBlock {
	b := componentBlock{
		Key: key, Label: label, Service: "node", Latest: latest,
		Countable: countable, Instances: insts,
	}
	if b.Instances == nil {
		b.Instances = []instance{}
	}
	for _, i := range insts {
		if i.Outdated {
			b.Outdated = true
		}
	}
	return b
}

// cores lists every Core instance, not just the one answering this request.
//
// Cores announce themselves in Redis with a 30s TTL (services.CoreHeartbeat),
// which is the same set the shared-storage checks already gate on, so the
// updates view reports the fleet the operator actually runs. Reporting only
// the responder is how two Cores on two builds read as "1/1, current" - the
// one case a per-component version is supposed to catch.
//
// Falls back to this Core alone when Redis cannot answer: a version this
// process knows for certain beats an error, and an under-reported fleet is the
// same thing the caller saw before. The responder is always present even if its
// own heartbeat has not landed yet.
func (h *UpdatesHandler) cores(ctx context.Context, releases []release.Release) []instance {
	self := h.d.CoreID
	if self == "" {
		self = "core"
	}

	hbs, err := services.OnlineCores(ctx, h.d.Redis)
	if err != nil || len(hbs) == 0 {
		if err != nil {
			log.Printf("updates: listing Core instances: %v", err)
		}
		return []instance{versioned(self, h.d.ReleaseVersion, releases, "core")}
	}

	out := make([]instance, 0, len(hbs)+1)
	seenSelf := false
	for _, hb := range hbs {
		v := hb.Version
		if hb.ID == self {
			seenSelf = true
			// Trust the running process over its own heartbeat, which may
			// predate a restart onto a new build by up to its TTL.
			v = h.d.ReleaseVersion
		}
		out = append(out, versioned(hb.ID, v, releases, "core"))
	}
	if !seenSelf {
		out = append(out, versioned(self, h.d.ReleaseVersion, releases, "core"))
	}
	return out
}

// versioned builds one instance, deciding outdated through the shared predicate
// so no call site can invent its own rule.
func versioned(label, raw string, releases []release.Release, service string) instance {
	v, err := release.ParseVersion(raw)
	if err != nil {
		return instance{Label: label} // not reporting; never outdated
	}
	return instance{Label: label, Version: v.String(), Outdated: release.Outdated(releases, service, v)}
}

// newestNaming is the newest release that names this service, i.e. the version
// an instance has to reach to be current. Empty when no release ever names it.
func newestNaming(releases []release.Release, service string) string {
	for _, r := range releases {
		if r.Names(service) {
			return r.Version.String()
		}
	}
	return ""
}

type nodeVersion struct {
	Name        string
	DisplayName string
	version     string
	kind        models.NodeKind
}

// nodes returns the nodes this reader may see, each with the version its
// heartbeat last reported and the kind it is.
//
// A non-admin sees only nodes they own: a BYON customer's update view is about
// their own hardware, and the fleet is not theirs to enumerate.
//
// An admin sees the machines they are RESPONSIBLE FOR - the cluster and the
// external ones - and not their customers' BYON nodes. A customer's node being
// behind is that customer's update to install, announced to them through their
// own feed; putting it here made the operator's "is anything of mine outdated"
// answer depend on hardware they cannot touch.
func (h *UpdatesHandler) nodes(ctx context.Context, userID string, isAdmin bool) []nodeVersion {
	if h.d.Store == nil {
		return nil
	}
	all, err := h.d.Store.ListNodes()
	if err != nil {
		return nil
	}
	beats := services.LoadHeartbeats(ctx, h.d.Redis)

	out := []nodeVersion{}
	for _, n := range all {
		kind := n.Kind()
		if isAdmin {
			if kind == models.NodeKindBYON {
				continue
			}
		} else if n.OwnerID == nil || *n.OwnerID != userID {
			continue
		}
		nv := nodeVersion{Name: n.Name, DisplayName: n.DisplayName, kind: kind}
		if hb := beats[n.Token]; hb != nil {
			nv.version = hb.ReleaseVersion
		}
		out = append(out, nv)
	}
	return out
}

// audienceNotes picks the feed for the caller. The customer notes describe the
// hosted service (plans, storefront, BYON), so on a self-hosted platform a
// non-admin has no feed at all: no list and, because Latest is empty, no badge.
func (h *UpdatesHandler) audienceNotes(ctx context.Context, isAdmin bool) (string, []release.Release) {
	if isAdmin {
		return feedPlatform, h.notes(ctx, feedPlatform)
	}
	if !h.d.Hosted {
		return feedHosted, nil
	}
	return feedHosted, h.notes(ctx, feedHosted)
}

// notes returns a feed's releases: the remote file when it is reachable and
// parses, otherwise the copy embedded in this build.
//
// The fallback direction matters. Embedded notes describe everything up to THIS
// build, so falling back reads as "you are current" - the honest answer when we
// cannot see what has been published since. Falling back to nothing would read
// as "no version", which flags nobody, and falling back to an error would put a
// changelog outage in front of an operator's actual work.
func (h *UpdatesHandler) notes(ctx context.Context, feed string) []release.Release {
	embedded := updates.Platform()
	if feed == feedHosted {
		embedded = updates.Hosted()
	}
	url := h.urls[feed]
	if url == "" {
		return embedded
	}

	h.cacheMu.Lock()
	if c, ok := h.cache[feed]; ok && time.Since(c.at) < notesTTL {
		h.cacheMu.Unlock()
		return c.releases
	}
	h.cacheMu.Unlock()

	rs, err := fetchNotes(ctx, url)
	if err != nil {
		log.Printf("updates: %s notes unreachable, using the embedded copy: %v", feed, err)
		rs = embedded
	}
	h.cacheMu.Lock()
	h.cache[feed] = cachedNotes{releases: rs, at: time.Now()}
	h.cacheMu.Unlock()
	return rs
}

func fetchNotes(ctx context.Context, url string) ([]release.Release, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &httpStatusError{resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, notesMaxBytes))
	if err != nil {
		return nil, err
	}
	return release.Parse(body)
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return http.StatusText(e.code) }

// MarkUpdatesSeen PUT /api/me/updates-seen - acknowledge everything published
// so far, clearing this user's badge.
//
// The version comes from the SERVER, not the request body. A client that sent
// its own would let a stale panel acknowledge a release it never displayed, and
// there is nothing the caller could legitimately say here that Core does not
// already know.
func (h *UpdatesHandler) MarkUpdatesSeen(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	isAdmin, _ := r.Context().Value("isAdmin").(bool)
	if userID == "" || h.d.Store == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_, releases := h.audienceNotes(r.Context(), isAdmin)
	version := ""
	if latest, ok := release.Latest(releases); ok {
		version = latest.Version.String()
	}
	if err := h.d.Store.SetUserUpdatesSeen(userID, version); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
