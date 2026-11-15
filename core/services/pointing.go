package services

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"dylaris-core/pkg/netguard"

	"golang.org/x/sync/singleflight"
)

// The pointing check answers one question: does a customer's own domain lead
// to one of our gateway regions? It is the only place in Core that resolves a
// name a customer controls for this purpose, so every bound lives here.
//
// It COMPARES, it never connects: an answer counts only when it equals a value
// we control (an edge address from the registry, a wildcard suffix of a
// region). A bogus resolver answer therefore fails closed, the same argument
// the ownership proof makes (custom_domain_proof.go). It proves nothing about
// ownership and is never a substitute for the TXT proof.

type PointingPurpose string

const (
	PurposeVoiceRegion PointingPurpose = "voice_region" // SRV target followed one hop
	PurposeBedrock     PointingPurpose = "bedrock"      // SRV only is refused
	PurposeVoiceName   PointingPurpose = "voice_name"   // SRV only is refused (Part 3)
)

type PointingStatus string

const (
	PointingOK          PointingStatus = "ok"
	PointingNotFound    PointingStatus = "not_found"
	PointingElsewhere   PointingStatus = "elsewhere"
	PointingPartial     PointingStatus = "partial"
	PointingMultiRegion PointingStatus = "multi_region"
	PointingIPv6        PointingStatus = "ipv6"
	PointingSRVOnly     PointingStatus = "srv_only"
	PointingPrivate     PointingStatus = "private"
	// Unknown: never cached, stored or counted, and never a refusal.
	PointingLookupFailed PointingStatus = "lookup_failed"
	PointingRateLimited  PointingStatus = "rate_limited"
)

type Pointing struct {
	Status PointingStatus
	Region string   // set when Status is ok
	Via    string   // "regional" | "cname" | "a" | "srv", set when Status is ok
	Found  []string // public non-edge addresses seen, sorted; never a private one
}

// Known is true only for the eight verdicts about the domain. A whitelist,
// so the zero value (and anything a later caller makes up) is unknown, never
// a "no".
func (p Pointing) Known() bool {
	switch p.Status {
	case PointingOK, PointingNotFound, PointingElsewhere, PointingPartial,
		PointingMultiRegion, PointingIPv6, PointingSRVOnly, PointingPrivate:
		return true
	}
	return false
}

// PointingResolver is the direct-mode resolver plus the canonical name.
type PointingResolver interface {
	ServerDomainResolver
	LookupCNAME(ctx context.Context, host string) (string, error)
}

// NewNetPointingResolver is the direct-mode resolver: pure Go, absolute names.
func NewNetPointingResolver() PointingResolver {
	return &netServerDomainResolver{r: &net.Resolver{PreferGo: true}}
}

const (
	pointingTimeout     = 3 * time.Second
	pointingOKTTL       = 60 * time.Second
	pointingNegativeTTL = 30 * time.Second
	pointingSlots       = 8
	// pointingPerOwner bounds the lookups one owner has RUNNING at once,
	// counted until each lookup ends rather than until its caller leaves, so
	// one tenant holds at most this many of the pointingSlots (CheckFor).
	pointingPerOwner   = 2
	pointingEdgeMemory = 15 * time.Minute
	// ponytail: a full cache of live entries stops caching new names rather
	// than evicting; every entry is gone within 60 s, so it heals itself.
	pointingCacheMax = 4096
)

var pointingFailed = Pointing{Status: PointingLookupFailed}

type pointingKey struct {
	name    string
	purpose PointingPurpose
}

type pointingCached struct {
	p     Pointing
	until time.Time
}

type pointingSeen struct {
	region string
	at     time.Time
}

