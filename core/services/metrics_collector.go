package services

import (
	"context"
	"database/sql"
	"dylaris-core/metrics"
	"dylaris-core/models"
	"dylaris-core/pkg/leader"
	"dylaris-core/store"
	"dylaris-pkg/protocol"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// MetricsCollector samples the numbers Core already knows and hands them to the
// long-term recorder.
//
// Sampling only. It creates no new instrumentation: every value here comes from
// something that is already published - the node heartbeats, the edge registry,
// the link keep-alives, the store. That is what lets the record start on the
// day this ships rather than after the rest of the work, and the clock is the
// point: a month of history cannot be collected retroactively.
type MetricsCollector struct {
	store store.Store
	redis *redis.Client
	db    *sql.DB
	// recorders hands out the recorder currently in use. Asked on every sample
	// rather than resolved once, because the metrics database is swappable at
	// runtime: a collector holding a recorder would keep writing to the
	// database an admin just replaced, and would keep it open for the life of
	// the process.
	recorders recorderSource
	flags     *FeatureFlags
	leader    leader.Election

	// gwStats is the live gateway telemetry, read through the consumer that
	// already holds it rather than through a second consumer group per stream.
	gwStats gatewayStatsSource

	pgCounters    *counterSource
	redisCounters *counterSource

	// Previous CPU-time reading for the Core process, so cpu_pct is a rate
	// rather than a total.
	lastCPUSeconds float64
	lastCPUAt      time.Time

	// lastNodes is the node list from this tick, so the link classifier does
	// not have to read the table a second time.
	lastNodes []models.Node

	// lastUptime is the previous UptimeSec of each gateway component. A value
	// that went DOWN means that component restarted between two samples, which
	// is the only way this record can see a restart at all - and it is what
	// makes the splice handover counters interpretable, because a handover with
	// no restart beside it is a different event from one caused by a deploy.
	lastUptime map[string]int64
}

// gatewayStatsSource is the live view of every gateway component, satisfied by
// GatewayBandwidthConsumerService. An interface so the collector can be tested
// without Redis.
type gatewayStatsSource interface {
	Snapshot() []protocol.GatewayStats
	DrainCounters() []CounterBatch
}

const (
	// Matches the cadence the gateway bandwidth consumer already samples at, so
	// the two sit on the same grid and can be read against each other.
	metricsSampleInterval = 30 * time.Second

	// The slow lane. Everything on it is either expensive to compute or moves
	// over days rather than seconds: a scan of every tenant monthly usage row,
	// and the on-disk size of the database. Sampling those every 30s would cost
	// far more than the resolution is worth.
	metricsSlowInterval = 5 * time.Minute
)

// MetricsEnabledSetting is the switch. Default OFF: recording a year of history
// is a decision an operator makes, not something that starts because the
// software supports it.
const MetricsEnabledSetting = "feature_metrics_enabled"

// recorderSource is the collector's view of the metrics manager. An interface
// so a test can hand over one fixed recorder (see fixedRecorder) without
// building a manager and a database behind it.
type recorderSource interface {
	Recorder() *metrics.Recorder
}

// fixedRecorder adapts a single recorder to that interface.
type fixedRecorder struct{ r *metrics.Recorder }

func (f fixedRecorder) Recorder() *metrics.Recorder { return f.r }

// NewMetricsCollector takes the SOURCE of the recorder, not a recorder. Pass a
// *metrics.Manager in production; fixedRecorder wraps a single one.
func NewMetricsCollector(s store.Store, r *redis.Client, db *sql.DB, rec recorderSource, flags *FeatureFlags) *MetricsCollector {
	return &MetricsCollector{
		store: s, redis: r, db: db, recorders: rec, flags: flags,
		pgCounters:    newCounterSource(),
		redisCounters: newCounterSource(),
		lastUptime:    map[string]int64{},
	}
}

// SetGatewayStats wires the live gateway telemetry view. Call once at boot;
// without it the gateway counters are simply absent and everything else is
// recorded as before.
func (c *MetricsCollector) SetGatewayStats(src gatewayStatsSource) { c.gwStats = src }

// SetLeader wires the leader-election gate. Call once at boot.
func (c *MetricsCollector) SetLeader(l leader.Election) { c.leader = l }

// Start runs the sampling loops.
//
// It does NOT start a flush loop: the manager owns that, because the flusher
// belongs to the database connection and has to end when that connection is
// retired. And it starts even with nothing open - the target can be configured
// in the panel later, and Observe on a nil recorder is a no-op until then.
func (c *MetricsCollector) Start(ctx context.Context) {
	if c.recorders == nil {
		return
	}
	log.Println("Metrics collector started")
	go c.loop(ctx)
	go c.slowLoop(ctx)
}

func (c *MetricsCollector) loop(ctx context.Context) {
	t := time.NewTicker(metricsSampleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sampleOnce(ctx)
		}
	}
}

