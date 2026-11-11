package services

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Data traffic (beam + custom tabs) past a tenant's allowance is THROTTLED,
// not stopped, unless the tenant agreed to pay for data overage. Owner
// decision 2026-10-24, docs/superpowers/specs/2026-10-24-data-traffic-throttle-design.md.
//
// Core decides; the node's beam server and Core's own tab proxy enforce, both
// reading dylaris:server:<uuid>:data_bw (bytes per second). A missing key is
// no throttle, so an older node simply does not throttle.

const (
	// DataThrottleSettingKey is the throttle speed in Mbit/s.
	DataThrottleSettingKey  = "traffic.data_throttle_mbit"
	DataThrottleDefaultMbit = 50
	DataThrottleMinMbit     = 20
	// dataThrottleTTL outlives several aggregator ticks, so a Core restart does
	// not lift a throttle, and a tenant who is no longer over is released by
	// the explicit delete well before it expires.
	dataThrottleTTL = 10 * time.Minute
	// dataThrottledSet lists the tenants currently throttled, so a release can
	// find the keys it has to remove.
	dataThrottledSet = "dylaris:traffic:data_throttled"
	// managedThrottledSet is the same for managed pools, kept apart: an account
	// can hold both, and releasing one must not release the other.
	managedThrottledSet = "dylaris:traffic:managed_throttled"

	// Data grace mode (owner decision 2026-11-11): a managed pool without the
	// traffic consent that reaches its included amount keeps beam at a FIXED
	// 50 Mbit/s (not traffic.data_throttle_mbit) and 30 GB per owner per UTC
	// day, enforced by the relay. Those bytes stay out of the pool.
	GraceBps             int64 = 6_250_000
	GraceDailyQuotaBytes int64 = 30_000_000_000
)

// DataBandwidthKey is where a server's data throttle (bytes/s) is published.
func DataBandwidthKey(serverUUID string) string { return "dylaris:server:" + serverUUID + ":data_bw" }

// DataThrottleMbit is the configured throttle speed, never below the minimum.
func DataThrottleMbit(st store.Store) int {
	v, err := st.GetSetting(DataThrottleSettingKey)
	n, perr := strconv.Atoi(v)
	if err != nil || perr != nil || v == "" {
		return DataThrottleDefaultMbit
	}
	if n < DataThrottleMinMbit {
		return DataThrottleMinMbit
	}
	return n
}

// DataThrottleVerdict is one tenant's state this month.
type DataThrottleVerdict struct {
	UsedGB, IncludedGB int64
	Limited            bool // an allowance exists
	Consent            bool // data overage is billed instead of throttled
	Warn               bool // >= 90 %
	Throttled          bool // >= 100 %
	// Grace is the managed no-consent state past the included amount:
	// Throttled too, at GraceBps plus GraceDailyQuotaBytes.
	Grace bool
}

// JudgeDataTraffic is the pure decision: no allowance means nothing to judge,
// consent means billed rather than throttled. units nil is an account that is
// not metered at all (TrafficUnits).
func JudgeDataTraffic(usedBytes int64, includedGB *int64, units *int64, consent bool) DataThrottleVerdict {
	v := DataThrottleVerdict{UsedGB: usedBytes / 1_000_000_000, Consent: consent}
	if includedGB == nil || units == nil {
		return v
	}
	v.Limited = true
	total := *includedGB * *units
	v.IncludedGB = total
	if consent {
		return v
	}
	limitBytes := total * 1_000_000_000
	v.Throttled = usedBytes >= limitBytes
	v.Warn = usedBytes*10 >= limitBytes*9
	return v
}

// TrafficUnits is how many units the allowance is multiplied by: the nodes and
// route-only locations bought, or one per live manual grant (grantedCap).
//
// nil means the account has no node cap and no link cap at all - nothing was
// ever bought or granted, which is an administrator or a hand-made account,
// not a customer. Such an account is not metered. It used to count as zero
// units, so its allowance was 0 GB: the panel warned "90 % used" at 0 bytes
// and the data throttle slowed it at the first byte. A lapsed customer never
// reads as nil: the store never clears both caps (a subscription that holds
// nothing is cancelled down the suspend path and keeps its last numbers, see
// purchasedEntitlement in the website's coreclient.go), and a store zero for
// one product is stored as nil for that one only.
func TrafficUnits(b *store.UserBilling, now time.Time) *int64 {
	if b == nil {
		return nil
	}
	nodes := grantedCap(b.MaxNodes, b.ManualByonExpiresAt, now)
	links := grantedCap(b.MaxLinks, b.ManualRouteExpiresAt, now)
	if nodes == nil && links == nil {
		return nil
	}
	var n int64
	if nodes != nil && *nodes > 0 {
		n += *nodes
	}
	if links != nil && *links > 0 {
		n += *links
	}
	return &n
}