// PointingChecker is ONE shared instance per Core (main.go).
//
// ponytail: cache, slots and edge memory are per replica (two replicas may
// answer differently for up to 60 s); a shared cache is not worth a Redis key.
type PointingChecker struct {
	res   PointingResolver
	edges func(ctx context.Context) []GatewayEdgeInfo
	// admitted is the last line of the "verified names only" rule: a name is
	// resolved only when it is, or lies under, a live gateway route. Nil admits
	// nothing.
	admitted func(ctx context.Context, name string) (bool, error)
	now      func() time.Time
	flight   singleflight.Group
	slots    chan struct{}

	mu       sync.Mutex
	inflight map[string]int // owner -> lookups running (CheckFor)
	cache    map[pointingKey]pointingCached
	// Edge addresses and wildcard suffixes seen in the registry recently. A
	// registry key lives about 10 s and is gone while an edge restarts, which
	// must not make the domains pointing at it look foreign.
	ips       map[netip.Addr]pointingSeen
	wildcards map[string]pointingSeen // ".eu.dylaris.com" -> region
}

func NewPointingChecker(res PointingResolver, edges func(ctx context.Context) []GatewayEdgeInfo) *PointingChecker {
	return &PointingChecker{
		res: res, edges: edges, now: time.Now,
		slots:     make(chan struct{}, pointingSlots),
		cache:     map[pointingKey]pointingCached{},
		inflight:  map[string]int{},
		ips:       map[netip.Addr]pointingSeen{},
		wildcards: map[string]pointingSeen{},
	}
}

// SetAdmitted wires the admission every resolved name must pass. Call once at
// boot. Until it is set no own domain is resolved at all.
func (c *PointingChecker) SetAdmitted(fn func(ctx context.Context, name string) (bool, error)) {
	c.admitted = fn
}

// Check answers within pointingTimeout. allow is called once per CACHE MISS
// (nil = no per-caller budget: background re-checks, external API calls).
//
// The caller must already have admitted name as a route of ITS caller (A4);
// the admission hook here only refuses names that are no route at all, so a
// forgotten caller check can never turn this into a resolver for any name.
//
// A nil checker answers lookup_failed.
func (c *PointingChecker) Check(ctx context.Context, name string, purpose PointingPurpose, allow func() bool) Pointing {
	return c.CheckFor(ctx, "", name, purpose, allow)
}

// CheckFor is Check with the owner of the route: one owner has at most
// pointingPerOwner lookups running at once, a further one answers
// lookup_failed at once. A caller that leaves early does not free its count:
// the lookup it started keeps a slot until it ends. Joining a lookup already
// running for the same name costs nothing. "" is no per-owner bound
// (background passes, which run serially).
func (c *PointingChecker) CheckFor(ctx context.Context, owner, name string, purpose PointingPurpose, allow func() bool) Pointing {
	if c == nil {
		return pointingFailed
	}
	ctx, cancel := context.WithTimeout(ctx, pointingTimeout)
	defer cancel()
	name = normaliseHost(name)
	if !pointingNameShape(name) {
		return Pointing{Status: PointingNotFound}
	}
	edges := c.edges(ctx)
	if len(edges) == 0 {
		// The registry could not be read or no edge is up: that says nothing
		// about the customer's DNS.
		return pointingFailed
	}
	c.remember(edges)
	if region, ok := VoiceRegionFor(name, edges); ok {
		return Pointing{Status: PointingOK, Region: region, Via: "regional"}
	}
	if c.admitted == nil {
		return pointingFailed
	}
	if ok, err := c.admitted(ctx, name); err != nil || !ok {
		if err == nil {
			log.Printf("[pointing] refused to resolve %s: not a gateway route", name)
		}
		return pointingFailed
	}
	key := pointingKey{name, purpose}
	if p, ok := c.cached(key); ok {
		return p
	}
	if allow != nil && !allow() {
		return Pointing{Status: PointingRateLimited}
	}
	// The edge read or the admission may have used up the deadline: do not
	// start work nobody waits for.
	if ctx.Err() != nil {
		return pointingFailed
	}
	// The shared lookup must not end because the caller who started it left.
	shared := context.WithoutCancel(ctx)
	ch := c.flight.DoChan(string(purpose)+" "+name, func() (any, error) {
		// Counted inside the flight, so the count lives exactly as long as the
		// lookup. A name is one owner's route, so only that owner joins it.
		if owner != "" {
			if !c.ownerEnter(owner) {
				return pointingFailed, nil
			}
			defer c.ownerLeave(owner)
		}
		return c.lookup(shared, key, edges), nil
	})
	select {
	case r := <-ch:
		return r.Val.(Pointing)
	case <-ctx.Done():
		return pointingFailed
	}
}

