package services

import (
	"context"
	"dylaris-core/pkg/leader"
	"dylaris-core/store"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Traffic metering keys (written by the edge + beam relay, read here).
//
//	dylaris:traffic:edge:{serverUUID}   HASH {rx,tx}   monotonic player bytes
//	dylaris:traffic:relay:{serverUUID}  HASH {up,down}  monotonic filebrowser bytes
//
// Each writer flushes positive DELTAS via HINCRBY, so the Redis value is the
// cumulative byte count for that server (across edge restarts and multiple
// edges). The aggregator stores the last value it has already billed in
//
//	dylaris:traffic:agg:seen:{kind}:{serverUUID}
//
// and adds (current - seen) to the tenant's monthly row, so it is idempotent and
// loss-tolerant: a tick that fails to write the DB simply leaves `seen` untouched
// and re-reads the same delta next tick.
const (
	trafficEdgePrefix  = "dylaris:traffic:edge:"
	trafficRelayPrefix = "dylaris:traffic:relay:"
	trafficSeenPrefix  = "dylaris:traffic:agg:seen:"
	trafficSeenTTL     = 30 * 24 * time.Hour
	trafficScanCount   = 200

	// Counter subjects for route-only addresses carry this infix. It lives
	// under the edge prefix rather than beside it so the edge's existing Redis
	// ACL grant covers it - see subjectOwners.
	trafficOwnerSubjectPrefix = "owner:"

	// Region seen keys hang off the subject's own seen key. '#' separates them
	// because a SUBJECT may contain ':' ("owner:<id>") and a region may not -
	// see meterRegion on the edge, which strips everything outside [a-z0-9-].
	trafficRegionSeenSep = "#"

	// Marks that a subject has been through region initialisation once.
	//
	// Without it "this region has no marker" cannot be told apart from "this
	// SUBJECT has no markers", and the two need opposite handling: the first is
	// a region that has just started carrying traffic and must be billed, the
	// second is a subject that predates region tracking and must be seeded.
	// Asking the subject-level question per region silently dropped the first
	// delta of every region that appeared later - the breakdown then under-counts
	// the total it is supposed to break down, permanently.
	trafficRegionInitPrefix = "dylaris:traffic:agg:rinit:"

	// Where bytes go when the edge that wrote them did not say. It is a named
	// region rather than a dropped one: they are real bytes, they are in the
	// billing total, and a breakdown that quietly omitted them would not add up
	// to the number the tenant is charged on.
	regionUnknown = "unknown"
)

// TrafficAggregator turns the per-server byte counters edges + relays publish to
// Redis into per-tenant monthly rows in traffic_usage. Leader-gated (one Core
// bills) and BYON-gated (no tenants = nothing to meter), so it is a no-op in
// solo/hoster mode.
type TrafficAggregator struct {
	store    store.Store
	redis    *redis.Client
	flags    *FeatureFlags
	leader   leader.Election
	interval time.Duration
}

func NewTrafficAggregator(st store.Store, r *redis.Client, flags *FeatureFlags) *TrafficAggregator {
	return &TrafficAggregator{store: st, redis: r, flags: flags, interval: 60 * time.Second}
}

// SetLeader wires the leader-election gate. Call once at boot.
func (a *TrafficAggregator) SetLeader(l leader.Election) { a.leader = l }

func (a *TrafficAggregator) Start(ctx context.Context) {
	if a.redis == nil {
		return
	}
	log.Println("Traffic Aggregator started (BYON metering)")
	ticker := time.NewTicker(a.interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if a.leader != nil && !a.leader.IsLeader() {
					continue
				}
				a.runOnce(ctx)
			}
		}
	}()
}

// trafficAcc accumulates one tenant's new bytes within a single tick.
type trafficAcc struct {
	edge  int64
	relay int64
	// byRegion splits the same bytes by (region, kind, product). Per kind it sums
	// to the matching total above, so the breakdown always adds up to the bill; a
	// producer that did not tag contributes to regionUnknown rather than
	// vanishing from the split.
	byRegion map[breakdownCell]int64
}

