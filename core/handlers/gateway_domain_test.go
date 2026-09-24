package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// gatewayDomainFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only GetSetting (read by resolveRouteDomain,
// loadBlockedRoutePrefixes and isSnapshotFetchHostAllowed) and
// GetGatewayRouteLimit (read by effectiveRouteLimit) are overridden. Any
// other call would panic - these tests never make one.
type gatewayDomainFakeStore struct {
	store.Store

	settings map[string]string

	routeLimits map[string]*models.GatewayRouteLimit
}

func (f *gatewayDomainFakeStore) GetSetting(key string) (string, error) {
	return f.settings[key], nil
}

func (f *gatewayDomainFakeStore) GetGatewayRouteLimit(scope string) (*models.GatewayRouteLimit, error) {
	l, ok := f.routeLimits[scope]
	if !ok {
		return nil, errors.New("not found")
	}
	return l, nil
}

func newGatewayDomainHandler(settings map[string]string) *GatewayHandler {
	return &GatewayHandler{state: &AppState{Store: &gatewayDomainFakeStore{settings: settings}}}
}

// routeDomainReqT names the exact anonymous struct type resolveRouteDomain
// takes, field-for-field including json tags, so it is identical to it and
// directly assignable without a conversion.
type routeDomainReqT = struct {
	Domain       string `json:"domain"`
	Subdomain    string `json:"subdomain"`
	HosterDomain string `json:"hosterDomain"`
	CustomDomain string `json:"customDomain"`
	TargetPort   int    `json:"targetPort"`
}

// routeDomainReq builds one.
func routeDomainReq(domain, subdomain, hosterDomain, customDomain string, targetPort int) *routeDomainReqT {
	return &struct {
		Domain       string `json:"domain"`
		Subdomain    string `json:"subdomain"`
		HosterDomain string `json:"hosterDomain"`
		CustomDomain string `json:"customDomain"`
		TargetPort   int    `json:"targetPort"`
	}{Domain: domain, Subdomain: subdomain, HosterDomain: hosterDomain, CustomDomain: customDomain, TargetPort: targetPort}
}

func hosterDomainsJSON(t *testing.T, hosters []HosterDomain) string {
	t.Helper()
	b, err := json.Marshal(hosters)
	if err != nil {
		t.Fatalf("marshal hosters: %v", err)
	}
	return string(b)
}