// RegionOf is step 1 alone (no DNS): the region of a name one label under an
// online edge wildcard, exactly VoiceRegionFor. Exported so read paths can
// classify a name without resolving.
func (c *PointingChecker) RegionOf(ctx context.Context, name string) (string, bool) {
	if c == nil {
		return "", false
	}
	return VoiceRegionFor(normaliseHost(name), c.edges(ctx))
}

// Admits is Check's admission alone (no DNS): whether name is a regional name
// or lies under a live gateway route. known is false when that cannot be told
// (no admission wired, a read error), which a caller must treat as unknown.
func (c *PointingChecker) Admits(ctx context.Context, name string) (admitted, known bool) {
	if c == nil || c.admitted == nil {
		return false, false
	}
	name = normaliseHost(name)
	ok, err := c.admitted(ctx, name)
	if err != nil {
		return false, false
	}
	if !ok {
		// Check answers a regional name before it asks the admission.
		if _, regional := VoiceRegionFor(name, c.edges(ctx)); regional {
			return true, true
		}
	}
	return ok, true
}

// pointingNameShape is the cheap sanity check before anything is asked.
func pointingNameShape(name string) bool {
	if name == "" || len(name) > 253 || !strings.Contains(name, ".") {
		return false
	}
	_, err := netip.ParseAddr(name)
	return err != nil
}

// lookup runs one check in a slot. A full pool answers "unknown" at once
// rather than queueing: a slow or hostile resolver must not collect waiters.
func (c *PointingChecker) lookup(ctx context.Context, key pointingKey, edges []GatewayEdgeInfo) Pointing {
	ctx, cancel := context.WithTimeout(ctx, pointingTimeout)
	defer cancel()
	select {
	case c.slots <- struct{}{}:
	default:
		return pointingFailed
	}
	defer func() { <-c.slots }()
	p, transient := c.resolve(ctx, key.name, key.purpose, edges)
	if !transient {
		c.put(key, p)
	}
	return p
}

// resolve is rules 4 and 5: at most 3 + 1 + 3 resolver INTERFACE calls
// (CNAME, A, AAAA; SRV; CNAME, A, AAAA of one SRV target). Each call is more
// than one DNS query: Go's pure resolver sends its query types (LookupCNAME
// alone asks A and AAAA) to every resolv.conf server, with resolv.conf's
// attempts. The number of queries is bounded by that product; the time, and
// so every retry, by the one pointingTimeout context.
//
// transient: do not cache p. Set for a failed SRV hop, which the caller sees
// as not_found (see below) but which the next try must ask again.
func (c *PointingChecker) resolve(ctx context.Context, name string, purpose PointingPurpose, edges []GatewayEdgeInfo) (p Pointing, transient bool) {
	if p, decided := c.judge(ctx, name, purpose); decided {
		return p, false
	}
	srvs, err := c.res.LookupSRV(ctx, "_minecraft._tcp."+name)
	if lookupFailed(err) {
		return pointingFailed, false
	}
	if len(srvs) == 0 {
		return Pointing{Status: PointingNotFound}, false
	}
	if purpose != PurposeVoiceRegion {
		return Pointing{Status: PointingSRVOnly}, false
	}
	targets := make([]string, 0, len(srvs))
	for _, s := range srvs {
		targets = append(targets, normaliseHost(s.Target))
	}
	sort.Strings(targets)
	target := targets[0]
	if !pointingNameShape(target) {
		return Pointing{Status: PointingNotFound}, false
	}
	if region, ok := VoiceRegionFor(target, edges); ok {
		return Pointing{Status: PointingOK, Region: region, Via: "srv"}, false
	}
	// One hop: the target's own SRV is never asked. The target is a name the
	// customer chose and is resolved without admission, so its answer must
	// not tell "exists inside our network" (a private answer, which an
	// internal resolver such as Docker's gives for a service name) from "does
	// not exist", nor a failing internal zone from a missing one: all of them
	// read as a target with no address. Only a failed hop is not cached, so a
	// transient SERVFAIL of a real target does not stick for the negative TTL;
	// status and sentence stay the same.
	p, decided := c.judge(ctx, target, purpose)
	switch {
	case p.Status == PointingLookupFailed:
		return Pointing{Status: PointingNotFound}, true
	case !decided, p.Status == PointingPrivate,
		p.Status == PointingPartial && len(p.Found) == 0:
		return Pointing{Status: PointingNotFound}, false
	case p.Status == PointingOK:
		p.Via = "srv"
	}
	return p, false
}

