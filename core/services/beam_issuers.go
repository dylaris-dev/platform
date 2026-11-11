package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"dylaris-core/pkg/leader"
	"dylaris-core/store"
)

// Beam issuer records: what the beam relay reads to admit, route, meter and
// limit a managed-gateway customer's own beam tickets. Only this Core writes
// them; the customer's Core never touches our Redis.
//
//	beam:issuer:<kid>  JSON BeamIssuerRecord, no TTL. Present only while the key
//	                   is live, the owner holds a managed product and is not cut
//	                   off. Deleted otherwise.
//	beam:ext:<kitID>   the kit's link token, 24 h TTL, refreshed by the kit's
//	                   heartbeat (handlers/gateway/link_api.go).
//
// The relay compiles its own copy of the record: fields may be ADDED, never
// renamed or repurposed; V tells an incompatible future shape apart.
const (
	BeamIssuerKeyPrefix = "beam:issuer:"
	BeamExtKeyPrefix    = "beam:ext:"

	// BeamTenantMaxConnsKey is the operator setting for the per-tenant relay
	// connection cap (ParseLimitSetting: "" = default, "unlimited" = no cap).
	BeamTenantMaxConnsKey = "beam.tenant_max_conns"

	// MaxBeamIssuerKeysPerOwner allows one rotation overlap: the new key is
	// registered before the old one is revoked.
	MaxBeamIssuerKeysPerOwner = 2

	beamIssuerReconcileInterval = 5 * time.Minute
	// ponytail: one pass handles at most this many owners, in owner-id order;
	// rotate an offset across passes if managed customers ever outgrow it.
	beamIssuerReconcileMaxOwners = 1000
)

// DefaultBeamTenantMaxConns is the product default for beam.tenant_max_conns.
var DefaultBeamTenantMaxConns = LimitPtr(16)

var beamKidRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidBeamKid reports whether kid has the shape Core mints (32 lowercase hex).
// Checked before any kid reaches Redis or the DB.
func ValidBeamKid(kid string) bool { return beamKidRe.MatchString(kid) }

// BeamIssuerRecord is the v1 JSON at beam:issuer:<kid>. MaxConns follows the
// limits convention: absent = no cap, 0 = none, n = the cap. BwBps is Task 5's
// and stays absent until then (absent = only the fleet cap).
type BeamIssuerRecord struct {
	V        int      `json:"v"`
	Owner    string   `json:"owner"`
	Pub      []byte   `json:"pub"` // base64 std, 32 bytes
	Kits     []string `json:"kits"`
	MaxConns *int64   `json:"max_conns,omitempty"`
}

// HasManagedProduct reports whether the owner holds a managed-gateway product.
// The only writer of a user-scope ("*", edge) traffic row is the store, and it
// writes one only for a managed plan (handlers/billing/store.go).
func HasManagedProduct(st TrafficLimitReader, ownerID string) (bool, error) {
	l, err := st.GetTrafficLimit("user:"+ownerID, TrafficRegionAny, TrafficKindEdge)
	if err != nil {
		return false, err
	}
	return l != nil, nil
}

// PublishBeamIssuers makes the owner's beam:issuer:* records match the DB:
// every live key of an owner who holds a managed product and is not cut off is
// written, every other key of the owner is deleted. A DB fault returns before
// Redis is touched, so a blip never deletes a paying customer's records.
//
// suspendGrace is the same grace routeOnlyKey applies (store.OwnerCutOff).
func PublishBeamIssuers(ctx context.Context, st store.Store, rdb *redis.Client, suspendGrace time.Duration, ownerID string) error {
	if st == nil || rdb == nil || ownerID == "" {
		return nil
	}
	keys, err := st.ListBeamIssuerKeysByOwner(ownerID)
	if err != nil {
		return fmt.Errorf("list beam issuer keys: %w", err)
	}
	if len(keys) == 0 {
		return nil
	}
	b, err := st.GetUserBilling(ownerID)
	if err != nil {
		return fmt.Errorf("billing: %w", err)
	}
	managed, err := HasManagedProduct(st, ownerID)
	if err != nil {
		return fmt.Errorf("managed product: %w", err)
	}
	allowed := managed && !store.OwnerCutOff(b, suspendGrace, OverLimitGrace, time.Now())

	var kits []string
	var maxConns *int64
	if allowed {
		wk, err := st.ListWarpAPIKeysByOwner(ownerID)
		if err != nil {
			return fmt.Errorf("list kits: %w", err)
		}
		kits = []string{}
		for _, k := range wk {
			if strings.HasPrefix(k.NodeID, "link-") && k.RevokedAt == nil {
				kits = append(kits, k.NodeID)
			}
		}
		sort.Strings(kits)
		raw, err := st.GetSetting("beam.tenant_max_conns")
		if err != nil {
			return fmt.Errorf("setting %s: %w", BeamTenantMaxConnsKey, err)
		}
		maxConns = ParseLimitSetting(strings.TrimSpace(raw), DefaultBeamTenantMaxConns)
	}

	pipe := rdb.TxPipeline()
	for _, k := range keys {
		if !ValidBeamKid(k.ID) {
			continue // never minted by us; nothing of ours to write or delete
		}
		rkey := BeamIssuerKeyPrefix + k.ID
		if !allowed || k.RevokedAt != nil {
			pipe.Del(ctx, rkey)
			continue
		}
		data, err := json.Marshal(BeamIssuerRecord{V: 1, Owner: ownerID, Pub: k.PublicKey, Kits: kits, MaxConns: maxConns})
		if err != nil {
			return fmt.Errorf("marshal issuer %s: %w", k.ID, err)
		}
		pipe.Set(ctx, rkey, data, 0)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("write beam issuer records: %w", err)
	}
	return nil
}