// TestResolveRouteDomain covers the three-path decision tree (hoster-picker,
// custom-domain, legacy-raw) plus the reserved-label and label-count guards.
func TestResolveRouteDomain(t *testing.T) {
	hostersJSON := func(t *testing.T) string {
		return hosterDomainsJSON(t, []HosterDomain{{Domain: "dylaris.com", Validation: "dns"}})
	}

	t.Run("valid hoster subdomain", func(t *testing.T) {
		h := newGatewayDomainHandler(map[string]string{"gateway_hoster_domains": hostersJSON(t)})
		got, _, err := h.resolveRouteDomain(routeDomainReq("", "myserver", "dylaris.com", "", 25565), false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "myserver.dylaris.com" {
			t.Fatalf("got %q, want myserver.dylaris.com", got)
		}
	})

	t.Run("hoster subdomain rejects an unconfigured hoster domain", func(t *testing.T) {
		h := newGatewayDomainHandler(map[string]string{"gateway_hoster_domains": hostersJSON(t)})
		_, _, err := h.resolveRouteDomain(routeDomainReq("", "myserver", "not-configured.com", "", 25565), false)
		if err == nil {
			t.Fatalf("expected an error for an unconfigured hoster domain")
		}
	})

	t.Run("valid custom domain", func(t *testing.T) {
		h := newGatewayDomainHandler(map[string]string{
			"gateway_hoster_domains":         hostersJSON(t),
			"gateway_custom_domains_enabled": "true",
		})
		got, _, err := h.resolveRouteDomain(routeDomainReq("", "", "", "sub.example.com", 25565), false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "sub.example.com" {
			t.Fatalf("got %q, want sub.example.com", got)
		}
	})

	t.Run("custom domain rejected when custom domains are not enabled", func(t *testing.T) {
		h := newGatewayDomainHandler(map[string]string{"gateway_custom_domains_enabled": "false"})
		_, _, err := h.resolveRouteDomain(routeDomainReq("", "", "", "sub.example.com", 25565), false)
		if err == nil {
			t.Fatalf("expected an error when custom domains are disabled")
		}
	})

	t.Run("reserved-label refusal on the hoster-picker path", func(t *testing.T) {
		h := newGatewayDomainHandler(map[string]string{"gateway_hoster_domains": hostersJSON(t)})
		_, _, err := h.resolveRouteDomain(routeDomainReq("", "admin", "dylaris.com", "", 25565), false)
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("err = %v, want a 'reserved' error for the default-blocklisted 'admin' subdomain", err)
		}
	})

	t.Run("reserved-label refusal on the custom-domain path (leftmost label)", func(t *testing.T) {
		h := newGatewayDomainHandler(map[string]string{"gateway_custom_domains_enabled": "true"})
		_, _, err := h.resolveRouteDomain(routeDomainReq("", "", "", "admin.example.com", 25565), false)
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("err = %v, want a 'reserved' error for the default-blocklisted 'admin' leftmost label", err)
		}
	})

	t.Run("custom domain with too few labels rejected", func(t *testing.T) {
		h := newGatewayDomainHandler(map[string]string{"gateway_custom_domains_enabled": "true"})
		_, _, err := h.resolveRouteDomain(routeDomainReq("", "", "", "onelabel", 25565), false)
		if err == nil || !strings.Contains(err.Error(), "at most two subdomain levels") {
			t.Fatalf("err = %v, want the label-count error", err)
		}
	})

	t.Run("custom domain with too many labels rejected", func(t *testing.T) {
		h := newGatewayDomainHandler(map[string]string{"gateway_custom_domains_enabled": "true"})
		_, _, err := h.resolveRouteDomain(routeDomainReq("", "", "", "a.b.c.d.e", 25565), false)
		if err == nil || !strings.Contains(err.Error(), "at most two subdomain levels") {
			t.Fatalf("err = %v, want the label-count error", err)
		}
	})

	t.Run("custom domain rejected as a subdomain of a configured hoster domain", func(t *testing.T) {
		h := newGatewayDomainHandler(map[string]string{
			"gateway_hoster_domains":         hostersJSON(t),
			"gateway_custom_domains_enabled": "true",
		})
		_, _, err := h.resolveRouteDomain(routeDomainReq("", "", "", "test.dylaris.com", 25565), false)
		if err == nil || !strings.Contains(err.Error(), "may not be a subdomain of a hoster domain") {
			t.Fatalf("err = %v, want the hoster-suffix rejection", err)
		}
	})

	t.Run("raw domain from an ADMIN is accepted and lowercased", func(t *testing.T) {
		h := newGatewayDomainHandler(nil)
		// Not "MC.Example.com": "mc" is a default-reserved leftmost label, and the
		// list is lifted for an admin anyway, so that input would test nothing.
		got, isCustom, err := h.resolveRouteDomain(routeDomainReq("Survival.Example.com", "", "", "", 25565), true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "survival.example.com" {
			t.Fatalf("got %q, want survival.example.com", got)
		}
		if isCustom {
			t.Fatalf("isCustom = true; an admin's raw domain is the escape hatch, not a tenant claim to prove")
		}
	})

	t.Run("no domain provided at all is rejected", func(t *testing.T) {
		h := newGatewayDomainHandler(nil)
		_, _, err := h.resolveRouteDomain(routeDomainReq("", "", "", "", 25565), false)
		if err == nil {
			t.Fatalf("expected an error when no domain field is set")
		}
	})
}

// TestLoadBlockedRoutePrefixes pins the default-vs-explicit-empty semantics:
// an unset setting (raw == "") falls back to the protective default list; an
// explicitly-saved list - even an empty one - is honored as-is, including
// dropping the defaults entirely.
func TestLoadBlockedRoutePrefixes(t *testing.T) {
	t.Run("unset setting falls back to the default list", func(t *testing.T) {
		h := newGatewayDomainHandler(nil)
		got := h.loadBlockedRoutePrefixes()
		if !got["admin"] {
			t.Fatalf("blocked = %+v, want the default list (includes 'admin')", got)
		}
	})

	t.Run("explicit empty list overrides the default (nothing is blocked)", func(t *testing.T) {
		h := newGatewayDomainHandler(map[string]string{"gateway_blocked_route_prefixes": "[]"})
		got := h.loadBlockedRoutePrefixes()
		if len(got) != 0 {
			t.Fatalf("blocked = %+v, want empty (admin should no longer be reserved)", got)
		}
	})

	t.Run("explicit list is honored verbatim, lowercased and trimmed", func(t *testing.T) {
		h := newGatewayDomainHandler(map[string]string{"gateway_blocked_route_prefixes": `[" Custom ", "OTHER"]`})
		got := h.loadBlockedRoutePrefixes()
		if !got["custom"] || !got["other"] || got["admin"] {
			t.Fatalf("blocked = %+v, want exactly {custom, other} and NOT the default 'admin'", got)
		}
	})
}

// TestEffectiveRouteLimit pins the scope walk: "user:<id>" -> "user_default" ->
// "global", most specific first.
//
// The rule is now the same at every tier, which it was not before. A scope with
// NO ROW says nothing and the walk continues. A scope WITH a row has answered,
// and its answer stands - including a NULL ("no cap, decided") and a 0 ("none").
//
// The old version of this file pinned the opposite as "an intentional
// asymmetry": a 0 meant "disabled" at the user tier and "unset, try the next
// tier" at the other two. One stored number, two meanings depending on which row
// it sat in - and it left the tiers unable to express "no cap" at all, so a
// computed zero from the store was rewritten into "no override", fell through to
// a tier nobody had configured, and came out unlimited. Absence is spelled by the
// row not existing now, which is a thing the database can represent and a number
// cannot.
func TestEffectiveRouteLimit(t *testing.T) {
	const uid = "user-1"
	n := func(v int) *int { return &v }
	got := func(h *GatewayHandler) string {
		l := h.effectiveRouteLimit(uid)
		if l == nil {
			return "no cap"
		}
		return fmt.Sprintf("%d", *l)
	}

	cases := []struct {
		name   string
		limits map[string]*models.GatewayRouteLimit
		want   string
	}{
		{
			name: "a user row of zero means none, and beats the tiers below it",
			limits: map[string]*models.GatewayRouteLimit{
				"user:" + uid:  {MaxRoutes: n(0)},
				"user_default": {MaxRoutes: n(5)},
				"global":       {MaxRoutes: n(10)},
			},
			want: "0",
		},
		{
			name: "a user row with a number wins",
			limits: map[string]*models.GatewayRouteLimit{
				"user:" + uid: {MaxRoutes: n(7)},
				"global":      {MaxRoutes: n(10)},
			},
			want: "7",
		},
		{
			// The state the old int could not hold: this user is decided, and the
			// decision is "no cap". It keeps saying so if the platform default is
			// later tightened, which is what makes it different from having no row.
			name: "a user row with NULL is an explicit no-cap",
			limits: map[string]*models.GatewayRouteLimit{
				"user:" + uid:  {MaxRoutes: nil},
				"user_default": {MaxRoutes: n(5)},
			},
			want: "no cap",
		},
		{
			name: "no user row defers to user_default",
			limits: map[string]*models.GatewayRouteLimit{
				"user_default": {MaxRoutes: n(5)},
				"global":       {MaxRoutes: n(10)},
			},
			want: "5",
		},
		{
			// Changed deliberately. This used to fall through to global, because a
			// zero at this tier was read as "unset". A platform default of zero is
			// now a decision an operator can actually make and have honoured.
			name: "a user_default of zero is a real default of none",
			limits: map[string]*models.GatewayRouteLimit{
				"user_default": {MaxRoutes: n(0)},
				"global":       {MaxRoutes: n(3)},
			},
			want: "0",
		},
		{
			name:   "nothing configured anywhere is no cap",
			limits: map[string]*models.GatewayRouteLimit{},
			want:   "no cap",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newGatewayDomainHandlerWithLimits(c.limits)
			if g := got(h); g != c.want {
				t.Fatalf("effectiveRouteLimit() = %s, want %s", g, c.want)
			}
		})
	}
}