// DataTrafficVerdictFor reads one tenant's data usage, allowance and consent.
func DataTrafficVerdictFor(st store.Store, userID string, now time.Time) (DataThrottleVerdict, error) {
	period := monthStartUTC(now)
	cells, err := st.GetTrafficUsageRegions(userID, period)
	if err != nil {
		return DataThrottleVerdict{}, err
	}
	var used int64
	for _, c := range cells {
		// Product "route" relay cells are a managed tenant's own-ticket beam
		// bytes: in the managed pool, never in this allowance.
		if c.Kind == store.TrafficKindRelay && c.Product != store.TrafficProductRoute {
			used += c.Bytes
		}
	}
	lim, err := ResolveTrafficLimit(st, userID, "*", store.TrafficKindRelay)
	if err != nil {
		return DataThrottleVerdict{}, err
	}
	b, _ := st.GetUserBilling(userID)
	consent, err := st.GetUserDataBillingEnabled(userID)
	if err != nil {
		return DataThrottleVerdict{}, err
	}
	return JudgeDataTraffic(used, lim.IncludedGB, TrafficUnits(b, now), consent), nil
}

// JudgeManagedPool is the managed-gateway verdict: ONE pool of player and data
// bytes against the user's "*" edge row (owner decision 2026-11-11). With the
// traffic consent the store bills past the allowance up to maxPurchaseGB, so
// data slows only past included + max purchase (nil max purchase: never).
// Without it nothing is billed, so data enters grace mode at the allowance
// itself. The row's numbers are the account's total (the store sends
// units = 1), so they are not multiplied by TrafficUnits. usedBytes is the
// pool WITHOUT grace bytes.
func JudgeManagedPool(usedBytes int64, includedGB, maxPurchaseGB *int64, consent bool) DataThrottleVerdict {
	v := DataThrottleVerdict{UsedGB: usedBytes / 1_000_000_000, Consent: consent}
	if includedGB == nil {
		return v
	}
	v.Limited = true
	limit := *includedGB
	if consent {
		if maxPurchaseGB == nil {
			v.IncludedGB = limit
			return v
		}
		limit += *maxPurchaseGB
	}
	v.IncludedGB = limit
	limitBytes := limit * 1_000_000_000
	v.Throttled = usedBytes >= limitBytes
	v.Grace = v.Throttled && !consent
	v.Warn = usedBytes*10 >= limitBytes*9
	return v
}

// ManagedPoolBytes is the pool a managed tenant is judged and billed on: player
// bytes plus the relay bytes of their own tickets (owner:<id>), minus those
// moved in grace mode. Their servers' beam and tab bytes on our fleet are not
// in it (BYON data verdict), and grace bytes stay in RelayBytes and
// traffic_usage_region for the statistics.
func ManagedPoolBytes(u *store.TrafficUsage) int64 {
	return u.EdgeBytes + u.ManagedRelayBytes - u.GraceRelayBytes()
}

// ManagedPoolVerdictFor judges a managed tenant's pool; managed is false (and
// the verdict empty) for anyone without the user "*" edge row, who keeps the
// BYON / route-only data verdict.
func ManagedPoolVerdictFor(st store.Store, userID string, now time.Time) (v DataThrottleVerdict, managed bool, err error) {
	l, err := st.GetTrafficLimit("user:"+userID, TrafficRegionAny, TrafficKindEdge)
	if err != nil || l == nil {
		return DataThrottleVerdict{}, false, err
	}
	u, err := st.GetTrafficUsage(userID, monthStartUTC(now))
	if err != nil {
		return DataThrottleVerdict{}, true, fmt.Errorf("usage: %w", err)
	}
	b, err := st.GetUserBilling(userID)
	if err != nil {
		return DataThrottleVerdict{}, true, fmt.Errorf("billing: %w", err)
	}
	consent := b != nil && b.TrafficBillingEnabled
	return JudgeManagedPool(ManagedPoolBytes(u), l.IncludedGB, l.MaxPurchaseGB, consent), true, nil
}

