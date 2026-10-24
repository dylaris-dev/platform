package services

import (
	"context"
	"fmt"
	"log"
	"strconv"
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
}

// JudgeDataTraffic is the pure decision: no allowance means nothing to judge,
// consent means billed rather than throttled.
func JudgeDataTraffic(usedBytes int64, includedGB *int64, units int64, consent bool) DataThrottleVerdict {
	v := DataThrottleVerdict{UsedGB: usedBytes / 1_000_000_000, Consent: consent}
	if includedGB == nil {
		return v
	}
	v.Limited = true
	total := *includedGB * units
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
// route-only locations bought.
func TrafficUnits(b *store.UserBilling) int64 {
	var n int64
	if b == nil {
		return 0
	}
	if b.MaxNodes != nil && *b.MaxNodes > 0 {
		n += *b.MaxNodes
	}
	if b.MaxLinks != nil && *b.MaxLinks > 0 {
		n += *b.MaxLinks
	}
	return n
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
		if c.Kind == store.TrafficKindRelay {
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
	return JudgeDataTraffic(used, lim.IncludedGB, TrafficUnits(b), consent), nil
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
	for tenant, servers := range serversOf {
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
			if len(uuid) > 6 && uuid[:6] == "owner:" {
				continue // route-only subject: no server, no beam, no tabs
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
	listed, err := a.redis.SMembers(ctx, dataThrottledSet).Result()
	if err != nil {
		return
	}
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