func (c *MetricsCollector) slowLoop(ctx context.Context) {
	t := time.NewTicker(metricsSlowInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sampleSlow(ctx)
		}
	}
}

// sampleSlow takes the expensive readings. Gated identically to sampleOnce -
// the gate is repeated rather than shared because a second entry point that
// forgot it would record from every replica, which is how a per-replica counter
// once billed every customer twice.
func (c *MetricsCollector) sampleSlow(ctx context.Context) {
	if c.leader != nil && !c.leader.IsLeader() {
		return
	}
	if c.flags != nil && !c.flags.Get(ctx, MetricsEnabledSetting, false) {
		return
	}
	now := time.Now()
	c.sampleUserTraffic(now)
	c.sampleDatabaseSize(ctx, c.db, now)
}

// sampleOnce takes one reading of everything.
//
// Leader-gated. Core runs several replicas and each one sees the same fleet, so
// without this every replica would fold the same numbers into the same bucket
// and a two-replica install would report twice the players. The counter that
// bills tenants was once wrong in exactly this way.
func (c *MetricsCollector) sampleOnce(ctx context.Context) {
	// Drained BEFORE both gates, and this ordering is the whole correctness of
	// it. The counters accumulate in the consumer, which every replica runs; a
	// replica that only drained while it held the lease would hand the next
	// leader every event since its own process started, and the previous leader
	// has already recorded almost all of them. Draining unconditionally keeps
	// each replica's backlog to one tick, so a failover costs at most one
	// 30-second window rather than a duplicate of the whole uptime.
	//
	// The same argument applies to the feature gate: recording switched off must
	// not turn into a backlog that the day it is switched on reports as a single
	// enormous spike.
	batches := c.drainGatewayCounters()
	if c.leader != nil && !c.leader.IsLeader() {
		return
	}
	if c.flags != nil && !c.flags.Get(ctx, MetricsEnabledSetting, false) {
		return
	}
	now := time.Now()
	c.recordGatewayCounters(batches, now)
	c.samplePlatform(ctx, now)
	c.sampleGateway(ctx, now)
	c.sampleGatewayComponents(now)
	c.sampleSelf(now)
	c.samplePostgres(ctx, c.db, now)
	c.sampleRedis(ctx, now)
	c.samplePresence(ctx, now)
}

// drainGatewayCounters takes the counted events waiting in the consumer, or
// nothing when no telemetry view is wired.
func (c *MetricsCollector) drainGatewayCounters() []CounterBatch {
	if c.gwStats == nil {
		return nil
	}
	return c.gwStats.DrainCounters()
}

// recordGatewayCounters turns each component's counted events into series.
//
// Separate from the gauges below because the two are read differently: a gauge
// is sampled (the newest reading is the answer) and a counter is SUMMED (every
// publish between two ticks is an event that happened). Folding them into one
// loop is what made every counter series a tenth of the truth.
func (c *MetricsCollector) recordGatewayCounters(batches []CounterBatch, now time.Time) {
	for _, b := range batches {
		if b.Component == "" || b.ID == "" {
			continue
		}
		accepted := 0
		for name, v := range b.Counters {
			if accepted >= protocol.MaxCustomMetrics || !protocol.ValidMetricName(name) {
				continue
			}
			accepted++
			c.obs(b.Component+"."+name, b.ID, b.Region, float64(v), now)
		}
	}
}

// sampleGatewayComponents records the gauges every gateway component publishes
// about itself, plus the restarts derived from their uptime. The counters take
// the separate path above.
//
// The names are the component own names, prefixed with the component: a gauge
// "active_sessions" from the splice becomes splice.active_sessions. That is what
// lets a component add a measurement without either repository changing the
// shared telemetry contract - and it is why the names are validated rather than
// trusted. Each distinct name is a stored series that outlives the process that
// published it, so a producer folding a session id or an address into one would
// grow the store with TRAFFIC instead of with code.
func (c *MetricsCollector) sampleGatewayComponents(now time.Time) {
	if c.gwStats == nil {
		return
	}
	for _, gs := range c.gwStats.Snapshot() {
		if gs.Component == "" || gs.ID == "" {
			continue
		}
		accepted := 0
		for name, v := range gs.Gauges {
			if accepted >= protocol.MaxCustomMetrics || !protocol.ValidMetricName(name) {
				continue
			}
			accepted++
			c.obs(gs.Component+"."+name, gs.ID, gs.Region, v, now)
		}
		c.recordSystemLoad(gs, now)
		c.recordRestart(gs, now)
	}
}