// judge asks CNAME, A and AAAA of one name concurrently. decided is false
// only when the name has no A answer, which is where SRV takes over.
//
// For Bedrock and a white-label voice name ANY AAAA in the answer is ipv6, at
// the end of an alias too: players connect to that very name, and our
// regional names carry no AAAA, so an AAAA always leads somewhere else. It
// therefore waits for the AAAA even when the alias is our own name, which
// also keeps the verdict the same whichever hop Go reports. The voice region
// lookup keeps the alias exemption below: it only picks a region, and the mod
// is told our own name.
func (c *PointingChecker) judge(ctx context.Context, name string, purpose PointingPurpose) (p Pointing, decided bool) {
	strictV6 := purpose == PurposeBedrock || purpose == PurposeVoiceName
	type cnameRes struct {
		name string
		err  error
	}
	type addrRes struct {
		addrs []netip.Addr
		err   error
	}
	// Buffered, so a lookup that returns after we gave up can still exit.
	cnCh := make(chan cnameRes, 1)
	aCh := make(chan addrRes, 1)
	a6Ch := make(chan addrRes, 1)
	go func() { n, err := c.res.LookupCNAME(ctx, name); cnCh <- cnameRes{n, err} }()
	go func() { v, err := c.res.LookupA(ctx, name); aCh <- addrRes{v, err} }()
	go func() { v, err := c.res.LookupAAAA(ctx, name); a6Ch <- addrRes{v, err} }()

	var a, a6 addrRes
	failed, aliased := false, false
	ourAlias := "" // region of an alias to our own name (strictV6 only)
	for pending := 3; pending > 0; pending-- {
		select {
		case <-ctx.Done():
			return pointingFailed, true
		case cn := <-cnCh:
			if lookupFailed(cn.err) {
				failed = true
				continue
			}
			// Our own name decides at once: its addresses are ours to keep
			// right, so a slow or failing AAAA says nothing then.
			if canon := normaliseHost(cn.name); cn.err == nil && canon != name {
				aliased = true
				if region, ok := c.wildcardRegion(canon); ok {
					if !strictV6 {
						return Pointing{Status: PointingOK, Region: region, Via: "cname"}, true
					}
					ourAlias = region
				}
			}
		case a = <-aCh:
			failed = failed || lookupFailed(a.err)
		case a6 = <-a6Ch:
			failed = failed || lookupFailed(a6.err)
		}
	}
	if failed {
		return pointingFailed, true
	}
	if ourAlias != "" {
		if len(a6.addrs) > 0 {
			return Pointing{Status: PointingIPv6}, true
		}
		return Pointing{Status: PointingOK, Region: ourAlias, Via: "cname"}, true
	}
	if len(a.addrs) == 0 {
		return Pointing{}, false
	}

	regions := map[string]bool{}
	known, private := 0, false
	var found []string
	for _, ip := range a.addrs {
		ip = ip.Unmap()
		if region, ok := c.edgeRegion(ip); ok {
			known++
			regions[region] = true
			continue
		}
		if netguard.Disallowed(net.IP(ip.AsSlice())) {
			private = true // never echoed
			continue
		}
		if s := ip.String(); !slices.Contains(found, s) {
			found = append(found, s)
		}
	}
	sort.Strings(found)
	switch {
	case known > 0 && (len(found) > 0 || private):
		return Pointing{Status: PointingPartial, Found: found}, true
	case known > 0 && len(regions) > 1:
		return Pointing{Status: PointingMultiRegion}, true
	case known > 0 && len(a6.addrs) > 0 && (!aliased || strictV6):
		return Pointing{Status: PointingIPv6}, true
	case known > 0:
		// Voice: an alias holds no AAAA of its own: with every A ours, the
		// AAAA belong to the chain end, which is ours. Go reports the first
		// hop or the end of a longer chain at random, so an alias is ok/cname
		// either way, never a flip to ipv6.
		var region string
		for r := range regions {
			region = r // exactly one
		}
		via := "a"
		if aliased {
			via = "cname"
		}
		return Pointing{Status: PointingOK, Region: region, Via: via}, true
	case private:
		return Pointing{Status: PointingPrivate, Found: found}, true
	}
	return Pointing{Status: PointingElsewhere, Found: found}, true
}

