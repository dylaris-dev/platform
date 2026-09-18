package services

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// dnsLookupTimeout bounds every individual DNS lookup and TCP dial so one
// unreachable target can never stall the whole check.
const dnsLookupTimeout = 3 * time.Second

// Public resolvers we query directly (instead of the host resolver) so the
// result reflects the PUBLIC view of DNS and bypasses any local cache. We try
// Cloudflare first, fall back to Google.
var publicDNSServers = []string{"1.1.1.1:53", "8.8.8.8:53"}

// DNSRecordCheck is one computed-and-verified DNS record in the contract.
type DNSRecordCheck struct {
	Category string   `json:"category"` // player | wildcard | cname | panel
	Type     string   `json:"type"`     // A | CNAME
	Name     string   `json:"name"`     // the FQDN the operator must create
	Expected []string `json:"expected"` // edge IPs (empty for panel: operator origin)
	Actual   []string `json:"actual"`   // what public DNS currently returns
	Status   string   `json:"status"`   // ok | mismatch | missing | unresolved
	Hint     string   `json:"hint"`
}

// DNSReachabilityCheck is one TCP-dial result in the contract.
type DNSReachabilityCheck struct {
	Target string `json:"target"`
	OK     bool   `json:"ok"`
	Hint   string `json:"hint"`
}

// DNSCheckResult is the full GET /api/gateway/dns-check payload.
type DNSCheckResult struct {
	Success      bool                   `json:"success"`
	Records      []DNSRecordCheck       `json:"records"`
	Reachability []DNSReachabilityCheck `json:"reachability"`
	Edges        []string               `json:"edges"`
	CheckedAt    string                 `json:"checkedAt"`
}

// DNSCheckConfig is the operator's OWN configuration the checker is bound to.
// Nothing here is user-supplied at request time — the handler fills it from
// FRONTEND_URL + the settings store, so there is no arbitrary-host / SSRF or
// recon surface: we only ever resolve and dial the operator's own domains and
// the registered edge IPs.
type DNSCheckConfig struct {
	PanelHost       string   // host of FRONTEND_URL (no scheme/port)
	PanelDialTarget string   // host:port to TCP-dial for the panel
	HosterDomains   []string // player base domains, e.g. "play.example.com"
	CustomDomainsOn bool     // gateway_custom_domains_enabled == "true"
	CNAMETarget     string   // gateway_cname_target (only when CustomDomainsOn)

	// APIHost is the hostname the panel's browser calls, from PANEL_API_URL, and
	// it is EMPTY for the normal deployment - Core serves the panel, so the
	// browser calls /api on the origin it was loaded from.
	//
	// It used to be read from core_public_url, which is a different question
	// with a different answer: that setting is the base a node downloads a built
	// pack from. An operator who had never touched it was told no API address was
	// set, while the panel it was supposedly breaking worked fine.
	APIHost       string
	APIDialTarget string

	// BeamHost is the relay the desktop client dials for REMOTE access, from the
	// override or discovery. Also missing, and for a different reason: relay
	// records are planned by the gateway Hub, not by Core, so nothing here had a
	// reason to know the name. It is still the name a user hits when Beam cannot
	// reach their server from outside.
	BeamHost       string
	BeamDialTarget string
}

// apiOriginRow decides what the report says about the API name.
//
// separate=false means there is no second name and the row it returns is the
// whole answer: the panel calls /api on its own origin, which is the shape of
// the software and not a gap in the configuration. separate=true means
// PANEL_API_URL genuinely names another host, and that host has to resolve or
// every request in the panel fails - the row is then resolved like any other.
func apiOriginRow(apiHost, panelHost string) (DNSRecordCheck, bool) {
	apiHost = strings.ToLower(strings.TrimSpace(apiHost))
	if apiHost != "" && !strings.EqualFold(apiHost, strings.TrimSpace(panelHost)) {
		return DNSRecordCheck{Category: "api", Type: "A", Name: apiHost, Expected: []string{}}, true
	}
	return DNSRecordCheck{
		Category: "api",
		Type:     "A",
		Name:     "same origin as the panel",
		Expected: []string{},
		Actual:   []string{},
		Status:   "info",
		Hint: "Nothing to create. Core serves the panel and the API together, so the browser " +
			"calls /api on the panel's own domain. A separate record is only needed if you set " +
			"PANEL_API_URL to a second hostname.",
	}, false
}

// publicResolver builds a net.Resolver that dials the public DNS servers
// directly. PreferGo forces Go's own resolver so the custom Dial is actually
// used (the cgo resolver would ignore it).
func publicResolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: dnsLookupTimeout}
			var lastErr error
			for _, server := range publicDNSServers {
				// DNS over UDP/TCP — honour whichever the resolver asked for.
				conn, err := d.DialContext(ctx, network, server)
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	}
}