// recordSystemLoad records what a gateway component costs its machine.
//
// CPU and RAM are TYPED fields on the record rather than entries in the Gauges
// map, which is why they never reached the long-term store: the loop above
// walks Counters and Gauges, and these two are neither. Measured 2026-09-03,
// the metrics database held edge.cpu_pct and edge.ram_pct and NOTHING for warp
// or beam - not a decision, just the gap this closes.
//
// Two guards, and both are the difference between a series and a lie:
//
//   - A component that does not measure its machine at all publishes 0 for
//     both. The splice and the link do exactly that. Recording them would put
//     a permanent flat zero into the record that reads like an idle machine
//     rather than like an absent measurement. RAM is the discriminator and CPU
//     cannot be: a running box never reports 0% memory used, while 0% CPU is an
//     ordinary quiet second.
//   - Edges are skipped HERE because sampleGateway already records them from
//     the edge list, which is a different source with its own view of which
//     edges exist. Recording both would fold two readings of one machine into
//     one bucket and inflate its sample count.
func (c *MetricsCollector) recordSystemLoad(gs protocol.GatewayStats, now time.Time) {
	if gs.Component == "edge" {
		// Recorded by sampleGateway from the edge list, which is a different
		// source with its own view of which edges exist. Doing it here as well
		// would fold two readings of one machine into one bucket.
		return
	}
	if gs.RAMPct > 0 {
		c.obs(gs.Component+".cpu_pct", gs.ID, gs.Region, gs.CPU, now)
		c.obs(gs.Component+".ram_pct", gs.ID, gs.Region, gs.RAMPct, now)
	}
	// Throughput, for the components that carry any. Same gap as CPU and RAM
	// above and closed for the same reason: RxBps and TxBps are typed fields on
	// the record rather than entries in its Gauges map, so the loop that turns a
	// component's own numbers into series walked past them. The catalog has
	// listed beam.rx_bps and beam.tx_bps since the day it was written and
	// nothing has ever produced them.
	//
	// carriesThroughput is the same predicate the bandwidth view uses, and the
	// same reasoning: the splice shares a namespace with an edge that already
	// reports every byte, and the link ships without a system monitor. Both
	// would record a permanent flat zero.
	if carriesThroughput(gs.Component) {
		c.obs(gs.Component+".rx_bps", gs.ID, gs.Region, float64(gs.RxBps), now)
		c.obs(gs.Component+".tx_bps", gs.ID, gs.Region, float64(gs.TxBps), now)
	}
}

// recordRestart turns a component uptime into a restart COUNT.
//
// An uptime lower than the previous reading is the restart: nothing else in the
// system reports one. It is recorded as 1 at the moment it is noticed, so
// summing the series over a window gives the number of restarts in it. A
// component seen for the first time records nothing - Core restarting must not
// be reported as every gateway component restarting.
func (c *MetricsCollector) recordRestart(gs protocol.GatewayStats, now time.Time) {
	if gs.UptimeSec <= 0 {
		return // an older build that does not report uptime
	}
	key := gs.Component + ":" + gs.ID
	prev, seen := c.lastUptime[key]
	c.lastUptime[key] = gs.UptimeSec
	if !seen {
		return
	}
	restarted := 0.0
	if gs.UptimeSec < prev {
		restarted = 1
	}
	c.obs(gs.Component+".restarts", gs.ID, gs.Region, restarted, now)
	c.obs(gs.Component+".uptime_sec", gs.ID, gs.Region, float64(gs.UptimeSec), now)
}

// obs is the shorthand this file uses; the recorder tolerates a nil receiver so
// no call site needs its own guard.
func (c *MetricsCollector) obs(metric, subject, region string, v float64, at time.Time) {
	c.recorders.Recorder().Observe(metrics.Key{Metric: metric, Subject: subject, Region: region}, v, at)
}