// remember refreshes the edge memory from the registry and drops what has
// not been seen for pointingEdgeMemory.
func (c *PointingChecker) remember(edges []GatewayEdgeInfo) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range edges {
		if e.Status != "online" || e.Region == "" {
			continue
		}
		if ip, err := netip.ParseAddr(strings.TrimSpace(e.IP)); err == nil {
			c.ips[ip.Unmap()] = pointingSeen{e.Region, now}
		}
		if strings.HasPrefix(e.Wildcard, "*.") {
			c.wildcards[strings.ToLower(e.Wildcard[1:])] = pointingSeen{e.Region, now}
		}
	}
	for ip, s := range c.ips {
		if now.Sub(s.at) > pointingEdgeMemory {
			delete(c.ips, ip)
		}
	}
	for w, s := range c.wildcards {
		if now.Sub(s.at) > pointingEdgeMemory {
			delete(c.wildcards, w)
		}
	}
}

// edgeRegion: a registered (or recently registered) edge address is ours
// whatever its range; a LAN self-hoster's edge has a private one.
func (c *PointingChecker) edgeRegion(ip netip.Addr) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.ips[ip]
	return s.region, ok
}

// wildcardRegion is the region of a name exactly one label under a known
// wildcard suffix.
func (c *PointingChecker) wildcardRegion(name string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for suffix, s := range c.wildcards {
		label, found := strings.CutSuffix(name, suffix)
		if found && label != "" && !strings.Contains(label, ".") {
			return s.region, true
		}
	}
	return "", false
}

func (c *PointingChecker) ownerEnter(owner string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight[owner] >= pointingPerOwner {
		return false
	}
	c.inflight[owner]++
	return true
}

func (c *PointingChecker) ownerLeave(owner string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight[owner]--; c.inflight[owner] <= 0 {
		delete(c.inflight, owner)
	}
}

func (c *PointingChecker) cached(key pointingKey) (Pointing, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[key]
	if !ok {
		return Pointing{}, false
	}
	if !c.now().Before(e.until) {
		delete(c.cache, key)
		return Pointing{}, false
	}
	p := e.p
	p.Found = slices.Clone(p.Found)
	return p, true
}

func (c *PointingChecker) put(key pointingKey, p Pointing) {
	var ttl time.Duration
	switch p.Status {
	case PointingLookupFailed, PointingRateLimited:
		return
	case PointingOK:
		ttl = pointingOKTTL
	default:
		ttl = pointingNegativeTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.cache) >= pointingCacheMax {
		for k, e := range c.cache {
			if !now.Before(e.until) {
				delete(c.cache, k)
			}
		}
		if len(c.cache) >= pointingCacheMax {
			return
		}
	}
	c.cache[key] = pointingCached{p: p, until: now.Add(ttl)}
}