// RunDNSCheck computes the expected DNS records from the operator's config,
// resolves each via the public resolver, and probes reachability of the edges
// and the panel. A single failed lookup or dial yields a per-record
// unresolved/false status with a hint — never an error for the whole call.
func RunDNSCheck(ctx context.Context, rdb *redis.Client, cfg DNSCheckConfig) DNSCheckResult {
	resolver := publicResolver()

	// Edge IPs = public IPs of the ONLINE edges. This is both the "edges"
	// array and the expected target for player/wildcard/cname records.
	edgeIPs := onlineEdgeIPs(ctx, rdb)

	result := DNSCheckResult{
		Success:      true,
		Records:      []DNSRecordCheck{},
		Reachability: []DNSReachabilityCheck{},
		Edges:        edgeIPs,
		CheckedAt:    time.Now().Format(time.RFC3339),
	}

	// --- Operator-origin records. No fixed expected IP: these point at whatever
	// the operator runs, so the only answerable question is whether they resolve
	// at all. All three sit in front of something a user notices immediately when
	// it is missing. ---
	for _, origin := range []struct{ category, host, missingHint, okHint string }{
		{
			"panel", cfg.PanelHost,
			"Panel domain does not resolve. Create an A record pointing it at your panel server's public IP.",
			"Panel domain resolves.",
		},
		{
			"api", cfg.APIHost,
			"API domain does not resolve. The panel calls it for every request, so the panel loads and then does nothing. Create an A record pointing it at the same server as the panel.",
			"API domain resolves.",
		},
		{
			"beam", cfg.BeamHost,
			"Beam relay hostname does not resolve. Remote Beam connections fail; LAN and direct connections are unaffected. Point it at the relay's public IP.",
			"Beam relay hostname resolves.",
		},
	} {
		host := strings.ToLower(strings.TrimSpace(origin.host))
		// The API row is decided rather than merely resolved: on the normal
		// deployment there is no second name, and the row has to say so instead
		// of leaving a gap that reads as a missing record.
		if origin.category == "api" {
			row, separate := apiOriginRow(host, cfg.PanelHost)
			if !separate {
				result.Records = append(result.Records, row)
				continue
			}
			host = row.Name
		}
		if host == "" {
			continue
		}
		rec := DNSRecordCheck{
			Category: origin.category,
			Type:     "A",
			Name:     host,
			Expected: []string{}, // operator-specific origin, no fixed expected IP
		}
		rec.Actual = lookupIPs(ctx, resolver, host)
		if len(rec.Actual) == 0 {
			rec.Status = "unresolved"
			rec.Hint = origin.missingHint
		} else {
			rec.Status = "ok"
			rec.Hint = origin.okHint
		}
		result.Records = append(result.Records, rec)
	}

	// --- Player + wildcard records for each hoster base domain. ---
	for _, base := range cfg.HosterDomains {
		base = strings.ToLower(strings.TrimSpace(base))
		if base == "" {
			continue
		}

		// Player base: A <base> -> edge IP. INFORMATIONAL only: players always
		// connect to <sub>.<base> (a route's subdomain is mandatory, and a custom
		// domain equal to a hoster base is rejected), so the bare base is never a
		// player address. Grading it "mismatch" sent operators chasing a record
		// that nothing needs - a base legitimately pointing at a website, or not
		// existing at all, is fine. Only the wildcard row is load-bearing.
		baseRec := checkAgainstEdges(ctx, resolver, "player", base, base, edgeIPs)
		if baseRec.Status == "mismatch" || baseRec.Status == "unresolved" {
			baseRec.Status = "info"
			baseRec.Hint = "Optional: players connect to subdomains (covered by the wildcard record below), never to the bare base domain. Pointing it somewhere else (e.g. a website) or leaving it unset is fine."
		}
		result.Records = append(result.Records, baseRec)

		// Wildcard: A *.<base> -> edge IP. You can't query "*.base" directly;
		// a wildcard A record answers ANY subdomain, so we resolve a synthetic
		// label that no operator would ever create as a real host. If it
		// resolves to an edge IP, the wildcard is in place.
		wildcardProbe := "dylaris-dnscheck." + base
		rec := checkAgainstEdges(ctx, resolver, "wildcard", "*."+base, wildcardProbe, edgeIPs)
		result.Records = append(result.Records, rec)
	}

	// --- Custom-domain CNAME targets: one per hoster base. ---
	// CNAMETarget is a LABEL ("route"), expanded to route.<base> for every
	// hoster domain, so each region gets its own target and a customer points
	// their domain at the region they actually want.
	if cfg.CustomDomainsOn && cfg.CNAMETarget != "" {
		label := strings.ToLower(strings.TrimSpace(cfg.CNAMETarget))
		for _, base := range cfg.HosterDomains {
			base = strings.ToLower(strings.TrimSpace(base))
			if base == "" {
				continue
			}
			fqdn := label + "." + base
			result.Records = append(result.Records,
				checkAgainstEdges(ctx, resolver, "cname", fqdn, fqdn, edgeIPs))
		}
	}

	// --- Reachability: TCP-dial the MC ingress on each edge + the panel. ---
	for _, ip := range edgeIPs {
		addr := net.JoinHostPort(ip, "25565")
		ok, hint := dialTCP(addr)
		result.Reachability = append(result.Reachability, DNSReachabilityCheck{
			Target: "edge " + addr,
			OK:     ok,
			Hint:   hint,
		})
	}
	for _, probe := range []struct{ label, target string }{
		{"panel", cfg.PanelDialTarget},
		{"api", cfg.APIDialTarget},
		// The relay's client port is what a Beam app outside the network
		// actually opens. Resolving the name and reaching the port are different
		// failures with different fixes, and only one of them is a DNS problem.
		{"beam relay", cfg.BeamDialTarget},
	} {
		if probe.target == "" {
			continue
		}
		ok, hint := dialTCP(probe.target)
		result.Reachability = append(result.Reachability, DNSReachabilityCheck{
			Target: probe.label + " " + probe.target,
			OK:     ok,
			Hint:   hint,
		})
	}

	return result
}