func newGatewayDomainHandlerWithLimits(limits map[string]*models.GatewayRouteLimit) *GatewayHandler {
	return &GatewayHandler{state: &AppState{Store: &gatewayDomainFakeStore{routeLimits: limits}}}
}

// --- ServerHandler.isSnapshotFetchHostAllowed (SSRF allowlist) ---

func newServerSnapshotHandler(settings map[string]string) *ServerHandler {
	return &ServerHandler{state: &AppState{Store: &gatewayDomainFakeStore{settings: settings}}}
}

// TestIsSnapshotFetchHostAllowed pins the SSRF host-allowlist gate
// (server_modpack_snapshot.go): cdn.modrinth.com or this Core's own pack mirror
// host, everything else refused.
func TestIsSnapshotFetchHostAllowed(t *testing.T) {
	localMirrorSettings := map[string]string{
		"modpack_storage_provider": "local",
		"core_public_url":          "https://core.example.com",
	}

	t.Run("modrinth cdn is allowed", func(t *testing.T) {
		h := newServerSnapshotHandler(localMirrorSettings)
		if !h.isSnapshotFetchHostAllowed("https://cdn.modrinth.com/data/abc/file.mrpack") {
			t.Fatalf("expected cdn.modrinth.com to be allowed")
		}
	})

	t.Run("this core's own mirror host is allowed", func(t *testing.T) {
		h := newServerSnapshotHandler(localMirrorSettings)
		if !h.isSnapshotFetchHostAllowed("https://core.example.com/mirror/modpacks/abc/pack.mrpack") {
			t.Fatalf("expected the configured core_public_url host to be allowed")
		}
	})

	// The Solder "public bucket" base no longer widens this list. A leftover
	// solder_mirror_url must not open a host a pack install never fetches from:
	// every panel-built pack is served from Core's own mirror now.
	t.Run("a leftover solder mirror url opens nothing", func(t *testing.T) {
		h := newServerSnapshotHandler(map[string]string{
			"modpack_storage_provider": "s3",
			"core_public_url":          "https://core.example.com",
			"solder_delivery_mode":     "public",
			"solder_mirror_url":        "https://cdn.myhoster.example/mirror",
		})
		if h.isSnapshotFetchHostAllowed("https://cdn.myhoster.example/mirror/pack.mrpack") {
			t.Fatalf("a Solder-only setting still widened the SSRF allowlist")
		}
	})

	t.Run("arbitrary host is refused", func(t *testing.T) {
		h := newServerSnapshotHandler(localMirrorSettings)
		if h.isSnapshotFetchHostAllowed("https://evil.example.com/data/abc/file.mrpack") {
			t.Fatalf("expected an arbitrary host to be refused")
		}
	})

	t.Run("garbage URL is refused", func(t *testing.T) {
		h := newServerSnapshotHandler(localMirrorSettings)
		if h.isSnapshotFetchHostAllowed("%zz") {
			t.Fatalf("expected an unparseable URL to be refused")
		}
	})

	// NOTE (real behavior, not a fix): isSnapshotFetchHostAllowed only ever
	// inspects u.Hostname() - it does not check u.Scheme at all. An
	// http:// URL to the same allowlisted host passes this gate exactly like
	// https://. This is not a standalone SSRF hole: the actual fetch goes
	// through services.SafeFetch, which independently rejects any scheme
	// other than "http"/"https" - so http is a deliberately still-allowed
	// scheme downstream, just not vetted a second time at this layer.
	t.Run("http scheme on an allowed host is NOT rejected by this gate (scheme is not checked here)", func(t *testing.T) {
		h := newServerSnapshotHandler(localMirrorSettings)
		if !h.isSnapshotFetchHostAllowed("http://cdn.modrinth.com/data/abc/file.mrpack") {
			t.Fatalf("expected http scheme on an allowed host to still pass this host-only gate")
		}
	})
}