// breakdownCell is one cell of the breakdown: where the bytes moved, which
// component moved them, and which product they belong to. Player traffic and
// data traffic are priced and capped separately, so they cannot share a cell;
// the product cannot either, or one product's bytes are attributed to the other
// by the upsert.
type breakdownCell struct {
	region  string
	kind    string
	product string
}

// trafficProductOf reads the product off the counter's SUBJECT.
//
// The subject already carries it and no producer had to be changed: Core writes
// an "owner:<userID>" subject for a route-only address, because a route-only
// tenant has no server of ours to key on, and a bare server UUID for a server
// running on a machine the tenant owns. Anything else is a shape this does not
// know, and it says so rather than guessing - those bytes are still in the
// billing total.
//
// Note what this means for data traffic: the beam relay only ever meters a
// server UUID, so relay bytes are BYON by construction. A route-only customer
// runs their own server and never touches beam, so an empty route-only cell for
// file transfers is correct rather than missing.
func trafficProductOf(subject string) string {
	if strings.HasPrefix(subject, trafficOwnerSubjectPrefix) {
		return store.TrafficProductRoute
	}
	if subject != "" {
		return store.TrafficProductBYON
	}
	return store.TrafficProductUnknown
}

// seenUpdate is a `seen` key write deferred until the tenant's DB row is written.
type seenUpdate struct {
	key string
	val int64
}

func (a *TrafficAggregator) runOnce(ctx context.Context) {
	if !a.flags.IsTenancyEnabled(ctx) {
		return // no tenants to bill
	}
	owners, err := a.subjectOwners()
	if err != nil {
		logErrf("traffic-aggregator", "tenant lookup: %v", err)
		return
	}
	if len(owners) == 0 {
		return
	}

	perTenant := map[string]*trafficAcc{}
	tenantSeen := map[string][]seenUpdate{}

	a.collect(ctx, trafficEdgePrefix, owners, perTenant, tenantSeen, store.TrafficKindEdge)
	a.collect(ctx, trafficRelayPrefix, owners, perTenant, tenantSeen, store.TrafficKindRelay)

	period := monthStartUTC(time.Now())
	for tenant, acc := range perTenant {
		if err := a.store.AddTrafficUsage(tenant, period, acc.edge, acc.relay); err != nil {
			// Leave `seen` untouched so the delta is retried next tick.
			logErrf("traffic-aggregator", "add usage for %s: %v", tenant, err)
			continue
		}
		// The breakdown is written AFTER the total and its failure does not
		// abort the tick: traffic_usage is what a tenant is charged on, and a
		// region row that cannot be written must never hold that back. The
		// markers still advance, so a lost region delta is lost rather than
		// re-billed - under-counting the split, never the bill.
		for rk, bytes := range acc.byRegion {
			if bytes <= 0 {
				continue
			}
			if err := a.store.AddTrafficUsageRegion(tenant, period, rk.region, rk.kind, rk.product, bytes); err != nil {
				logErrf("traffic-aggregator", "add region usage for %s/%s/%s/%s: %v", tenant, rk.region, rk.kind, rk.product, err)
			}
		}
		for _, su := range tenantSeen[tenant] {
			a.redis.Set(ctx, su.key, su.val, trafficSeenTTL)
		}
	}

	// Deliberately NOT the widened map: this walks tenants who own SERVERS, and
	// a route-only tenant has no backups to snapshot. Passing them in would
	// write a zero-byte storage row for an account that never had one.
	serverOwners, err := a.store.TenantServerOwners()
	if err != nil {
		logErrf("traffic-aggregator", "server owner lookup for backup snapshot: %v", err)
		return
	}
	a.snapshotBackupStorage(serverOwners, period)
}

