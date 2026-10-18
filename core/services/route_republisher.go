package services

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/redis/go-redis/v9"

	"dylaris-core/pkg/leader"
	"dylaris-core/store"
)

// republishInterval is how often the durable route-only records are compared
// against the live routing table.
//
// A minute, matching the hub's own sync cadence: this is a repair loop, not the
// write path. Creating a route publishes it immediately and does not wait for a
// tick, so the only thing this interval bounds is how long a route stays gone
// after Redis lost it - which previously was forever.
const republishInterval = 60 * time.Second

type routeRepublishStore interface {
	ListCoreLinkRoutes() ([]store.CoreLinkRoute, error)
	UpsertCoreLinkRoute(r store.CoreLinkRoute) error
}

// RouteRepublisher writes the stored route-only entries back into Redis.
//
// Route-only routes are published by Core straight into Redis, because the edge
// reaches the tenant's own link over the tenant's own tunnel and no hub row
// describes that. The consequence was that route:<domain> was their ONLY copy:
// the hub rebuilds its own routes from its rows every sync tick and skips these
// by design, so a Redis restart without persistence, a recreated volume or an
// eviction took every route-only route away for good while every managed route
// came back within the minute. An operator saw it as "after a redeploy the
// route is sometimes just gone".
//
// This is the writer that was missing. It never deletes and never invents: it
// restores a domain that has no entry, and corrects one whose entry disagrees
// with the row - and it leaves anything owned by somebody else exactly where it
// is, saying so once per pass.
type RouteRepublisher struct {
	store  routeRepublishStore
	redis  *redis.Client
	leader leader.Election

	// adopted is written and read only by the loop goroutine, so it needs no
	// lock. It exists because adoption has to WAIT for leadership rather than
	// be skipped when it is not there yet - see adoptOnce.
	adopted bool
}

func NewRouteRepublisher(s routeRepublishStore, r *redis.Client) *RouteRepublisher {
	return &RouteRepublisher{store: s, redis: r}
}

func (s *RouteRepublisher) SetLeader(l leader.Election) { s.leader = l }

func (s *RouteRepublisher) Start(ctx context.Context) {
	log.Printf("Route republisher started (interval: %s)", republishInterval)
	ticker := time.NewTicker(republishInterval)
	go func() {
		defer ticker.Stop()
		s.adoptOnce(ctx)
		s.RunOnce(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.adoptOnce(ctx)
				s.RunOnce(ctx)
			}
		}
	}()
}

// adoptOnce runs the boot-time adoption pass on the leader, and keeps trying
// until it has run.
//
// The election starts in its own goroutine and its flag is false until a Redis
// round trip sets it, so a synchronous adopt at startup asks "am I the leader"
// before anybody is - and adoption is one-shot, so losing that race skipped it
// for the life of the process on EVERY replica. The one thing that recorded the
// addresses predating the table then silently did nothing, which is the failure
// it exists to prevent, inside the feature meant to prevent it.
func (s *RouteRepublisher) adoptOnce(ctx context.Context) {
	if s.adopted {
		return
	}
	if s.leader != nil && !s.leader.IsLeader() {
		return
	}
	s.adoptExisting(ctx)
	s.adopted = true
}

// RunOnce reconciles every stored route once. Exported for tests and for a
// caller that wants the routing table current right now.
func (s *RouteRepublisher) RunOnce(ctx context.Context) {
	if s.leader != nil && !s.leader.IsLeader() {
		return
	}
	if s.redis == nil || s.store == nil {
		return
	}
	rows, err := s.store.ListCoreLinkRoutes()
	if err != nil {
		// Reading it as "no routes" would be a silent no-op every minute, and
		// this loop only ever exists for the case where something already went
		// wrong.
		logErrf("route-republisher", "republish: list stored routes: %v", err)
		return
	}
	restored, corrected := 0, 0
	for _, r := range rows {
		switch s.republish(ctx, r) {
		case republishRestored:
			restored++
		case republishCorrected:
			corrected++
		}
	}
	if restored > 0 || corrected > 0 {
		log.Printf("[routes] republished %d missing and corrected %d drifted route-only entries", restored, corrected)
	}
}

type republishOutcome int

const (
	republishUnchanged republishOutcome = iota
	republishRestored
	republishCorrected
	republishSkipped
)

// coreOwnedRouteValue is the route:<domain> value for a route-only entry. The
// create path and the republisher both write it, and the drift check compares
// bytes, so there is exactly one place that builds it.
func coreOwnedRouteValue(r store.CoreLinkRoute) ([]byte, error) {
	return json.Marshal(map[string]interface{}{
		"domain":         r.Domain,
		"target_ip":      r.TargetHost,
		"target_port":    r.TargetPort,
		"tunnel_id":      r.LinkToken,
		"server_uuid":    "",
		"core_owned":     true,
		"owner_id":       r.OwnerID,
		"proxy_protocol": r.ProxyProtocol,
	})
}

