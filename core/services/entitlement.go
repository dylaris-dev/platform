package services

import (
	"strings"
	"time"

	"dylaris-core/store"
)

// Entitlement kinds. A plan or a grant carries one of these; "" means none.
const (
	EntitlementByon      = "byon"
	EntitlementRouteOnly = "route_only"
	EntitlementBoth      = "both"
)

// Where an entitlement came from. Reported so the UI can say "from your
// subscription" vs "granted by an admin until <date>" instead of just yes/no.
const (
	EntitlementSourceNone      = "none"
	EntitlementSourcePlan      = "plan"
	EntitlementSourceGrant     = "grant"
	EntitlementSourceBoth      = "plan+grant"
	EntitlementSourceUnlimited = "unlimited"
	EntitlementSourceSuspended = "suspended"
)

// Entitlement answers "what may this tenant use", separately from "how much"
// (that is Limits).
type Entitlement struct {
	// Byon: may enroll their own nodes and run servers on them.
	Byon bool `json:"byon"`
	// RouteOnly: may create routes / link kits without owning a node.
	RouteOnly bool `json:"routeOnly"`
	// Source explains the answer; see the EntitlementSource* constants.
	Source string `json:"source"`
	// PlanKind is the kind the resolved plan carries ("" when there is no plan).
	PlanKind string `json:"planKind,omitempty"`
	// GrantKind + GrantExpiresAt describe an ACTIVE manual grant only. An expired
	// grant is reported as no grant at all, so a stale row can never read as one.
	GrantKind      string     `json:"grantKind,omitempty"`
	GrantExpiresAt *time.Time `json:"grantExpiresAt,omitempty"`
	// Per-kind deadlines for the two grants, so an admin screen can show and
	// end them one at a time. GrantExpiresAt above stays the LATER of the two
	// and remains the single date to render where only one fits.
	GrantByonExpiresAt  *time.Time `json:"grantByonExpiresAt,omitempty"`
	GrantRouteExpiresAt *time.Time `json:"grantRouteExpiresAt,omitempty"`
}

// entitlementStore is the narrow store surface EffectiveEntitlement needs. It is
// one lookup now: entitlement is what the store pushed onto the billing row plus
// any manual grant, and plans no longer take part.
type entitlementStore interface {
	GetUserBilling(userID string) (*store.UserBilling, error)
}

// kindGrants expands a kind into the two booleans. An unknown kind grants
// nothing rather than everything: a typo in a plan row must not widen access.
func kindGrants(kind string) (byon, routeOnly bool) {
	switch strings.TrimSpace(strings.ToLower(kind)) {
	case EntitlementBoth:
		return true, true
	case EntitlementByon:
		return true, false
	case EntitlementRouteOnly:
		return false, true
	default:
		return false, false
	}
}

// purchasedKind names what the pushed overrides amount to, in the same
// vocabulary a manual grant uses, so the panel renders one thing rather than two.
func purchasedKind(byon, routeOnly bool) string {
	switch {
	case byon && routeOnly:
		return EntitlementBoth
	case byon:
		return EntitlementByon
	case routeOnly:
		return EntitlementRouteOnly
	}
	return ""
}

