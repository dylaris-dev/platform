package services

import (
	"context"
	"fmt"
	"log"
	"time"

	"dylaris-core/mailer"
)

// overLimitGrace is how long a tenant keeps everything running after they are
// first seen holding more than they bought. Fixed rather than configurable: it
// is a fairness window, not a knob, and the platform-wide payment grace is a
// different clock for a different problem.
// Exported so the panel can be told the same deadline the sweep enforces,
// rather than duplicating the number and drifting from it.
const OverLimitGrace = 72 * time.Hour

// tenantUsage is what a tenant currently HOLDS, against what they may hold.
// Counted fresh each pass rather than tracked, because every one of these can
// change without going through billing (a node is deleted, a kit is revoked).
type tenantUsage struct {
	// The *Limit fields follow the platform convention: nil is no cap at all,
	// 0 is a real "none", n is the cap. See services.Limits.
	nodes      int64
	nodeLimit  *int64
	links      int64
	linkLimit  *int64
	routes     int64
	routeLimit *int64
	// entitledByon / entitledRoute are whether the tenant may hold each KIND.
	// Separate from the caps because a cap cannot express it, and separate from
	// each other because the two doors ask separately: see over().
	entitledByon  bool
	entitledRoute bool
}

// entitledToAnything is the combined question, and it is the right one for
// ADDRESSES only. Addresses come out of one pool that either product feeds, and
// route creation gates on the allowance rather than on a kind - so a BYON tenant
// holding addresses is entitled to them. Nodes and route-only locations belong
// to one kind each and are asked about individually.
func (u tenantUsage) entitledToAnything() bool {
	return u.entitledByon || u.entitledRoute
}

func (u tenantUsage) holdsAnything() bool {
	return u.nodes > 0 || u.links > 0 || u.routes > 0
}

// over reports whether a tenant holds more than they are due.
//
// Two questions, and the caps can only answer one of them. A cap of 0 means "no
// cap", which is right for self-host and for an unmetered tenant - but 0 is also
// exactly what a cancellation pushes, so reading the caps alone made a tenant
// who cancelled EVERYTHING permanently within their limits. The entitlement gate
// stopped them creating anything new; nothing ever took away what they had.
// Downgrading from five nodes to one was enforced and downgrading to none was
// not, which is the wrong way round.
//
// Asked PER KIND, because the doors are. handlers.requireEntitlement resolves
// EntitlementByon for a node and EntitlementRouteOnly for a link kit, but this
// swept on whether the tenant was entitled to EITHER - so a tenant whose BYON
// grant lapsed while they paid for route-only kept running the granted machine
// indefinitely. They could not mint another (that door asks the right question)
// and nothing ever took away the one they had. An expired grant leaves the node
// CAP at nil, which is "no cap", so the numeric arms could not catch it either.
//
// Entitlement is resolved from services.EffectiveEntitlement, so both are true
// for every self-host install (no store, no billing plane, nothing to buy) and
// these clauses never fire there.
func (u tenantUsage) over() bool {
	if u.nodes > 0 && !u.entitledByon {
		return true
	}
	if u.links > 0 && !u.entitledRoute {
		return true
	}
	if u.routes > 0 && !u.entitledToAnything() {
		return true
	}
	return Exceeds(u.nodeLimit, u.nodes) ||
		Exceeds(u.linkLimit, u.links) ||
		Exceeds(u.routeLimit, u.routes)
}

// describe renders the dimensions that are actually over, for the email and the
// log. Naming only what is wrong keeps a one-node overage from reading like a
// total failure.
func (u tenantUsage) describe() string {
	if !u.entitledToAnything() && u.holdsAnything() {
		// No plan at all is one fact, not three dimensions. Listing "3 nodes
		// (allowed 0)" would read as a cap that was lowered, when what happened
		// is that the subscription ended.
		return fmt.Sprintf("%d nodes, %d route-only locations and %d protected addresses with no active plan",
			u.nodes, u.links, u.routes)
	}
	var parts []string
	// A kind whose entitlement lapsed while another one is still paid. Named as
	// what it is rather than as a cap, because there IS no cap to quote: an
	// expired grant leaves the ceiling at nil, and "allowed <nil>" is not a
	// sentence anyone can act on.
	if u.nodes > 0 && !u.entitledByon {
		parts = append(parts, fmt.Sprintf("%d nodes with no active bring-your-own-node plan", u.nodes))
	}
	if u.links > 0 && !u.entitledRoute {
		parts = append(parts, fmt.Sprintf("%d route-only locations with no active route-only plan", u.links))
	}
	if u.entitledByon && Exceeds(u.nodeLimit, u.nodes) {
		parts = append(parts, fmt.Sprintf("%d nodes (allowed %d)", u.nodes, *u.nodeLimit))
	}
	if u.entitledRoute && Exceeds(u.linkLimit, u.links) {
		parts = append(parts, fmt.Sprintf("%d route-only locations (allowed %d)", u.links, *u.linkLimit))
	}
	if Exceeds(u.routeLimit, u.routes) {
		parts = append(parts, fmt.Sprintf("%d protected addresses (allowed %d)", u.routes, *u.routeLimit))
	}
	out := ""
	for i, p := range parts {
		switch {
		case i == 0:
			out = p
		case i == len(parts)-1:
			out += " and " + p
		default:
			out += ", " + p
		}
	}
	return out
}