func (s *RouteRepublisher) republish(ctx context.Context, r store.CoreLinkRoute) republishOutcome {
	data, err := coreOwnedRouteValue(r)
	if err != nil {
		return republishSkipped
	}

	// SETNX, the same claim the create path makes, so restoring a route can
	// never take a domain from whoever holds it now.
	claimed, err := s.redis.SetNX(ctx, "route:"+r.Domain, data, coreOwnedRouteTTL).Result()
	if err != nil {
		logErrf("route-republisher", "republish %s: %v", r.Domain, err)
		return republishSkipped
	}
	if claimed {
		if err := s.redis.SAdd(ctx, "sys:index:routes", r.Domain).Err(); err != nil {
			logErrf("route-republisher", "republish %s: index: %v", r.Domain, err)
		}
		log.Printf("[routes] restored route-only entry %s (it was missing from Redis)", r.Domain)
		return republishRestored
	}

	live, err := s.redis.Get(ctx, "route:"+r.Domain).Result()
	if err != nil {
		return republishSkipped
	}
	if live == string(data) {
		// Still index it: the value can be present while the set that lists it
		// is not, and every listing in the platform reads the set.
		s.redis.SAdd(ctx, "sys:index:routes", r.Domain)
		return republishUnchanged
	}
	var holder struct {
		CoreOwned bool   `json:"core_owned"`
		OwnerID   string `json:"owner_id"`
	}
	if json.Unmarshal([]byte(live), &holder) != nil || !holder.CoreOwned || holder.OwnerID != r.OwnerID {
		// Somebody else's entry, or a managed route the hub wrote. Never
		// overwritten from here: this loop repairs, and a domain held by
		// another owner is a conflict for a human to resolve, not a race for
		// two writers to keep flipping every minute.
		log.Printf("[routes] not republishing %s: it is held by another route now", r.Domain)
		return republishSkipped
	}
	if err := s.redis.Set(ctx, "route:"+r.Domain, data, coreOwnedRouteTTL).Err(); err != nil {
		logErrf("route-republisher", "republish %s: %v", r.Domain, err)
		return republishSkipped
	}
	s.redis.SAdd(ctx, "sys:index:routes", r.Domain)
	return republishCorrected
}

// adoptExisting records the route-only entries that were live BEFORE there was
// anywhere to record them.
//
// Without this the repair loop protects only routes created from now on, and an
// operator has no way to tell which of their existing addresses are covered -
// they would each have to be opened and saved again to acquire a row. So the
// first pass after the upgrade reads what Redis holds and writes it down.
//
// Deliberately ONE-SHOT, at startup, and never on the ticker. Redis is the
// authority here exactly once; making it a standing rule would undo the point
// of the table, and would resurrect a route deleted in the window between its
// row and its key going away.
func (s *RouteRepublisher) adoptExisting(ctx context.Context) {
	if s.leader != nil && !s.leader.IsLeader() {
		return
	}
	if s.redis == nil || s.store == nil {
		return
	}
	rows, err := s.store.ListCoreLinkRoutes()
	if err != nil {
		// Every domain would look unrecorded, and this writes rows.
		logErrf("route-republisher", "adopt: list stored routes: %v", err)
		return
	}
	stored := make(map[string]bool, len(rows))
	for _, r := range rows {
		stored[r.Domain] = true
	}
	domains, err := s.redis.SMembers(ctx, "sys:index:routes").Result()
	if err != nil {
		logErrf("route-republisher", "adopt: %v", err)
		return
	}
	adopted := 0
	for _, domain := range domains {
		if stored[domain] {
			continue
		}
		val, err := s.redis.Get(ctx, "route:"+domain).Result()
		if err != nil {
			continue
		}
		var live struct {
			TargetIP   string `json:"target_ip"`
			TargetPort int    `json:"target_port"`
			TunnelID   string `json:"tunnel_id"`
			CoreOwned  bool   `json:"core_owned"`
			OwnerID    string `json:"owner_id"`
			// Carried over so adopting a route never switches it off.
			ProxyProtocol bool `json:"proxy_protocol"`
		}
		// Only OUR kind. A hub-managed route already has a durable row of its
		// own in the hub's database, and copying it here would give one route
		// two owners that both write it.
		if json.Unmarshal([]byte(val), &live) != nil || !live.CoreOwned || live.OwnerID == "" {
			continue
		}
		if err := s.store.UpsertCoreLinkRoute(store.CoreLinkRoute{
			Domain: domain, OwnerID: live.OwnerID, LinkToken: live.TunnelID,
			TargetHost: live.TargetIP, TargetPort: live.TargetPort,
			ProxyProtocol: live.ProxyProtocol,
		}); err != nil {
			logErrf("route-republisher", "adopt %s: %v", domain, err)
			continue
		}
		adopted++
	}
	if adopted > 0 {
		log.Printf("[routes] recorded %d route-only entries that existed before there was anywhere to record them", adopted)
	}
}