func (c *MetricsCollector) samplePlatform(ctx context.Context, now time.Time) {
	if c.store == nil {
		return
	}
	nodes, err := c.store.ListNodes()
	if err != nil {
		return
	}
	// The live figures are NOT columns on the nodes table - they arrive in the
	// node's heartbeat - so the rows have to be enriched before anything reads
	// CPU, RAM or the server count off them. Skipping this is what made
	// node.cpu_pct and node.servers a flat zero for the life of the record and
	// left node.ram_pct with no rows at all.
	withHeartbeat := EnrichNodesWithLiveStats(ctx, c.store, c.redis, nodes)
	// Kept for sampleGateway, which runs later in the same tick and needs to
	// know which links are customers'. Reading the table again there would be a
	// second query per 30 seconds for an answer already in hand.
	c.lastNodes = nodes

	// Ours and theirs, counted apart and never averaged together.
	//
	// This is the record somebody is eventually shown, and availability is the
	// number they will read hardest. A BYON machine is a customer's laptop or
	// their spare box; switching it off at night is not downtime of this
	// platform, and folding it into `node.up` would understate the uptime this
	// fleet actually delivered - with somebody else's decisions.
	//
	// Their machines are still recorded, as a count and an online count. That
	// is a real number about the business (how much hardware customers brought)
	// and it cannot contaminate an availability figure, because it is a gauge
	// of its own rather than a per-machine `up` series the summary averages.
	var online, platform, external int
	var byon, byonOnline int
	for i := range nodes {
		n := &nodes[i]
		up := n.Status == "online"
		if n.Kind() == models.NodeKindBYON {
			byon++
			if up {
				byonOnline++
			}
			continue
		}
		if up {
			online++
		}
		if n.Kind() == models.NodeKindExternal {
			external++
		} else {
			platform++
		}
		c.sampleNode(n, withHeartbeat[n.Token], now)
	}

	// platform.nodes is OUR fleet: platform + external. It is what
	// platform.nodes_online is a fraction of, so the two have to describe the
	// same set or the ratio is meaningless.
	c.obs("platform.nodes", "", "", float64(platform+external), now)
	c.obs("platform.nodes_online", "", "", float64(online), now)
	c.obs("platform.nodes_platform", "", "", float64(platform), now)
	c.obs("platform.nodes_external", "", "", float64(external), now)
	c.obs("platform.nodes_byon", "", "", float64(byon), now)
	c.obs("platform.nodes_byon_online", "", "", float64(byonOnline), now)

	if users, err := c.store.CountUsers(); err == nil {
		c.obs("platform.users", "", "", float64(users), now)
	}

	servers, err := c.store.ListServers("")
	if err == nil {
		var up int
		for i := range servers {
			if servers[i].Status == "online" {
				up++
			}
		}
		c.obs("platform.servers", "", "", float64(len(servers)), now)
		c.obs("platform.servers_online", "", "", float64(up), now)
	}
}

// sampleNode records one machine's live figures.
//
// `node.up` is 1 or 0, and it is the series that answers "what was the uptime".
// A boolean averaged over a bucket IS the availability fraction, which is why
// it is recorded as a number rather than inferred later from gaps - a gap is
// ambiguous between "down" and "nothing was sampling".
func (c *MetricsCollector) sampleNode(n *models.Node, hasHeartbeat bool, now time.Time) {
	up := 0.0
	if n.Status == "online" {
		up = 1
	}
	c.obs("node.up", n.Token, n.Region, up, now)
	if up == 0 {
		// CPU and RAM from an offline node are the last values seen, not
		// current ones. Recording them would put a flatline into the average
		// that reads like a healthy idle machine.
		return
	}
	// No heartbeat is not a reading of zero, and the struct cannot tell them
	// apart: these fields are not persisted, so an unenriched or unreported node
	// carries the zero value. The guard below is `>= 0` because the documented
	// sentinel for "not available" is -1 - which a zero passes, so without this
	// an absent heartbeat recorded a machine at 0% CPU running 0 servers.
	if !hasHeartbeat {
		return
	}
	if n.CPUUsage >= 0 {
		c.obs("node.cpu_pct", n.Token, n.Region, n.CPUUsage, now)
	}
	if n.RAMTotal > 0 {
		used := float64(n.RAMTotal) - float64(n.RAMFree)
		c.obs("node.ram_used_bytes", n.Token, n.Region, used, now)
		c.obs("node.ram_pct", n.Token, n.Region, used/float64(n.RAMTotal)*100, now)
	}
	c.obs("node.servers", n.Token, n.Region, float64(n.ServerCount), now)
}