// onlineEdgeIPs returns the public IPs of the online edges, deduplicated and
// in stable order.
// OnlineEdgeIPs is the exported view of the same set, for callers outside the
// DNS check (the custom-domain proof needs it to accept an A-record).
func OnlineEdgeIPs(ctx context.Context, rdb *redis.Client) []string {
	return onlineEdgeIPs(ctx, rdb)
}

func onlineEdgeIPs(ctx context.Context, rdb *redis.Client) []string {
	edges := GetEdgesFromRedis(ctx, rdb)
	seen := map[string]struct{}{}
	ips := []string{}
	for _, e := range edges {
		if e.Status != "online" {
			continue
		}
		ip := strings.TrimSpace(e.IP)
		if ip == "" {
			continue
		}
		if _, dup := seen[ip]; dup {
			continue
		}
		seen[ip] = struct{}{}
		ips = append(ips, ip)
	}
	return ips
}

// checkAgainstEdges resolves probeName and grades it against the expected edge
// IP set. recordName is what's reported to the operator (may differ from the
// probe, e.g. "*.base" vs the synthetic wildcard probe).
func checkAgainstEdges(ctx context.Context, resolver *net.Resolver, category, recordName, probeName string, edgeIPs []string) DNSRecordCheck {
	rec := DNSRecordCheck{
		Category: category,
		Type:     "A",
		Name:     recordName,
		Expected: edgeIPs,
	}
	actual := lookupIPs(ctx, resolver, probeName)
	rec.Actual = actual

	if len(edgeIPs) == 0 {
		rec.Status = "missing"
		rec.Hint = "No online edges registered. Bring an edge online before its IP can be used as the record target."
		return rec
	}
	if len(actual) == 0 {
		rec.Status = "unresolved"
		rec.Hint = "Record does not resolve. Create an A record pointing " + recordName + " at one of the edge IPs."
		return rec
	}
	for _, a := range actual {
		for _, e := range edgeIPs {
			if a == e {
				rec.Status = "ok"
				rec.Hint = "Resolves to an edge IP."
				return rec
			}
		}
	}
	rec.Status = "mismatch"
	rec.Hint = "Resolves, but not to any current edge IP. Update the record to point at one of the edge IPs."
	return rec
}

// lookupIPs resolves a host to its IPv4/IPv6 addresses via the public
// resolver. A failed lookup returns an empty slice (graded as unresolved by
// the caller) — it never propagates an error.
func lookupIPs(ctx context.Context, resolver *net.Resolver, host string) []string {
	lctx, cancel := context.WithTimeout(ctx, dnsLookupTimeout)
	defer cancel()
	addrs, err := resolver.LookupHost(lctx, host)
	if err != nil {
		return []string{}
	}
	return addrs
}

// dialTCP attempts a short TCP connection and returns ok plus a hint.
func dialTCP(addr string) (bool, string) {
	conn, err := net.DialTimeout("tcp", addr, dnsLookupTimeout)
	if err != nil {
		return false, "Could not connect: " + err.Error()
	}
	_ = conn.Close()
	return true, ""
}
