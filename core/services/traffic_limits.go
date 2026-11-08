package services

import (
	"dylaris-core/models"
)

// TrafficScopeDefault is the one platform-wide scope: what every tenant gets
// unless a per-user override says otherwise.
//
// There used to be a third scope, "global", asked after this one. For traffic it
// answered nothing the default could not: unlike gateway routes, where "global"
// covers the platform's OWN routes, every byte counted here belongs to a tenant.
// Two settings doing one job is a screen where an operator has to decide which
// of two identical-looking fields to fill in, and a value that stops working the
// day somebody fills in the other. The scope is gone and its rows were migrated.
const TrafficScopeDefault = "user_default"

// TrafficRegionAny is the region a NON-regional kind stores its limit under.
//
// Player traffic is measured where the edge that served it stands, so it is
// capped per region: a terabyte out of Singapore costs 7.4x one out of
// Nuremberg. File transfers are not like that - every beam relay is in
// eu-central - so a per-region cap for them would be a screen full of regions
// asking the same question with only one possible answer, plus one more place
// for the real answer to be missing.
const TrafficRegionAny = "*"

// RegionalKind reports whether a traffic kind is capped per region.
//
// Deliberately a function over an explicit list rather than a column: adding a
// kind should force this decision to be made, not default to whichever behaviour
// the zero value happens to give.
func RegionalKind(kind string) bool { return kind == TrafficKindEdge }

// Traffic kinds as the producers write them.
const (
	TrafficKindEdge = "edge"
	// TrafficKindRelay is beam file transfer, carried by a relay. Not regional:
	// every relay is in eu-central.
	TrafficKindRelay = "relay"
)

// There is deliberately NO backup traffic kind.
//
// Backups go from the node straight to object storage, and R2 charges nothing
// for ingress - so on a BYON node the bytes are the customer's own bandwidth and
// cost us nothing in either direction. A pool that capped and billed them would
// be metering a resource we do not pay for. What backups actually cost is
// STORAGE, which is r2_quota_gb and is counted and enforced separately.

// TrafficLimitRegion maps a (region, kind) question onto the region its limit is
// actually stored under. Non-regional kinds all resolve to TrafficRegionAny, so
// usage recorded in any region is judged against the one global row.
func TrafficLimitRegion(region, kind string) string {
	if RegionalKind(kind) {
		return region
	}
	return TrafficRegionAny
}

// TrafficLimitScopes is the order a (region, kind) question is asked in, most
// specific first. Exported because the panel shows which scope answered, and a
// screen that guessed the order would eventually disagree with the resolver.
func TrafficLimitScopes(userID string) []string {
	return []string{"user:" + userID, TrafficScopeDefault}
}

// TrafficLimitReader is the slice of the store this resolver needs. Narrow on
// purpose: it makes the walk testable without a database, and the walk is the
// part worth testing.
type TrafficLimitReader interface {
	GetTrafficLimit(scope, region, kind string) (*models.TrafficLimit, error)
}

// ResolvedTrafficLimit is one answer: how much is included, how much may be
// bought, and which scope said so.
//
// Both numbers follow the platform-wide convention (CLAUDE.md, "Limits"): nil
// is no limit at all, 0 is none, n is the cap. Read them through Exceeds /
// AtOrOver rather than comparing by hand, so no call site can forget the nil
// check and so a 0 compares like any other number.
type ResolvedTrafficLimit struct {
	IncludedGB *int64
	// MaxPurchaseGB is carried, not enforced here. The stop lives in the store
	// (billing.RegionCeilings), because stopping is a money decision and the
	// store is what knows whether the tenant agreed to be billed at all. Core
	// hands the number over through /store/usage.
	MaxPurchaseGB *int64
	// Scope that answered, "" when nothing did. "" therefore means no limit of
	// any kind for this (region, kind) - which is a real state and the reason
	// the panel needs to show unconfigured regions rather than hide them.
	Scope string
}

// ResolveTrafficLimit walks the scopes most-specific-first and returns the
// first ROW that exists.
//
// The row is the unit of answering, not the column. A scope holding a row has
// answered both questions, including with NULL - so a per-user override cannot
// set an allowance and silently inherit a purchase cap from a scope the
// operator was not looking at. That split answer is the shape that produced
// four separate defects on this platform before limits were made pointers.
//
// No row anywhere means no limit, which matches every other limit here. It is
// permissive by default and that is deliberate: an operator who has configured
// nothing has not yet decided, and refusing traffic on an undecided platform
// would take servers down over a settings screen nobody opened. The panel is
// where that gap has to be visible.
func ResolveTrafficLimit(r TrafficLimitReader, userID, region, kind string) (ResolvedTrafficLimit, error) {
	region = TrafficLimitRegion(region, kind)
	for _, scope := range TrafficLimitScopes(userID) {
		l, err := r.GetTrafficLimit(scope, region, kind)
		if err != nil {
			return ResolvedTrafficLimit{}, err
		}
		// A user's own "*" row for a regional kind covers every region that user
		// has no exact row for. The store writes it for a product that includes
		// the same allowance in each region, and the store cannot know the region
		// labels the edges emit. Only the user scope reads it: the operator's
		// user_default rows stay exact, so no row they set before this answers
		// for a new region by surprise.
		if l == nil && region != TrafficRegionAny && scope != TrafficScopeDefault {
			if l, err = r.GetTrafficLimit(scope, TrafficRegionAny, kind); err != nil {
				return ResolvedTrafficLimit{}, err
			}
		}
		if l == nil {
			continue // this scope says nothing: ask the next one
		}
		return ResolvedTrafficLimit{
			IncludedGB:    l.IncludedGB,
			MaxPurchaseGB: l.MaxPurchaseGB,
			Scope:         scope,
		}, nil
	}
	return ResolvedTrafficLimit{}, nil
}