// EffectiveEntitlement resolves what a tenant may use, at time `now`.
//
// The rules, in order:
//
//  0. An administrator is not a customer and is entitled to everything.
//  1. A suspended tenant gets nothing, whatever they hold. Suspension is a hard
//     stop; an admin who wants to restore access sets the status back to active.
//     (past_due is NOT a stop - that is what the grace window is for.)
//  2. An ACTIVE manual grant contributes its kind. Active means a kind is set and
//     the expiry is in the future; an expired row contributes nothing.
//  3. What the STORE pushed contributes. A purchase arrives as per-user limit
//     overrides (maxNodes / maxLinks), so a positive one is the record of having
//     bought that thing.
//  4. Only with NO store integration at all does "nothing configured" mean
//     unlimited. That is the self-host case, where there is no billing plane and
//     denying would lock out every install that never defined anything.
//
// storeEnabled is what separates 3 from 4, and it is the whole point of this
// function's shape. Before it, "no plan anywhere" meant unlimited unconditionally
// - which on a hosted install handed every freshly registered account the run of
// the platform, because an account that has bought nothing has nothing
// configured. The one state meaning "paid for nothing" was read as "entitled to
// everything".
//
// The contributions are a UNION, deliberately: "give them 14 days now, they can
// subscribe later" must not have the subscription and the grant fight.
//
// Callers must gate this behind feature_byon_enabled; with BYON off none of it
// is meaningful.
func EffectiveEntitlement(st entitlementStore, userID string, now time.Time, storeEnabled, isAdmin bool) (Entitlement, error) {
	// 0. An administrator is not a customer. They run the platform, and the store
	// has nothing to sell them. Without this the owner of a hosted install cannot mint an enroll
	// token or a warp key for their own machine without first selling themselves
	// a subscription, and the over-limit sweep would eventually stop anything
	// they had enrolled under their own account. Checked before the store is
	// even read, because none of it applies to them.
	if isAdmin {
		return Entitlement{Byon: true, RouteOnly: true, Source: EntitlementSourceUnlimited}, nil
	}

	billing, err := st.GetUserBilling(userID)
	if err != nil {
		return Entitlement{}, err
	}
	if billing != nil && billing.Status == "suspended" {
		return Entitlement{Source: EntitlementSourceSuspended}, nil
	}

	out := Entitlement{Source: EntitlementSourceNone}

	// Manual grant. The two kinds carry their OWN expiry, so an admin can hold
	// both with different deadlines - granting one used to replace the other.
	//
	// The legacy single string + single expiry is still read, but only for a row
	// that predates the split and has not been touched since: the migration
	// backfills the per-kind columns, so anything written after it answers here.
	// Keeping the fallback means a Core that rolls back mid-upgrade does not read
	// every existing grant as revoked.
	grantByon, grantRoute := false, false
	if billing != nil {
		if exp := billing.ManualByonExpiresAt; exp != nil && exp.After(now) {
			grantByon = true
			out.GrantExpiresAt = exp
			out.GrantByonExpiresAt = exp
		}
		if exp := billing.ManualRouteExpiresAt; exp != nil && exp.After(now) {
			grantRoute = true
			out.GrantRouteExpiresAt = exp
			// The LATER of the two is what the summary line reports, so it says
			// when the tenant stops being entitled to anything rather than when
			// the first half lapses.
			if out.GrantExpiresAt == nil || exp.After(*out.GrantExpiresAt) {
				out.GrantExpiresAt = exp
			}
		}
		if !grantByon && !grantRoute && strings.TrimSpace(billing.ManualEntitlement) != "" &&
			billing.ManualByonExpiresAt == nil && billing.ManualRouteExpiresAt == nil {
			if exp := billing.ManualEntitlementExpiresAt; exp != nil && exp.After(now) {
				grantByon, grantRoute = kindGrants(billing.ManualEntitlement)
				out.GrantExpiresAt = exp
			}
		}
		out.GrantKind = purchasedKind(grantByon, grantRoute)
	}

	// What was bought. The store pushes a purchase as per-user limit overrides,
	// so a positive maxNodes is the record of a BYON purchase and a positive
	// maxLinks the record of a route-only one. Nothing else in Core knows a
	// subscription exists.
	//
	// nil and 0 are read differently on purpose. nil is "the store never said
	// anything about this account". 0 is "the store said zero", which is what a
	// cancellation pushes, and it must not read as unlimited the way a bare
	// numeric cap check does.
	planByon, planRoute := false, false
	if billing != nil {
		if billing.MaxNodes != nil && *billing.MaxNodes > 0 {
			planByon = true
		}
		if billing.MaxLinks != nil && *billing.MaxLinks > 0 {
			planRoute = true
		}
	}
	if planByon || planRoute {
		out.PlanKind = purchasedKind(planByon, planRoute)
	}

	// Self-host: no store, no billing plane, nothing to buy. Everything is
	// allowed, which is what this platform has always done for an install that
	// configured none of this.
	if !storeEnabled {
		planByon, planRoute = true, true
		out.Source = EntitlementSourceUnlimited
		out.PlanKind = ""
	}

	out.Byon = grantByon || planByon
	out.RouteOnly = grantRoute || planRoute

	// Name the source(s) that actually contributed.
	hasGrant := grantByon || grantRoute
	hasPlan := planByon || planRoute
	switch {
	case out.Source == EntitlementSourceUnlimited && !hasGrant:
		// keep "unlimited"
	case hasGrant && hasPlan && out.Source != EntitlementSourceUnlimited:
		out.Source = EntitlementSourceBoth
	case hasGrant && out.Source == EntitlementSourceUnlimited:
		out.Source = EntitlementSourceBoth
	case hasGrant:
		out.Source = EntitlementSourceGrant
	case hasPlan:
		out.Source = EntitlementSourcePlan
	default:
		out.Source = EntitlementSourceNone
	}
	return out, nil
}