// linkOwnership is the classifier for this tick, built from the nodes
// samplePlatform already listed. Empty when nothing has been sampled yet, which
// classifies every link as ours - the same conservative direction the handlers
// take, because over-reporting our own fleet can only raise an alarm somebody
// dismisses while the opposite hides a real outage of ours.
func (c *MetricsCollector) linkOwnership() LinkOwnership {
	return LinkOwnershipFrom(c.store, c.lastNodes)
}

func (c *MetricsCollector) sampleGateway(ctx context.Context, now time.Time) {
	if c.redis == nil {
		return
	}
	edges := GetEdgesFromRedis(ctx, c.redis)
	var onlineEdges int
	var players int64
	// In BITS per second, like every other throughput figure here. The edge
	// reports bytes (see the conversion below), so the accumulation converts
	// too - the platform totals are the same reading as the per-edge series and
	// must not disagree with it by a factor of eight.
	var totalRxBits, totalTxBits uint64
	for _, e := range edges {
		up := 0.0
		if e.Status == "online" {
			up = 1
			onlineEdges++
		}
		c.obs("edge.up", e.EdgeID, e.Region, up, now)
		if up == 0 || e.Stats == nil {
			continue
		}
		s := e.Stats
		c.obs("edge.cpu_pct", e.EdgeID, e.Region, s.CPU, now)
		c.obs("edge.ram_pct", e.EdgeID, e.Region, s.RAMPercent, now)
		// TIMES EIGHT, and it is not a fudge. RxSpeed/TxSpeed come from the
		// edge's legacy `rx_speed`/`tx_speed` fields, which are BYTES per
		// second; the metric is named _bps and the catalog declares it UnitBps,
		// so recording them raw stored an eighth of the truth under a name that
		// said otherwise. Measured against gateway_bandwidth_stats over the
		// same window on 2026-09-03: peak 7541 here against 60328 there, an
		// exact factor of 8 on both edges.
		//
		// Everything else on this platform - the live view, the alerts, the
		// per-component series below - is bits per second, which is why the
		// conversion belongs here rather than the name changing.
		c.obs("edge.rx_bps", e.EdgeID, e.Region, float64(s.RxSpeed)*8, now)
		c.obs("edge.tx_bps", e.EdgeID, e.Region, float64(s.TxSpeed)*8, now)
		c.obs("edge.players", e.EdgeID, e.Region, float64(s.ActiveMCStreams), now)
		players += s.ActiveMCStreams
		totalRxBits += s.RxSpeed * 8
		totalTxBits += s.TxSpeed * 8
	}
	if len(edges) > 0 {
		c.obs("platform.edges", "", "", float64(len(edges)), now)
		c.obs("platform.edges_online", "", "", float64(onlineEdges), now)
		// The players number the whole record is built around. It comes from
		// the edges because that is where a connection actually terminates.
		c.obs("platform.players", "", "", float64(players), now)
		c.obs("platform.player_rx_bps", "", "", float64(totalRxBits), now)
		c.obs("platform.player_tx_bps", "", "", float64(totalTxBits), now)
		// What ONE player costs in bandwidth, sampled rather than divided later:
		// a quotient of two averages is not the average of the quotient, so
		// dividing the monthly totals at read time would give a number that is
		// close and wrong. Recorded per sample, the bucket min/max are then the
		// real quietest and busiest player load seen in that window.
		//
		// Both directions together, which is what makes it a cost figure rather
		// than a capacity figure: a link's rated speed applies to each
		// direction on its own, so this number must never be used to ask how
		// many players fit on an uplink. The label says "in + out" for that
		// reason, and player_tx_bps is the one to size against.
		if players > 0 {
			c.obs("platform.bps_per_player", "", "", float64(totalRxBits+totalTxBits)/float64(players), now)
		}
	}

	// Same split for links, and for the same reason: a route-only customer
	// rebooting their router is not an outage of ours.
	links := GetLinksFromRedis(ctx, c.redis)
	if len(links) > 0 {
		split := c.linkOwnership().SplitLinks(links)
		c.obs("platform.links", "", "", float64(len(split.Ours)), now)
		c.obs("platform.links_online", "", "", float64(split.OursOnline), now)
		c.obs("platform.links_customer", "", "", float64(len(split.Customer)), now)
		c.obs("platform.links_customer_online", "", "", float64(split.CustomerOnline), now)
	}

	if routes := CountRoutesFromRedis(ctx, c.redis); routes >= 0 {
		c.obs("platform.routes", "", "", float64(routes), now)
	}
}
