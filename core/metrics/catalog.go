package metrics

// The catalog is the list of series this platform records, written down in
// code rather than derived from the table.
//
// Deriving it would mean a DISTINCT over every row on every page load, and
// Postgres has no skip scan - so the cost of listing twenty names would grow
// with a year of samples. Writing it down is also more honest for a picker: a
// series that has been recording since March and one that starts today both
// appear, and an empty chart says "nothing happened yet" instead of the
// metric not existing.
//
// Kind decides how a series is re-aggregated over a longer window, and getting
// it wrong is not cosmetic. A COUNTER is summed (twelve buckets of handovers
// make an hourly total); a GAUGE is averaged (twelve buckets of CPU make an
// hourly average, and summing them would report 1200%).
type Kind string

const (
	// KindCounter: how many times something happened. Summed over a window.
	KindCounter Kind = "counter"
	// KindGauge: what was true at the instant of the sample. Averaged.
	KindGauge Kind = "gauge"
)

type Unit string

const (
	UnitCount   Unit = "count"
	UnitBytes   Unit = "bytes"
	UnitBps     Unit = "bps"
	UnitPercent Unit = "percent"
	UnitSeconds Unit = "seconds"
)

// Series describes one recorded metric.
type Series struct {
	Metric string `json:"metric"`
	Label  string `json:"label"`
	Group  string `json:"group"`
	Kind   Kind   `json:"kind"`
	Unit   Unit   `json:"unit"`
	// Help is the one thing a reader needs to know that the name does not say.
	// Empty where the name is genuinely self-explaining.
	Help string `json:"help,omitempty"`
	// PerSubject is true where the series is recorded once per component, so a
	// chart over it has to say whether it is showing one component or the fleet.
	PerSubject bool `json:"perSubject,omitempty"`
}