// BeamIssuerReconciler re-publishes every owner's records every five minutes,
// so a publish that failed or was never called (a graced suspension that has
// since elapsed, a plan change) heals, and sweeps records whose row is gone
// (an account deletion cascades the rows away).
type BeamIssuerReconciler struct {
	store        store.Store
	redis        *redis.Client
	suspendGrace time.Duration
	leader       leader.Election
}

func NewBeamIssuerReconciler(st store.Store, rdb *redis.Client, suspendGrace time.Duration) *BeamIssuerReconciler {
	return &BeamIssuerReconciler{store: st, redis: rdb, suspendGrace: suspendGrace}
}

func (r *BeamIssuerReconciler) SetLeader(l leader.Election) { r.leader = l }

func (r *BeamIssuerReconciler) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(beamIssuerReconcileInterval)
		defer t.Stop()
		for {
			r.RunOnce(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// RunOnce is one bounded pass, one owner at a time. Errors are logged, never
// returned: the next pass retries.
func (r *BeamIssuerReconciler) RunOnce(ctx context.Context) {
	defer func() {
		if p := recover(); p != nil {
			logErrf("beam-issuers", "reconcile panic: %v", p)
		}
	}()
	if r.leader != nil && !r.leader.IsLeader() {
		return
	}
	if r.store == nil || r.redis == nil {
		return
	}
	owners, err := r.store.ListBeamIssuerOwners()
	if err != nil {
		logErrf("beam-issuers", "list owners: %v", err)
		return
	}
	if len(owners) > beamIssuerReconcileMaxOwners {
		log.Printf("beam-issuers: %d owners, reconciling the first %d this pass", len(owners), beamIssuerReconcileMaxOwners)
		owners = owners[:beamIssuerReconcileMaxOwners]
	}
	for _, owner := range owners {
		if ctx.Err() != nil {
			return
		}
		octx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := PublishBeamIssuers(octx, r.store, r.redis, r.suspendGrace, owner); err != nil {
			logErrf("beam-issuers", "publish %s: %v", owner, err)
		}
		cancel()
	}
	r.sweepOrphans(ctx)
}

// sweepOrphans deletes beam:issuer:<kid> records with no row behind them.
func (r *BeamIssuerReconciler) sweepOrphans(ctx context.Context) {
	kids, err := r.store.ListBeamIssuerKids()
	if err != nil {
		logErrf("beam-issuers", "list kids: %v", err)
		return
	}
	known := make(map[string]bool, len(kids))
	for _, k := range kids {
		known[k] = true
	}
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	iter := r.redis.Scan(sctx, 0, BeamIssuerKeyPrefix+"*", 200).Iterator()
	for iter.Next(sctx) {
		key := iter.Val()
		if known[strings.TrimPrefix(key, BeamIssuerKeyPrefix)] {
			continue
		}
		if err := r.redis.Del(sctx, key).Err(); err != nil {
			logErrf("beam-issuers", "delete orphan %s: %v", key, err)
		}
	}
	if err := iter.Err(); err != nil {
		logErrf("beam-issuers", "scan: %v", err)
	}
}