// syncManagedGrace makes the stored grace spell follow the verdict: opened at
// the current relay bytes when the pool enters grace, closed (its bytes kept
// out of the pool for the month) when it leaves through consent, a plan change
// or anything else. A new month has no row and so no spell. changed reports a
// transition, so the issuer records are republished at once.
func syncManagedGrace(st store.Store, tenant string, now time.Time, v DataThrottleVerdict) (changed bool, err error) {
	if v.Grace {
		return st.StartTrafficGrace(tenant, monthStartUTC(now))
	}
	return st.EndTrafficGrace(tenant, monthStartUTC(now))
}

// evaluateDataThrottle runs after a tick's usage is written: throttles the
// tenants over their data allowance, releases the ones that are not, and
// tells each tenant once a month at 90 % and at 100 %.
func (a *TrafficAggregator) evaluateDataThrottle(ctx context.Context, owners map[string]string) {
	now := time.Now().UTC()
	serversOf := map[string][]string{}
	for subject, tenant := range owners {
		serversOf[tenant] = append(serversOf[tenant], subject)
	}
	bps := int64(DataThrottleMbit(a.store)) * 125_000
	throttledNow := map[string]bool{}
	managedNow := map[string]bool{}
	for tenant, servers := range serversOf {
		// Two independent verdicts, because they judge different bytes: the
		// managed pool (edge + the tenant's own-ticket relay bytes) paces the
		// issuer records, the BYON data verdict (their servers on our fleet)
		// paces those servers. An account holding both gets both.
		mv, managed, err := ManagedPoolVerdictFor(a.store, tenant, now)
		if err != nil {
			logErrf("data-throttle", "managed verdict for %s: %v", tenant, err)
		} else {
			// A tenant that is not (or no longer) managed reads as no grace,
			// which closes a spell left open by a cancelled plan.
			changed, err := syncManagedGrace(a.store, tenant, now, mv)
			if err != nil {
				logErrf("data-throttle", "grace for %s: %v", tenant, err)
			}
			if managed {
				a.applyManagedThrottle(ctx, tenant, mv, changed, now)
				if mv.Throttled {
					managedNow[tenant] = true
				}
			}
		}

		v, err := DataTrafficVerdictFor(a.store, tenant, now)
		if err != nil {
			logErrf("data-throttle", "verdict for %s: %v", tenant, err)
			continue
		}
		if v.Warn {
			a.notifyDataOnce(ctx, tenant, now, "90", "Data traffic at 90 %",
				fmt.Sprintf("Beam and custom tabs have used %d of %d GB this month. At 100 %% they slow to %d Mbit/s until the month resets, unless data overage is on.", v.UsedGB, v.IncludedGB, bps/125_000))
		}
		if !v.Throttled {
			continue
		}
		throttledNow[tenant] = true
		pipe := a.redis.TxPipeline()
		for _, uuid := range servers {
			if strings.HasPrefix(uuid, trafficOwnerSubjectPrefix) {
				continue // route-only or managed subject: no server, no beam, no tabs
			}
			pipe.Set(ctx, DataBandwidthKey(uuid), bps, dataThrottleTTL)
		}
		pipe.SAdd(ctx, dataThrottledSet, tenant)
		if _, err := pipe.Exec(ctx); err != nil {
			logErrf("data-throttle", "throttle %s: %v", tenant, err)
		}
		a.notifyDataOnce(ctx, tenant, now, "100", "Data traffic limit reached",
			fmt.Sprintf("Beam and custom tabs are slowed to %d Mbit/s until the month resets on %s. Turn on data overage in your store account to lift it.", bps/125_000, monthStartUTC(now).AddDate(0, 1, 0).Format("2 January")))
	}
	// Release whoever is no longer over.
	if listed, err := a.redis.SMembers(ctx, dataThrottledSet).Result(); err == nil {
		for _, tenant := range listed {
			if throttledNow[tenant] {
				continue
			}
			pipe := a.redis.TxPipeline()
			for _, uuid := range serversOf[tenant] {
				pipe.Del(ctx, DataBandwidthKey(uuid))
			}
			pipe.SRem(ctx, dataThrottledSet, tenant)
			if _, err := pipe.Exec(ctx); err != nil {
				logErrf("data-throttle", "release %s: %v", tenant, err)
				continue
			}
			log.Printf("data-throttle: released %s", tenant)
		}
	}
	if listed, err := a.redis.SMembers(ctx, managedThrottledSet).Result(); err == nil {
		for _, tenant := range listed {
			if managedNow[tenant] {
				continue
			}
			if err := a.redis.SRem(ctx, managedThrottledSet, tenant).Err(); err != nil {
				logErrf("data-throttle", "release managed %s: %v", tenant, err)
				continue
			}
			// Drops bw_bps and daily_quota_bytes from the issuer records.
			if err := PublishBeamIssuers(ctx, a.store, a.redis, a.suspendGrace, tenant); err != nil {
				logErrf("data-throttle", "release %s: issuer records: %v", tenant, err)
			}
			log.Printf("data-throttle: released managed pool of %s", tenant)
		}
	}
}

