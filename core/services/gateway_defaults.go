package services

import (
	"database/sql"
	"errors"
	"fmt"

	"dylaris-core/models"
	"dylaris-core/store"
)

// HosterDomain is one of the platform-provided base domains under which a
// user can register a route by entering only a subdomain. The validation
// mode controls what characters are accepted in that subdomain field.
type HosterDomain struct {
	Domain     string `json:"domain"`     // e.g. "dylaris.com"
	Validation string `json:"validation"` // "letters" | "alphanumeric" | "dns"
}

// SettingMaxCustomDomains caps the routes one account may hold on its own
// domains. Those cost us no address, but each one is a claim the verifier
// queries a tenant-chosen nameserver for and a key every edge loads, and there
// was no bound on them at all.
const SettingMaxCustomDomains = "gateway_max_custom_domains"

var DefaultMaxCustomDomains = LimitPtr(50)

// CustomDomainCapReader is the slice of the store CustomDomainCapFor reads.
type CustomDomainCapReader interface {
	GetGatewayCustomDomainLimit(scope string) (*models.GatewayCustomDomainLimit, error)
	GetSetting(key string) (string, error)
}

// CustomDomainCapFor resolves the cap on routes on an account's own domains:
// admin:<id> -> user:<id> -> user_default -> global in
// gateway_custom_domain_limits, the first ROW answering, NULL included (no cap
// for this account). When no scope has a row the platform setting answers.
// scoped reports that a row did, which the claim cap needs. One resolver for
// the create paths and the over-limit sweep, so they cannot disagree.
func CustomDomainCapFor(st CustomDomainCapReader, userID string) (limit *int64, scoped bool) {
	limit, scoped, _ = customDomainCap(st, userID, false)
	return limit, scoped
}

// CustomDomainCapForSweep is CustomDomainCapFor for the over-limit sweep, which
// must not read a failed lookup as "no row" or the default: that could put a
// tenant over a cap nobody set. Only sql.ErrNoRows means "this scope says
// nothing"; any other error is returned and the sweep skips the tenant.
func CustomDomainCapForSweep(st CustomDomainCapReader, userID string) (*int64, error) {
	limit, _, err := customDomainCap(st, userID, true)
	return limit, err
}

func customDomainCap(st CustomDomainCapReader, userID string, strict bool) (*int64, bool, error) {
	for _, scope := range []string{"admin:" + userID, "user:" + userID, "user_default", "global"} {
		l, err := st.GetGatewayCustomDomainLimit(scope)
		if strict && err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, false, fmt.Errorf("custom-domain limit %s: %w", scope, err)
		}
		if err != nil || l == nil {
			continue // no row here: ask the next scope
		}
		return l.MaxCustomDomains, true, nil
	}
	raw, err := st.GetSetting(SettingMaxCustomDomains)
	// A NULL value is "never saved" (the default), like a missing row.
	if strict && err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, store.ErrSettingNull) {
		return nil, false, fmt.Errorf("setting %s: %w", SettingMaxCustomDomains, err)
	}
	return ParseLimitSetting(raw, DefaultMaxCustomDomains), false, nil
}

// DefaultBlockedRoutePrefixes seeds a protective reserved list when the admin has
// never saved one. Once saved (even empty), the admin's list wins.
// "route" is reserved because it is the default custom-domain CNAME label: a
// user who claimed route.<base> would take over the very name other users are
// told to point their own domains at.
// The second group is the set a hoster wants for its OWN flagship server and a
// tenant must not be able to take first: "play.<base>" and "mc.<base>" read as
// the platform's official address, not as one customer's. Admins are exempt
// (resolveRouteDomain's allowReserved), so reserving them costs the operator
// nothing and keeps them available.
var DefaultBlockedRoutePrefixes = []string{
	"admin", "dylaris", "app", "api", "www", "panel", "gateway", "edge", "hub",
	"link", "warp", "beam", "mail", "ns", "status", "support", "staff", "system", "root",
	"route",
	"minecraft", "mc", "play", "server", "store", "shop", "billing", "account",
	"login", "auth", "cdn", "docs", "help",
}

// DefaultWarpSpokeAllowedPorts is the compiled-in default allowlist (6379 Redis,
// 25501 Core gRPC, 25551 beam relay). Written in the sorted order
// normalizeWarpPorts emits, so the first-boot value does not visibly reorder on
// the first save. MUST match gateway/warp defaultSpokeAllowedPorts.
//
// 25560, the edge tunnel, is deliberately absent. A customer machine must reach
// an edge over the internet: through the overlay its players share one warp
// leader's bandwidth with that same customer's Redis traffic and Beam uploads,
// and drop every session whenever the leader restarts. This allowlist is the
// only place that can enforce it - LINK_EXTERNAL, NODE_EXTERNAL and NODE_TAGS
// are all env on the customer's own host, so nothing the machine declares about
// itself is evidence.
//
// Adding it back re-opens that path for every tenant at once. The edge must
// publish :25560 instead; a link then fails outright rather than quietly
// degrading onto the overlay and looking healthy.
const DefaultWarpSpokeAllowedPorts = "6379,25501,25551"