// TestCnameLabelValidation pins that the custom-domain CNAME setting is a single
// DNS LABEL, not a full domain. It is prefixed onto every hoster base, so a
// value like "route.eu.example.com" would silently build
// "route.eu.example.com.eu.example.com" - a name that resolves nowhere and only
// surfaces as a customer whose CNAME never works. Save-side validation uses the
// same subRegexDNS the subdomain picker uses, so this pins the shared contract.
func TestCnameLabelValidation(t *testing.T) {
	valid := []string{"route", "connect", "mc", "r", "play-here", "a1"}
	for _, v := range valid {
		if !subRegexDNS.MatchString(v) {
			t.Errorf("label %q should be accepted as a CNAME label", v)
		}
	}

	invalid := []string{
		"route.eu.example.com", // the whole point: a full domain is rejected
		"route.eu",
		".route",
		"route.",
		"-route", // a DNS label may not start with a hyphen
		"route-", // ...nor end with one
		"ROUTE",  // callers lowercase first; the raw form must not slip through
		"ro ute",
		"route_1",
	}
	for _, v := range invalid {
		if subRegexDNS.MatchString(v) {
			t.Errorf("label %q should be rejected as a CNAME label", v)
		}
	}
}

// TestRouteIsReservedByDefault pins that "route" cannot be claimed as a user
// subdomain. It is the default CNAME label, so a user holding route.<base>
// would hijack the exact name every other user is told to point their own
// domain at.
func TestRouteIsReservedByDefault(t *testing.T) {
	h := newGatewayDomainHandler(nil)
	if !h.loadBlockedRoutePrefixes()["route"] {
		t.Fatal("'route' must be in the default reserved-prefix list")
	}
}