// applyManagedThrottle paces a managed tenant's relay sessions through bw_bps
// (and daily_quota_bytes in grace mode) in the beam issuer records; a foreign
// node never reads our Redis. Their servers on our fleet are NOT touched here:
// those bytes are not in the pool and follow the BYON data verdict. The records
// are republished when the tenant enters the throttled set or the grace spell
// changes; the issuer reconciler recomputes them every five minutes either way.
func (a *TrafficAggregator) applyManagedThrottle(ctx context.Context, tenant string, v DataThrottleVerdict, graceChanged bool, now time.Time) {
	bps := int64(DataThrottleMbit(a.store)) * 125_000
	if v.Warn && !v.Throttled {
		// v.IncludedGB is where the throttle starts: included + what may be
		// bought with consent, the allowance itself without. Say which.
		body := fmt.Sprintf("Player traffic and beam file transfers together have used %d of the %d GB your plan includes this month. At %d GB file transfers slow to %d Mbit/s and %d GB per day until the month resets, unless extra traffic billing is on.", v.UsedGB, v.IncludedGB, v.IncludedGB, GraceBps/125_000, GraceDailyQuotaBytes/1_000_000_000)
		if v.Consent {
			body = fmt.Sprintf("Player traffic and beam file transfers together have used %d GB this month. At %d GB, the most extra traffic your plan can buy, file transfers slow to %d Mbit/s until the month resets.", v.UsedGB, v.IncludedGB, bps/125_000)
		}
		a.notifyDataOnce(ctx, tenant, now, "managed-90", "Gateway traffic nearly at its limit", body)
	}
	if !v.Throttled {
		return
	}
	added, err := a.redis.SAdd(ctx, managedThrottledSet, tenant).Result()
	if err != nil {
		logErrf("data-throttle", "throttle %s: %v", tenant, err)
		return
	}
	if added > 0 || graceChanged {
		if err := PublishBeamIssuers(ctx, a.store, a.redis, a.suspendGrace, tenant); err != nil {
			logErrf("data-throttle", "throttle %s: issuer records: %v", tenant, err)
		}
	}
	resets := monthStartUTC(now).AddDate(0, 1, 0).Format("2 January")
	if v.Grace {
		a.notifyDataOnce(ctx, tenant, now, "managed-100", "Gateway traffic allowance reached",
			fmt.Sprintf("Your plan's included traffic is used. Until the month resets on %s, beam file transfers through the gateway run at %d Mbit/s and up to %d GB per day (UTC); past that, they are unavailable until midnight UTC. These transfers do not count against your plan and are not billed. Turn on extra traffic billing in your store account to lift this.", resets, GraceBps/125_000, GraceDailyQuotaBytes/1_000_000_000))
		return
	}
	a.notifyDataOnce(ctx, tenant, now, "managed-100", "Gateway traffic limit reached",
		fmt.Sprintf("Beam file transfers through the gateway are slowed to %d Mbit/s until the month resets on %s.", bps/125_000, resets))
}

// notifyDataOnce sends one in-app notification per tenant, level and month.
func (a *TrafficAggregator) notifyDataOnce(ctx context.Context, tenant string, now time.Time, level, title, body string) {
	key := fmt.Sprintf("dylaris:traffic:data_notified:%s:%s:%s", tenant, now.Format("2006-01"), level)
	ok, err := a.redis.SetNX(ctx, key, 1, 40*24*time.Hour).Result()
	if err != nil || !ok {
		return
	}
	notify(a.store, tenant, title, body, "/infrastructure")
}

// IsDataThrottled reports whether the tenant is currently throttled.
func IsDataThrottled(ctx context.Context, rdb *redis.Client, tenant string) bool {
	if rdb == nil {
		return false
	}
	ok, err := rdb.SIsMember(ctx, dataThrottledSet, tenant).Result()
	return err == nil && ok
}

// DataLimiter paces data bytes per server for Core's tab proxy, shared across
// every stream of that server so parallel tabs cannot add up past the limit.
type DataLimiter struct {
	rdb   *redis.Client
	mu    sync.Mutex
	bps   map[string]cachedBps
	pacer map[string]time.Time // next free instant per server
}

type cachedBps struct {
	v  int64
	at time.Time
}