// tenantUsageFor counts what one tenant holds against their effective caps.
//
// Node usage counts every UNREDEEMED node identity - enroll tokens and warp keys
// alike - alongside live nodes, through the same services.NodeSlotsUsed the two
// mint gates enforce. A minted identity is a machine that has not connected yet,
// and ignoring it would let a downgraded tenant sit under the cap on paper while
// three boxes are mid-setup.
func (s *BillingLifecycleService) tenantUsageFor(ctx context.Context, userID string) (tenantUsage, error) {
	var u tenantUsage

	lim, err := EffectiveLimits(s.store, userID)
	if err != nil {
		return u, err
	}
	u.nodeLimit, u.linkLimit = lim.MaxNodes, lim.MaxLinks

	// May they hold each KIND of this, which the caps above cannot express: an
	// account that bought nothing carries the same zeroes as one that bought
	// unlimited. See over(). This is the same resolver the create paths gate on,
	// and it is now read the same WAY they read it - per kind - so the sweep and
	// the doors cannot disagree about who is entitled to what.
	//
	// The administrator flag is the SUBJECT's, read from their row - an operator
	// running the platform is not a customer of it and must never be swept off
	// their own machines.
	subjectIsAdmin := false
	if usr, uerr := s.store.GetUserByID(userID); uerr == nil && usr != nil {
		subjectIsAdmin = usr.IsAdmin
	}
	ent, err := EffectiveEntitlement(s.store, userID, time.Now(), s.storeEnabled, subjectIsAdmin)
	if err != nil {
		return u, err
	}
	u.entitledByon, u.entitledRoute = ent.Byon, ent.RouteOnly

	// The SAME count the two mint gates enforce. It has to be: this sweep cuts a
	// tenant off for holding more than they bought, so counting a kind of pending
	// identity the doors do not count (or missing one they do) means punishing a
	// state the platform itself handed out. See services.NodeSlotsUsed.
	u.nodes, err = NodeSlotsUsed(s.store, userID)
	if err != nil {
		return u, err
	}

	kits, err := s.store.CountLinkKitsByOwner(userID)
	if err != nil {
		return u, err
	}
	u.links = int64(kits)

	// The address pool lives in gateway_route_limits under the per-user scope,
	// which is what a purchase writes. Only that scope is read here: the
	// user_default and global fallbacks are the PLATFORM's baseline, and cutting
	// a tenant off for exceeding a number nobody sold them would be wrong.
	if l, lerr := s.store.GetGatewayRouteLimit("user:" + userID); lerr == nil && l != nil && l.MaxRoutes != nil {
		n := int64(*l.MaxRoutes)
		u.routeLimit = &n
	}
	// Routes live in Redis, not the database - they are gateway data plane state.
	// With no Redis there is no route plane, so there is nothing to be over.
	//
	// Only addresses on OUR domains count, exactly as the two create paths count
	// them. If this counted custom domains too, a tenant could create a route the
	// panel allowed and then be cut off by this sweep for holding it - the guard
	// one door takes and its sibling does not.
	if s.redis != nil {
		raw, _ := s.store.GetSetting(HosterDomainsSettingKey)
		bases := HosterBaseDomains(raw)
		for _, rt := range GetRoutesFromRedis(ctx, s.redis) {
			if rt.OwnerID == userID && DomainIsOurs(rt.Domain, bases) {
				u.routes++
			}
		}
	}

	return u, nil
}