// The reserved list protects tenants from claiming names the platform speaks
// with. Admins are the exception it exists for: without the bypass nobody could
// ever register play.<base> or mc.<base>, including the operator whose server it
// is. Both entry shapes must agree, or the availability hint would say "reserved"
// on a name the create call then accepts.
func TestResolveRouteDomain_AdminMayUseReservedPrefixes(t *testing.T) {
	h := newGatewayDomainHandler(map[string]string{
		"gateway_hoster_domains":         `[{"domain":"dylaris.com","validation":"letters"}]`,
		"gateway_custom_domains_enabled": "true",
	})

	for _, tc := range []struct {
		name string
		req  *routeDomainReqT
		want string
	}{
		{"hoster subdomain", routeDomainReq("", "admin", "dylaris.com", "", 25565), "admin.dylaris.com"},
		{"custom domain", routeDomainReq("", "", "", "play.example.com", 25565), "play.example.com"},
		{"raw domain", routeDomainReq("mc.example.com", "", "", "", 25565), "mc.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := h.resolveRouteDomain(tc.req, false); err == nil {
				t.Fatal("a non-admin was allowed a reserved prefix")
			}
			got, _, err := h.resolveRouteDomain(tc.req, true)
			if err != nil {
				t.Fatalf("admin refused a reserved prefix: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The bypass lifts ONLY the reserved-name check. Everything else the resolver
// enforces is structural (format, hoster config, label count) and an admin
// hitting those has a broken request, not a privileged one.
func TestResolveRouteDomain_AdminBypassDoesNotLiftOtherRules(t *testing.T) {
	h := newGatewayDomainHandler(map[string]string{
		"gateway_hoster_domains": `[{"domain":"dylaris.com","validation":"letters"}]`,
	})
	cases := map[string]*routeDomainReqT{
		"unconfigured hoster": routeDomainReq("", "admin", "not-configured.com", "", 25565),
		"custom domains off":  routeDomainReq("", "", "", "admin.example.com", 25565),
		"too many labels":     routeDomainReq("", "", "", "a.b.c.d.e", 25565),
		"invalid raw domain":  routeDomainReq("not a domain", "", "", "", 25565),
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := h.resolveRouteDomain(req, true); err == nil {
				t.Error("admin bypass swallowed a structural validation error")
			}
		})
	}
}

// The names a hoster needs for its own flagship server are the ones tenants want
// most. They are worth nothing reserved if the operator cannot use them either,
// so this pins both halves: blocked by default, available to an admin.
func TestDefaultBlockedPrefixesReserveTheHostersOwnNames(t *testing.T) {
	h := newGatewayDomainHandler(nil)
	blocked := h.loadBlockedRoutePrefixes()
	for _, name := range []string{"play", "mc", "minecraft", "admin", "dylaris", "store"} {
		if !blocked[name] {
			t.Errorf("%q is not reserved by default — a tenant could claim the platform's own address", name)
		}
	}
}

// TestRawDomainFromATenantIsNotAThirdPath pins what the raw `domain` field is
// worth in a tenant's hands. It used to be worth everything: whatever FQDN it
// carried was registered, so moving the same string out of `customDomain` and
// into `domain` skipped the custom-domain switch, the ownership proof, the
// picker's format rules and - because the address allowance only counts the
// configured hoster domains - the allowance too.
//
// Measured on production before the fix: an ordinary account registered
// r7-squat.example.com with custom domains switched OFF platform-wide, and
// r7probe.dylaris.com on our own apex, which the picker would have refused.
// First registration wins, so that is a squatting lever.
//
// The rule now is that a tenant's raw domain is normalised into whichever of
// the two real paths it belongs to, and answers to that path's rules.
func TestRawDomainFromATenantIsNotAThirdPath(t *testing.T) {
	hosters := func(t *testing.T) string {
		return hosterDomainsJSON(t, []HosterDomain{{Domain: "eu.dylaris.com", Validation: "letters"}})
	}

	t.Run("foreign domain is refused while custom domains are off", func(t *testing.T) {
		h := newGatewayDomainHandler(map[string]string{
			"gateway_hoster_domains":         hosters(t),
			"gateway_custom_domains_enabled": "false",
		})
		_, _, err := h.resolveRouteDomain(routeDomainReq("squat.example.com", "", "", "", 25565), false)
		if err == nil || !strings.Contains(err.Error(), "custom domains are not enabled") {
			t.Fatalf("err = %v, want the same refusal the customDomain field gets", err)
		}
	})

	t.Run("foreign domain still has to prove ownership when custom domains are on", func(t *testing.T) {
		h := newGatewayDomainHandler(map[string]string{
			"gateway_hoster_domains":         hosters(t),
			"gateway_custom_domains_enabled": "true",
		})
		got, isCustom, err := h.resolveRouteDomain(routeDomainReq("squat.example.com", "", "", "", 25565), false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "squat.example.com" {
			t.Fatalf("got %q, want squat.example.com", got)
		}
		// The whole point of the second return value: the caller arms the
		// ownership claim from it, and reading the request FIELD instead is what
		// let this domain through unproven.
		if !isCustom {
			t.Fatal("isCustom = false, so the ownership gate and the claim would both be skipped")
		}
	})

	t.Run("a name under a hoster domain answers to the picker's format rules", func(t *testing.T) {
		h := newGatewayDomainHandler(map[string]string{"gateway_hoster_domains": hosters(t)})
		// "letters" validation: digits are not a name the picker would accept.
		if _, _, err := h.resolveRouteDomain(routeDomainReq("r7probe123.eu.dylaris.com", "", "", "", 25565), false); err == nil {
			t.Fatal("a raw FQDN skipped the hoster format rule the picker enforces")
		}
		got, isCustom, err := h.resolveRouteDomain(routeDomainReq("survival.eu.dylaris.com", "", "", "", 25565), false)
		if err != nil {
			t.Fatalf("a well-formed name under a hoster domain was refused: %v", err)
		}
		if got != "survival.eu.dylaris.com" {
			t.Fatalf("got %q, want survival.eu.dylaris.com", got)
		}
		if isCustom {
			t.Fatal("isCustom = true for one of OUR domains; it would be sent off to prove ownership of ours")
		}
	})

	t.Run("a reserved name under a hoster domain is refused", func(t *testing.T) {
		h := newGatewayDomainHandler(map[string]string{"gateway_hoster_domains": hosters(t)})
		_, _, err := h.resolveRouteDomain(routeDomainReq("panel.eu.dylaris.com", "", "", "", 25565), false)
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("err = %v, want a 'reserved' refusal", err)
		}
	})

	t.Run("the hoster apex itself is not a tenant's to route", func(t *testing.T) {
		h := newGatewayDomainHandler(map[string]string{"gateway_hoster_domains": hosters(t)})
		if _, _, err := h.resolveRouteDomain(routeDomainReq("eu.dylaris.com", "", "", "", 25565), false); err == nil {
			t.Fatal("a tenant was allowed to register the hoster domain itself")
		}
	})

	t.Run("an admin keeps the raw escape hatch", func(t *testing.T) {
		h := newGatewayDomainHandler(map[string]string{
			"gateway_hoster_domains":         hosters(t),
			"gateway_custom_domains_enabled": "false",
		})
		got, isCustom, err := h.resolveRouteDomain(routeDomainReq("anything.example.com", "", "", "", 25565), true)
		if err != nil || got != "anything.example.com" || isCustom {
			t.Fatalf("got %q, isCustom %v, err %v; an admin's raw domain must pass through unchanged", got, isCustom, err)
		}
	})

	t.Run("an unconfigured platform keeps the raw path, reserved names aside", func(t *testing.T) {
		// No hoster domains and custom domains off: the operator has said
		// nothing about what a tenant may claim, and the panel's picker is in
		// its "legacy" mode. Closing the raw path here would leave those
		// tenants unable to create any address at all.
		h := newGatewayDomainHandler(nil)
		got, isCustom, err := h.resolveRouteDomain(routeDomainReq("survival.example.com", "", "", "", 25565), false)
		if err != nil || got != "survival.example.com" || isCustom {
			t.Fatalf("got %q, isCustom %v, err %v; want the legacy fallback intact", got, isCustom, err)
		}
		if _, _, err := h.resolveRouteDomain(routeDomainReq("panel.example.com", "", "", "", 25565), false); err == nil {
			t.Fatal("the reserved list must still hold on an unconfigured platform")
		}
	})

	t.Run("one configured hoster domain is enough to close it", func(t *testing.T) {
		h := newGatewayDomainHandler(map[string]string{"gateway_hoster_domains": hosters(t)})
		if _, _, err := h.resolveRouteDomain(routeDomainReq("survival.example.com", "", "", "", 25565), false); err == nil {
			t.Fatal("a configured platform still accepted an unproven foreign domain")
		}
	})
}