func NewDataLimiter(rdb *redis.Client) *DataLimiter {
	return &DataLimiter{rdb: rdb, bps: map[string]cachedBps{}, pacer: map[string]time.Time{}}
}

// rate is the server's throttle in bytes/s, 0 = none; cached for 10 s.
func (d *DataLimiter) rate(ctx context.Context, uuid string) int64 {
	d.mu.Lock()
	c, ok := d.bps[uuid]
	d.mu.Unlock()
	if ok && time.Since(c.at) < 10*time.Second {
		return c.v
	}
	var v int64
	if d.rdb != nil {
		v, _ = d.rdb.Get(ctx, DataBandwidthKey(uuid)).Int64()
	}
	d.mu.Lock()
	d.bps[uuid] = cachedBps{v: v, at: time.Now()}
	d.mu.Unlock()
	return v
}

// Wait blocks until n more bytes of the server's data traffic may pass.
func (d *DataLimiter) Wait(ctx context.Context, uuid string, n int) {
	if d == nil || n <= 0 {
		return
	}
	bps := d.rate(ctx, uuid)
	if bps <= 0 {
		return
	}
	d.mu.Lock()
	now := time.Now()
	next := d.pacer[uuid]
	if next.Before(now) {
		next = now
	}
	wait := next.Sub(now)
	d.pacer[uuid] = next.Add(time.Duration(float64(n) / float64(bps) * float64(time.Second)))
	d.mu.Unlock()
	if wait <= 0 {
		return
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// TabTrafficMeter counts the bytes Core proxies for custom tabs and flushes
// them every 10 s into the server's relay counter, fields tab_up:<region> and
// tab_down:<region>, which the aggregator already sums as data traffic.
type TabTrafficMeter struct {
	rdb      *redis.Client
	regionOf func(nodeID int) string
	mu       sync.Mutex
	counts   map[tabMeterKey]int64
}

type tabMeterKey struct {
	uuid, field string
}

func NewTabTrafficMeter(rdb *redis.Client, regionOf func(nodeID int) string) *TabTrafficMeter {
	return &TabTrafficMeter{rdb: rdb, regionOf: regionOf, counts: map[tabMeterKey]int64{}}
}

// Add records up (browser to server) and down (server to browser) bytes.
func (m *TabTrafficMeter) Add(uuid string, nodeID int, up, down int64) {
	if m == nil || uuid == "" || (up <= 0 && down <= 0) {
		return
	}
	region := ""
	if m.regionOf != nil {
		region = m.regionOf(nodeID)
	}
	m.mu.Lock()
	if up > 0 {
		m.counts[tabMeterKey{uuid, "tab_up:" + region}] += up
	}
	if down > 0 {
		m.counts[tabMeterKey{uuid, "tab_down:" + region}] += down
	}
	m.mu.Unlock()
}

// Run flushes every 10 s until ctx ends, then once more.
func (m *TabTrafficMeter) Run(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.flush(context.Background())
			return
		case <-t.C:
			m.flush(ctx)
		}
	}
}

func (m *TabTrafficMeter) flush(ctx context.Context) {
	m.mu.Lock()
	batch := m.counts
	m.counts = map[tabMeterKey]int64{}
	m.mu.Unlock()
	if len(batch) == 0 || m.rdb == nil {
		return
	}
	pipe := m.rdb.Pipeline()
	for k, n := range batch {
		key := "dylaris:traffic:relay:" + k.uuid
		pipe.HIncrBy(ctx, key, k.field, n)
		pipe.Expire(ctx, key, 7*24*time.Hour)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		// Put the counts back so the next flush carries them.
		m.mu.Lock()
		for k, n := range batch {
			m.counts[k] += n
		}
		m.mu.Unlock()
		logErrf("tab-meter", "flush: %v", err)
	}
}

// NodeRegionCache answers a node's region for the tab meter, refreshed every
// 10 minutes; an unknown node is "".
type NodeRegionCache struct {
	st store.Store
	mu sync.Mutex
	m  map[int]string
	at time.Time
}

func NewNodeRegionCache(st store.Store) *NodeRegionCache {
	return &NodeRegionCache{st: st, m: map[int]string{}}
}

func (c *NodeRegionCache) Region(nodeID int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) > 10*time.Minute {
		if nodes, err := c.st.ListNodes(); err == nil {
			c.m = map[int]string{}
			for _, n := range nodes {
				c.m[n.ID] = n.Region
			}
			c.at = time.Now()
		}
	}
	return c.m[nodeID]
}