// snapshotBackupStorage overwrites each tenant's R2 backup-storage gauge for the
// period. Unlike traffic (a cumulative flow), storage is a current total, so it
// is set, not added — and tenants whose backups dropped to 0 are explicitly
// reset, which is why every known tenant is written, not just those with backups.
func (a *TrafficAggregator) snapshotBackupStorage(owners map[string]string, period time.Time) {
	byTenant, err := a.store.TenantBackupBytes()
	if err != nil {
		logErrf("traffic-aggregator", "backup storage lookup: %v", err)
		return
	}
	// Union of tenants that own a server and tenants that still hold backups
	// (a tenant can keep backups after deleting every server).
	tenants := map[string]struct{}{}
	for _, tenant := range owners {
		tenants[tenant] = struct{}{}
	}
	for tenant := range byTenant {
		tenants[tenant] = struct{}{}
	}
	for tenant := range tenants {
		if err := a.store.SetTrafficBackupBytes(tenant, period, byTenant[tenant]); err != nil {
			logErrf("traffic-aggregator", "set backup bytes for %s: %v", tenant, err)
		}
	}
}

// subjectOwners maps every counter SUBJECT the edges write to the tenant it
// belongs to.
//
// Two kinds share one namespace, because they share one Redis key prefix (the
// edge's ACL grants exactly dylaris:traffic:edge:*, so route-only could not get
// a prefix of its own without a re-provisioned ACL):
//
//	<serverUUID>      a managed server on a tenant-owned node
//	owner:<userID>    a route-only address, which has no server at all
//
// Route-only used to be absent from this map because it was absent from the
// EDGE too: Core writes server_uuid:"" for those routes, metering keyed on the
// server, and the whole product's traffic reached no counter. Adding the
// counter without adding it here would have been the same defect one layer up.
//
// A subject that resolves to nothing is skipped by collect, which is what keeps
// a stale counter for a deleted server or a removed route from inventing a row.
func (a *TrafficAggregator) subjectOwners() (map[string]string, error) {
	servers, err := a.store.TenantServerOwners()
	if err != nil {
		return nil, err
	}
	// COPIED, never extended in place. runOnce asks TenantServerOwners a second
	// time for the backup snapshot precisely because that one must see servers
	// only; writing the route-only subjects into the map it returned would hand
	// them to it anyway the moment the store hands back a shared or cached map.
	owners := make(map[string]string, len(servers))
	for k, v := range servers {
		owners[k] = v
	}
	routes, err := a.store.ListCoreLinkRoutes()
	if err != nil {
		return nil, err
	}
	for _, r := range routes {
		if r.OwnerID == "" {
			continue
		}
		owners[trafficOwnerSubjectPrefix+r.OwnerID] = r.OwnerID
	}
	return owners, nil
}