// Catalog is every series, in the order a reader should meet them.
var Catalog = []Series{
	// Platform
	{Metric: "platform.players", Label: "Players online", Group: "Platform", Kind: KindGauge, Unit: UnitCount,
		Help: "Counted at the edges, where a player connection actually terminates."},
	{Metric: "platform.concurrent_users", Label: "Concurrent panel users", Group: "Platform", Kind: KindGauge, Unit: UnitCount,
		Help: "People holding a live panel session, counted once each however many tabs they have open."},
	{Metric: "platform.panel_streams", Label: "Panel sessions", Group: "Platform", Kind: KindGauge, Unit: UnitCount},
	{Metric: "platform.users", Label: "Registered users", Group: "Platform", Kind: KindGauge, Unit: UnitCount},
	{Metric: "platform.servers", Label: "Servers", Group: "Platform", Kind: KindGauge, Unit: UnitCount},
	{Metric: "platform.servers_online", Label: "Servers running", Group: "Platform", Kind: KindGauge, Unit: UnitCount},
	{Metric: "platform.core_replicas", Label: "Core replicas serving", Group: "Platform", Kind: KindGauge, Unit: UnitCount},

	// Machines
	{Metric: "platform.nodes", Label: "Nodes", Group: "Machines", Kind: KindGauge, Unit: UnitCount,
		Help: "Your own fleet: cluster plus external. Customer machines are counted separately."},
	{Metric: "platform.nodes_online", Label: "Nodes online", Group: "Machines", Kind: KindGauge, Unit: UnitCount},
	{Metric: "platform.nodes_platform", Label: "Cluster nodes", Group: "Machines", Kind: KindGauge, Unit: UnitCount},
	{Metric: "platform.nodes_external", Label: "External nodes", Group: "Machines", Kind: KindGauge, Unit: UnitCount},
	{Metric: "platform.nodes_byon", Label: "BYON nodes", Group: "Machines", Kind: KindGauge, Unit: UnitCount,
		Help: "Hardware customers brought. Counted, and deliberately kept out of every availability figure."},
	{Metric: "platform.nodes_byon_online", Label: "BYON nodes online", Group: "Machines", Kind: KindGauge, Unit: UnitCount},
	{Metric: "node.up", Label: "Node availability", Group: "Machines", Kind: KindGauge, Unit: UnitPercent, PerSubject: true,
		Help: "1 while one of YOUR nodes is online, 0 while it is not. Averaged over a window this IS the uptime fraction. Customer machines are excluded, so a tenant powering their box down cannot understate it."},
	{Metric: "node.cpu_pct", Label: "Node CPU", Group: "Machines", Kind: KindGauge, Unit: UnitPercent, PerSubject: true},
	{Metric: "node.ram_pct", Label: "Node RAM", Group: "Machines", Kind: KindGauge, Unit: UnitPercent, PerSubject: true},
	{Metric: "node.ram_used_bytes", Label: "Node RAM used", Group: "Machines", Kind: KindGauge, Unit: UnitBytes, PerSubject: true},
	{Metric: "node.servers", Label: "Servers per node", Group: "Machines", Kind: KindGauge, Unit: UnitCount, PerSubject: true},

	// Traffic
	{Metric: "platform.player_rx_bps", Label: "Player traffic in", Group: "Traffic", Kind: KindGauge, Unit: UnitBps},
	{Metric: "platform.player_tx_bps", Label: "Player traffic out", Group: "Traffic", Kind: KindGauge, Unit: UnitBps},
	{Metric: "platform.bps_per_player", Label: "Traffic per player (in + out)", Group: "Traffic", Kind: KindGauge, Unit: UnitBps,
		Help: "Both directions added together, so this is what a player costs, not what an uplink can hold - a link carries its rated speed each way at the same time. Size against \"Player traffic out\". Sampled per reading rather than divided afterwards, so the minimum and maximum are real observations."},
	{Metric: "platform.user_traffic_total_bytes", Label: "Customer traffic this month", Group: "Traffic", Kind: KindGauge, Unit: UnitBytes},
	{Metric: "platform.user_traffic_avg_bytes", Label: "Average per customer", Group: "Traffic", Kind: KindGauge, Unit: UnitBytes},
	{Metric: "platform.user_traffic_min_bytes", Label: "Lightest customer", Group: "Traffic", Kind: KindGauge, Unit: UnitBytes},
	{Metric: "platform.user_traffic_max_bytes", Label: "Heaviest customer", Group: "Traffic", Kind: KindGauge, Unit: UnitBytes},
	{Metric: "platform.billed_users", Label: "Customers with usage", Group: "Traffic", Kind: KindGauge, Unit: UnitCount},

	// Edge
	{Metric: "platform.edges", Label: "Edges", Group: "Edge", Kind: KindGauge, Unit: UnitCount},
	{Metric: "platform.edges_online", Label: "Edges online", Group: "Edge", Kind: KindGauge, Unit: UnitCount},
	{Metric: "edge.up", Label: "Edge availability", Group: "Edge", Kind: KindGauge, Unit: UnitPercent, PerSubject: true},
	{Metric: "edge.players", Label: "Players per edge", Group: "Edge", Kind: KindGauge, Unit: UnitCount, PerSubject: true},
	{Metric: "edge.rx_bps", Label: "Edge traffic in", Group: "Edge", Kind: KindGauge, Unit: UnitBps, PerSubject: true},
	{Metric: "edge.tx_bps", Label: "Edge traffic out", Group: "Edge", Kind: KindGauge, Unit: UnitBps, PerSubject: true},
	{Metric: "edge.cpu_pct", Label: "Edge CPU", Group: "Edge", Kind: KindGauge, Unit: UnitPercent, PerSubject: true},
	{Metric: "edge.ram_pct", Label: "Edge RAM", Group: "Edge", Kind: KindGauge, Unit: UnitPercent, PerSubject: true},
	{Metric: "edge.tunnels", Label: "Link tunnels per edge", Group: "Edge", Kind: KindGauge, Unit: UnitCount, PerSubject: true},
	{Metric: "edge.restarts", Label: "Edge restarts", Group: "Edge", Kind: KindCounter, Unit: UnitCount, PerSubject: true},
	{Metric: "edge.uptime_sec", Label: "Edge uptime", Group: "Edge", Kind: KindGauge, Unit: UnitSeconds, PerSubject: true},

	// The kernel-level shield. The edge has always published these and nothing
	// had ever read them, so the platform could neither say whether the filter
	// was loaded nor what it had absorbed.
	{Metric: "edge.xdp_up", Label: "DDoS shield loaded", Group: "Edge", Kind: KindGauge, Unit: UnitCount, PerSubject: true,
		Help: "1 while the edge has the XDP filter attached, 0 while it does not. Averaged over a window this is the fraction of the time that edge was actually protected."},
	{Metric: "edge.xdp_passed", Label: "Packets passed by the shield", Group: "Edge", Kind: KindCounter, Unit: UnitCount, PerSubject: true},
	{Metric: "edge.xdp_dropped_blocked", Label: "Packets dropped, blocked address", Group: "Edge", Kind: KindCounter, Unit: UnitCount, PerSubject: true},
	{Metric: "edge.xdp_dropped_ratelimit", Label: "Packets dropped, rate limit", Group: "Edge", Kind: KindCounter, Unit: UnitCount, PerSubject: true},
	{Metric: "edge.xdp_blocked_ips", Label: "Addresses currently blocked", Group: "Edge", Kind: KindGauge, Unit: UnitCount, PerSubject: true},

	// Splice - the handover record
	{Metric: "splice.sessions_opened", Label: "Player sessions opened", Group: "Handover", Kind: KindCounter, Unit: UnitCount, PerSubject: true},
	{Metric: "splice.handover_attempted", Label: "Handovers attempted", Group: "Handover", Kind: KindCounter, Unit: UnitCount, PerSubject: true,
		Help: "A player was mid-session when their edge went away, so another edge had to take them over."},
	{Metric: "splice.handover_ok", Label: "Players carried over", Group: "Handover", Kind: KindCounter, Unit: UnitCount, PerSubject: true,
		Help: "The player kept playing through an edge restart without noticing."},
	{Metric: "splice.players_dropped", Label: "Players dropped", Group: "Handover", Kind: KindCounter, Unit: UnitCount, PerSubject: true,
		Help: "No edge could complete the takeover, so the player was disconnected."},
	{Metric: "splice.resume_refused", Label: "Resumes refused", Group: "Handover", Kind: KindCounter, Unit: UnitCount, PerSubject: true,
		Help: "The stream could no longer be reconstructed, so no edge was going to succeed."},
	{Metric: "splice.splice_back", Label: "Returns to local edge", Group: "Handover", Kind: KindCounter, Unit: UnitCount, PerSubject: true,
		Help: "A deliberate move back once the local edge recovered, not a failure."},
	{Metric: "splice.active_sessions", Label: "Sessions through the splice", Group: "Handover", Kind: KindGauge, Unit: UnitCount, PerSubject: true},
	{Metric: "splice.pool_edges", Label: "Siblings available", Group: "Handover", Kind: KindGauge, Unit: UnitCount, PerSubject: true,
		Help: "How many other edges a handover could reach. Zero here explains a drop that was not the splice's doing."},

	// Warp
	{Metric: "warp.peers", Label: "Overlay peers", Group: "Warp", Kind: KindGauge, Unit: UnitCount, PerSubject: true},
	{Metric: "warp.peers_active", Label: "Overlay tunnels up", Group: "Warp", Kind: KindGauge, Unit: UnitCount, PerSubject: true,
		Help: "Peers with a recent handshake. The gap to configured peers is the outage."},
	{Metric: "warp.rx_bps", Label: "Warp leader in", Group: "Warp", Kind: KindGauge, Unit: UnitBps, PerSubject: true},
	{Metric: "warp.tx_bps", Label: "Warp leader out", Group: "Warp", Kind: KindGauge, Unit: UnitBps, PerSubject: true},
	{Metric: "warp.cpu_pct", Label: "Warp leader CPU", Group: "Warp", Kind: KindGauge, Unit: UnitPercent, PerSubject: true},
	{Metric: "warp.ram_pct", Label: "Warp leader RAM", Group: "Warp", Kind: KindGauge, Unit: UnitPercent, PerSubject: true},
	{Metric: "warp.restarts", Label: "Warp leader restarts", Group: "Warp", Kind: KindCounter, Unit: UnitCount, PerSubject: true},

	// Link
	{Metric: "link.active_tunnels", Label: "Link tunnels up", Group: "Link", Kind: KindGauge, Unit: UnitCount, PerSubject: true},
	{Metric: "link.tunnels_established", Label: "Tunnels established", Group: "Link", Kind: KindCounter, Unit: UnitCount, PerSubject: true},
	{Metric: "link.tunnels_lost", Label: "Tunnels lost", Group: "Link", Kind: KindCounter, Unit: UnitCount, PerSubject: true},
	{Metric: "link.dial_failures", Label: "Link dial failures", Group: "Link", Kind: KindCounter, Unit: UnitCount, PerSubject: true},
	{Metric: "link.handshake_failures", Label: "Link handshake failures", Group: "Link", Kind: KindCounter, Unit: UnitCount, PerSubject: true},
	{Metric: "link.replay_gap_peak_bytes", Label: "Largest replay gap", Group: "Link", Kind: KindGauge, Unit: UnitBytes, PerSubject: true,
		Help: "The most a resume ever had to replay on this link. Compare it against the ring below: the ring is a fixed 1 MB held for EVERY player session, so a peak far under it means that memory is being reserved for nothing."},
	{Metric: "link.replay_ring_bytes", Label: "Replay ring size", Group: "Link", Kind: KindGauge, Unit: UnitBytes, PerSubject: true,
		Help: "What the link reserves per session to carry a player across an edge restart. Published so the peak above can be read without knowing the constant."},
	{Metric: "link.resumes_served", Label: "Resumes carried", Group: "Link", Kind: KindCounter, Unit: UnitCount, PerSubject: true},
	{Metric: "link.resumes_refused", Label: "Resumes refused", Group: "Link", Kind: KindCounter, Unit: UnitCount, PerSubject: true,
		Help: "The ring could not cover the gap, so the session was ended rather than resumed with a hole in it. Any of these means the ring is too small, not too large."},
	{Metric: "platform.links", Label: "Links", Group: "Link", Kind: KindGauge, Unit: UnitCount,
		Help: "Links you run. A customer's BYON or route-only link is counted separately."},
	{Metric: "platform.links_online", Label: "Links online", Group: "Link", Kind: KindGauge, Unit: UnitCount},
	{Metric: "platform.links_customer", Label: "Customer links", Group: "Link", Kind: KindGauge, Unit: UnitCount},
	{Metric: "platform.links_customer_online", Label: "Customer links online", Group: "Link", Kind: KindGauge, Unit: UnitCount},
	{Metric: "platform.routes", Label: "Routes", Group: "Link", Kind: KindGauge, Unit: UnitCount},

	// Beam
	{Metric: "beam.active_transfers", Label: "Beam transfers in flight", Group: "Beam", Kind: KindGauge, Unit: UnitCount, PerSubject: true},
	{Metric: "beam.transfers_started", Label: "Beam transfers started", Group: "Beam", Kind: KindCounter, Unit: UnitCount, PerSubject: true},
	{Metric: "beam.transfers_failed", Label: "Beam transfers refused", Group: "Beam", Kind: KindCounter, Unit: UnitCount, PerSubject: true,
		Help: "Reached the relay and did not get through: a bad ticket, no tunnel to the node, or a failed stream."},
	{Metric: "beam.tunnels", Label: "Beam relay tunnels", Group: "Beam", Kind: KindGauge, Unit: UnitCount, PerSubject: true},
	{Metric: "beam.cpu_pct", Label: "Beam relay CPU", Group: "Beam", Kind: KindGauge, Unit: UnitPercent, PerSubject: true},
	{Metric: "beam.ram_pct", Label: "Beam relay RAM", Group: "Beam", Kind: KindGauge, Unit: UnitPercent, PerSubject: true},
	{Metric: "beam.rx_bps", Label: "Beam relay in", Group: "Beam", Kind: KindGauge, Unit: UnitBps, PerSubject: true},
	{Metric: "beam.tx_bps", Label: "Beam relay out", Group: "Beam", Kind: KindGauge, Unit: UnitBps, PerSubject: true},

	// Core and its databases
	{Metric: "core.cpu_pct", Label: "Core CPU", Group: "Services", Kind: KindGauge, Unit: UnitPercent,
		Help: "Percentage of one core, averaged over the interval between samples."},
	{Metric: "core.rss_bytes", Label: "Core memory", Group: "Services", Kind: KindGauge, Unit: UnitBytes},
	{Metric: "core.heap_bytes", Label: "Core heap", Group: "Services", Kind: KindGauge, Unit: UnitBytes},
	{Metric: "core.goroutines", Label: "Core goroutines", Group: "Services", Kind: KindGauge, Unit: UnitCount},
	{Metric: "postgres.backends", Label: "Postgres connections", Group: "Services", Kind: KindGauge, Unit: UnitCount},
	{Metric: "postgres.commits", Label: "Postgres commits", Group: "Services", Kind: KindCounter, Unit: UnitCount},
	{Metric: "postgres.rollbacks", Label: "Postgres rollbacks", Group: "Services", Kind: KindCounter, Unit: UnitCount},
	{Metric: "postgres.blocks_read", Label: "Postgres blocks from disk", Group: "Services", Kind: KindCounter, Unit: UnitCount},
	{Metric: "postgres.blocks_hit", Label: "Postgres blocks from cache", Group: "Services", Kind: KindCounter, Unit: UnitCount},
	{Metric: "postgres.rows_returned", Label: "Postgres rows scanned", Group: "Services", Kind: KindCounter, Unit: UnitCount},
	{Metric: "postgres.rows_fetched", Label: "Postgres rows returned", Group: "Services", Kind: KindCounter, Unit: UnitCount,
		Help: "Rows actually handed back, against the rows scanned to find them - the gap is index quality."},
	{Metric: "postgres.rows_inserted", Label: "Postgres rows inserted", Group: "Services", Kind: KindCounter, Unit: UnitCount},
	{Metric: "postgres.rows_updated", Label: "Postgres rows updated", Group: "Services", Kind: KindCounter, Unit: UnitCount},
	{Metric: "postgres.rows_deleted", Label: "Postgres rows deleted", Group: "Services", Kind: KindCounter, Unit: UnitCount},
	{Metric: "postgres.deadlocks", Label: "Postgres deadlocks", Group: "Services", Kind: KindCounter, Unit: UnitCount},
	{Metric: "postgres.size_bytes", Label: "Database size", Group: "Services", Kind: KindGauge, Unit: UnitBytes},
	{Metric: "redis.clients", Label: "Redis clients", Group: "Services", Kind: KindGauge, Unit: UnitCount},
	{Metric: "redis.memory_bytes", Label: "Redis memory", Group: "Services", Kind: KindGauge, Unit: UnitBytes},
	{Metric: "redis.ops_per_sec", Label: "Redis operations", Group: "Services", Kind: KindGauge, Unit: UnitCount},
	{Metric: "redis.commands", Label: "Redis commands", Group: "Services", Kind: KindCounter, Unit: UnitCount},
	{Metric: "redis.rx_bytes", Label: "Redis bytes in", Group: "Services", Kind: KindCounter, Unit: UnitBytes},
	{Metric: "redis.tx_bytes", Label: "Redis bytes out", Group: "Services", Kind: KindCounter, Unit: UnitBytes},
	{Metric: "redis.keyspace_hits", Label: "Redis cache hits", Group: "Services", Kind: KindCounter, Unit: UnitCount},
	{Metric: "redis.keyspace_misses", Label: "Redis cache misses", Group: "Services", Kind: KindCounter, Unit: UnitCount},
	{Metric: "redis.expired_keys", Label: "Redis keys expired", Group: "Services", Kind: KindCounter, Unit: UnitCount},
	{Metric: "redis.evicted_keys", Label: "Redis keys evicted", Group: "Services", Kind: KindCounter, Unit: UnitCount,
		Help: "Anything but zero means Redis is discarding data to stay under its memory limit."},
}

// catalogByMetric indexes Catalog. Built once; the catalog is a constant.
var catalogByMetric = func() map[string]Series {
	m := make(map[string]Series, len(Catalog))
	for _, s := range Catalog {
		m[s.Metric] = s
	}
	return m
}()

// Known returns the catalog entry for a metric.
//
// A metric NOT in the catalog is still readable - the gateway contract lets a
// component publish a name this build has never heard of, and refusing to chart
// it would make new telemetry invisible until Core was rebuilt. It is treated
// as a gauge, which is the safer default: averaging a counter understates it,
// while summing a gauge produces a number with no meaning at all.
func Known(metric string) (Series, bool) {
	s, ok := catalogByMetric[metric]
	if !ok {
		return Series{Metric: metric, Label: metric, Group: "Other", Kind: KindGauge, Unit: UnitCount}, false
	}
	return s, true
}