// enforceEntitlementLimits is the over-limit half of the lifecycle: a tenant who
// holds more than they bought gets a fixed grace window with a warning, and is
// cut off in full if they are still over when it ends.
//
// Cutting EVERYTHING rather than the excess is deliberate. Picking which three
// of five nodes to kill is a judgement the platform has no business making, and
// any rule for it (oldest, emptiest, last added) is wrong for someone. The
// tenant chooses instead, by getting back under their own limit; if they do not
// choose, nothing runs. Same cutoff the payment path uses, so a tenant who is
// both over-limit and unpaid is not stopped twice.
func (s *BillingLifecycleService) enforceEntitlementLimits(ctx context.Context) {
	rows, err := s.store.ListUserBilling()
	if err != nil {
		log.Printf("billing over-limit: list user_billing: %v", err)
		return
	}
	now := time.Now()

	for _, b := range rows {
		// A suspended tenant is already stopped by the payment path. Marking them
		// over-limit on top would send a second warning about a second deadline
		// for services that are not running.
		if b.Status == "suspended" {
			continue
		}

		u, err := s.tenantUsageFor(ctx, b.UserID)
		if err != nil {
			log.Printf("billing over-limit: usage for %s: %v", b.UserID, err)
			continue
		}

		if !u.over() {
			if b.OverLimitSince != nil {
				if err := s.store.SetUserOverLimitSince(b.UserID, nil); err != nil {
					log.Printf("billing over-limit: clear %s: %v", b.UserID, err)
					continue
				}
				log.Printf("billing over-limit: %s is back within its limits", b.UserID)
			}
			continue
		}

		if b.OverLimitSince == nil {
			at := now
			if err := s.store.SetUserOverLimitSince(b.UserID, &at); err != nil {
				log.Printf("billing over-limit: stamp %s: %v", b.UserID, err)
				continue
			}
			deadline := at.Add(OverLimitGrace)
			log.Printf("billing over-limit: %s holds %s, cutoff at %s",
				b.UserID, u.describe(), deadline.Format(time.RFC3339))
			s.sendOverLimitEmail(b.UserID, u, deadline)
			continue
		}

		if now.Before(b.OverLimitSince.Add(OverLimitGrace)) {
			continue
		}

		log.Printf("billing over-limit: cutoff for %s, still holding %s after the grace window",
			b.UserID, u.describe())
		// The same three steps the payment cutoff takes, in the same order.
		//
		// suspendTenantLinks was missing, and its absence was not a smaller
		// cutoff - it was the whole cutoff for a route-only tenant, who has no
		// servers to stop and no warp peers to drop. They held more addresses
		// than they bought, got the email, and then nothing happened.
		//
		// This only takes effect at all because handlers.ownerCutOff and
		// store.ownerCutOffSQL now know about the over-limit grace too. Without
		// them the ACL reconciler re-provisioned every link this dropped on its
		// next 60s tick, which is what it was already doing to the warp peers
		// above: the sweep and the reconciler fought each other every hour and
		// the tenant never noticed either.
		s.stopTenantServers(ctx, b.UserID)
		s.suspendTenantLinks(ctx, b.UserID)
		s.dropWarpPeersOnceStopped(ctx, b.UserID, b.OverLimitSince.Add(OverLimitGrace), now)
	}
}

func (s *BillingLifecycleService) sendOverLimitEmail(userID string, u tenantUsage, deadline time.Time) {
	usr, err := s.store.GetUserByID(userID)
	if err != nil || usr == nil || usr.Email == "" {
		return
	}
	transport, err := mailer.Load(s.store, "auth")
	if err != nil {
		return
	}
	body := fmt.Sprintf(`Hi %s,

Your account is using more than your subscription covers: %s.

Nothing has stopped. You have until %s to get back within your limits, either by
removing what you no longer need or by raising your subscription.

If you are still over the limit after that, EVERYTHING on your account is
disconnected - not just the excess. We do not choose which of your machines to
stop; that choice is yours.

Nothing is deleted either way. Your servers, data and backups stay where they
are and come straight back once you are within your limits.

Manage your account here:

%s

- Dylaris
`, usr.Username, u.describe(), deadline.Format("2 January 2006, 15:04 MST"), s.frontendURL)

	if err := transport.Send(mailer.Message{
		To:      usr.Email,
		Subject: "Action needed: your account is over its limit",
		Body:    body,
	}); err != nil {
		log.Printf("billing over-limit: email %s: %v", userID, err)
	}
}