// collect scans one counter family, attributes each subject's new bytes to its
// tenant, and records the matching `seen` advance (applied later, after the DB
// write succeeds).
func (a *TrafficAggregator) collect(ctx context.Context, prefix string, owners map[string]string, perTenant map[string]*trafficAcc, tenantSeen map[string][]seenUpdate, kind string) {
	isEdge := kind == store.TrafficKindEdge
	var cursor uint64
	for {
		keys, next, err := a.redis.Scan(ctx, cursor, prefix+"*", trafficScanCount).Result()
		if err != nil {
			logErrf("traffic-aggregator", "scan %s: %v", prefix, err)
			return
		}
		for _, key := range keys {
			subject := strings.TrimPrefix(key, prefix)
			tenant, ok := owners[subject]
			if !ok {
				continue // platform server, deleted route, or unknown — not billed
			}
			fields, err := a.redis.HGetAll(ctx, key).Result()
			if err != nil {
				continue
			}
			current := sumByteFields(fields)
			seenKey := trafficSeenPrefix + kind + ":" + subject
			last, _ := a.redis.Get(ctx, seenKey).Int64()
			delta := current - last
			if delta < 0 {
				delta = current // counter reset (key expired then recreated)
			}
			if delta <= 0 {
				continue
			}
			acc := perTenant[tenant]
			if acc == nil {
				acc = &trafficAcc{byRegion: map[breakdownCell]int64{}}
				perTenant[tenant] = acc
			}
			if isEdge {
				acc.edge += delta
			} else {
				acc.relay += delta
			}
			// Both kinds are split. The beam relay carries a customer's file
			// transfers and is region-aware in its own right, so leaving it out
			// would put a US customer's data traffic nowhere - which is the
			// cross-region case the split exists for.
			a.collectRegions(ctx, subject, kind, trafficProductOf(subject), fields, last > 0, acc, tenantSeen, tenant)
			tenantSeen[tenant] = append(tenantSeen[tenant], seenUpdate{key: seenKey, val: current})
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
}

// regionBytes splits a counter hash into per-region totals.
//
// An edge new enough to tag writes "rx:<region>" / "tx:<region>"; one that is
// not writes bare "rx" / "tx", and those land in regionUnknown. Both shapes can
// be present in the same hash at once - that is what a rolling edge upgrade
// looks like - and the sum across regions equals sumByteFields either way,
// which is the property the whole breakdown rests on.
func regionBytes(fields map[string]string) map[string]int64 {
	out := map[string]int64{}
	for name, v := range fields {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			continue
		}
		_, region, tagged := strings.Cut(name, ":")
		if !tagged || region == "" {
			region = regionUnknown
		}
		out[region] += n
	}
	return out
}

// collectRegions computes the per-region deltas for one counter.
//
// Two different absences look identical from a single region marker, and they
// need opposite handling:
//
//	the SUBJECT has never been split      seed, do not bill
//	this REGION has just started moving   bill it in full
//
// The first is a subject that was already being counted before this Core knew
// about regions: its region markers are missing because nobody was tracking
// them, and reading that as a delta would post the whole historical counter
// into the breakdown in one tick. The second is ordinary new traffic, and
// seeding it drops the first delta - which is what this used to do to every
// region that appeared after a subject was already counted, leaving the
// breakdown permanently short of the total it breaks down.
//
// trafficRegionInitPrefix is what separates them: it is per SUBJECT, so once a
// subject has been initialised a missing region marker can only mean the second
// case. hadTotalSeen only decides what initialisation DOES - seed a subject the
// aggregator already knew, bill one it is meeting for the first time, which is
// what the total does on the same tick.
func (a *TrafficAggregator) collectRegions(
	ctx context.Context,
	subject, kind, product string,
	fields map[string]string,
	hadTotalSeen bool,
	acc *trafficAcc,
	tenantSeen map[string][]seenUpdate,
	tenant string,
) {
	initKey := trafficRegionInitPrefix + kind + ":" + subject
	initialised, err := a.redis.Get(ctx, initKey).Result()
	subjectKnown := err == nil && initialised != ""
	if !subjectKnown {
		// Deferred like every other marker, so a failed DB write leaves it
		// unset and the whole initialisation is retried next tick.
		tenantSeen[tenant] = append(tenantSeen[tenant], seenUpdate{key: initKey, val: 1})
	}

	for region, current := range regionBytes(fields) {
		seenKey := trafficSeenPrefix + kind + ":" + subject + trafficRegionSeenSep + region
		last, err := a.redis.Get(ctx, seenKey).Int64()
		missing := err != nil
		if missing && !subjectKnown && hadTotalSeen {
			// Seeding this subject's history away, once. Only reachable while
			// the subject has never been split before.
			tenantSeen[tenant] = append(tenantSeen[tenant], seenUpdate{key: seenKey, val: current})
			continue
		}
		delta := current - last
		if delta < 0 {
			delta = current
		}
		if delta <= 0 {
			continue
		}
		if acc.byRegion == nil {
			acc.byRegion = map[breakdownCell]int64{}
		}
		acc.byRegion[breakdownCell{region: region, kind: kind, product: product}] += delta
		tenantSeen[tenant] = append(tenantSeen[tenant], seenUpdate{key: seenKey, val: current})
	}
}

// sumByteFields sums every numeric value in a counter hash (rx+tx, up+down),
// ignoring unparseable fields so an extra label never poisons the total.
func sumByteFields(fields map[string]string) int64 {
	var total int64
	for _, v := range fields {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			continue
		}
		total += n
	}
	return total
}

// monthStartUTC is the first instant of t's month in UTC — the billing-period key.
func monthStartUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}