// PointingUnknownText is the sentence for a check that could not decide
// (lookup_failed, rate_limited, anything not Known), "" for a verdict.
func PointingUnknownText(p Pointing, name string) string {
	switch {
	case p.Known():
		return ""
	case p.Status == PointingRateLimited:
		return "Too many DNS checks. Wait a minute."
	}
	return fmt.Sprintf("The lookup of %s failed right now. Nothing was changed. Try again in a moment.", name)
}

// PointingRefusal is the customer's sentence for a VERDICT that is not ok
// (spec section 11). It names "the gateway", never "us": a customer Core on the
// managed gateway passes it verbatim to its own users, for whom "us" would be
// their hoster. targets are the full CNAME targets from CNAMETargets;
// none means the operator never set the label. "" for ok and for an unknown
// verdict, so no caller can refuse on "could not check" by accident: branch
// on Known first and use PointingUnknownText for the rest.
func PointingRefusal(p Pointing, name string, targets []string, purpose PointingPurpose) string {
	to := "the address shown under Your own domains"
	switch len(targets) {
	case 0:
	case 1:
		to = targets[0]
	default:
		to = "whichever of these is your region (" + strings.Join(targets, ", ") + ")"
	}
	if !p.Known() || p.Status == PointingOK {
		return ""
	}
	found := strings.Join(p.Found, ", ")
	if found == "" && p.Status == PointingPartial {
		found = "an address that is not on the public internet" // never echoed
	}
	switch p.Status {
	case PointingPartial:
		return fmt.Sprintf("%s points to the gateway and also to %s. Remove the records that do not lead to the gateway, or use a CNAME to %s.", name, found, to)
	case PointingMultiRegion:
		one := to
		if len(targets) > 1 {
			one = strings.Join(targets, ", ")
		}
		return fmt.Sprintf("%s points at more than one of the gateway's regions. Point it at one: %s.", name, one)
	case PointingIPv6:
		return fmt.Sprintf("%s has an IPv6 (AAAA) record that does not lead to the gateway. Remove it, or use a CNAME to %s.", name, to)
	case PointingPrivate:
		return fmt.Sprintf("%s resolves to an address that is not on the public internet.", name)
	}
	if purpose == PurposeVoiceRegion {
		// Name what was found, as the Bedrock sentence does: "not yet" reads as
		// "wait" to a customer whose record points somewhere else.
		state := "does not point at the gateway yet"
		if p.Status == PointingElsewhere && found != "" {
			state = "points to " + found + ", not to the gateway"
		}
		return fmt.Sprintf("Voice chat needs to know which region this server's players arrive in. "+
			"%s %s: add a CNAME from %s to %s, or add an address on one of the gateway's domains.", name, state, name, to)
	}
	bedrock := purpose == PurposeBedrock
	switch p.Status {
	case PointingElsewhere:
		if bedrock {
			return fmt.Sprintf("%s points to %s, not to the gateway. Add a CNAME from %s to %s. "+
				"A proxy or DDoS filter in front of the domain cannot carry Bedrock.", name, found, name, to)
		}
		return fmt.Sprintf("%s points to %s, not to the gateway. Add a CNAME from %s to %s.", name, found, name, to)
	case PointingSRVOnly:
		if bedrock {
			return fmt.Sprintf("%s reaches the gateway only through an SRV record, and Bedrock ignores SRV records. "+
				"Add a CNAME from %s to %s, or give Bedrock to an address on one of the gateway's domains.", name, name, to)
		}
		return fmt.Sprintf("%s reaches the gateway only through an SRV record, and a voice name needs an address. "+
			"Add a CNAME from %s to %s.", name, name, to)
	}
	if bedrock {
		return fmt.Sprintf("%s does not resolve yet. Bedrock players connect straight to the address: "+
			"add a CNAME from %s to %s, wait a few minutes, then try again.", name, name, to)
	}
	return fmt.Sprintf("%s does not resolve yet. Add a CNAME from %s to %s, wait a few minutes, then try again.", name, name, to)
}
