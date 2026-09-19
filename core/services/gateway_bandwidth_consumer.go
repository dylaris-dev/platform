package services

import (
	"context"
	"dylaris-core/models"
	"dylaris-core/pkg/leader"
	"dylaris-core/store"
	"dylaris-pkg/protocol"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// GatewayBandwidthConsumerService ingests the edge/warp/beam telemetry streams,
// keeps the latest per-component sample in memory, aggregates per swarm host,
// mirrors the view to Redis for the panel, and persists one downsampled row per
// component per persistInterval. Leader-gated: only one Core persists + mirrors.
type GatewayBandwidthConsumerService struct {
	store  store.Store
	redis  *redis.Client
	coreID string
	leader leader.Election

	mu      sync.Mutex
	latest  map[string]seenStat // keyed by component ID
	pending map[string]*CounterBatch
	streams map[string]bool // stream keys already being consumed
}

// CounterBatch is everything one component COUNTED since the last drain.
//
// It exists because a counter and a gauge cannot be read the same way. A gauge
// is an instant ("2 peers right now"), so sampling the newest one is correct. A
// counter arrives as a DELTA - "events since my last publish" - and the
// components publish one every 3 seconds while the metrics collector samples
// every 30, so keeping only the newest threw nine out of every ten away.
//
// Measured in production on 2026-09-03: the two splices logged 8 dropped
// players between 13:25 and 17:00 UTC and the long-term record held 1. Four
// headline figures are built on these series ("Player sessions carried",
// "Players carried through an edge restart", "Players dropped in a handover",
// "Beam transfers"), so all four read roughly a tenth of the truth.
type CounterBatch struct {
	Component string
	ID        string
	Region    string
	Counters  map[string]int64
}

// seenStat pairs a component's latest sample with the Core receive time, so
// persistOnce can prune a component that stopped publishing without relying
// on the publisher's own clock (skew-immune).
type seenStat struct {
	gs   protocol.GatewayStats
	seen time.Time
}

const (
	gwbwGroup           = "dylaris-core-gwbw"
	gwbwPersistInterval = 30 * time.Second // downsample cadence
	gwbwScanInterval    = 30 * time.Second
	gwbwMirrorTTL       = 90 * time.Second // > persist interval so a stale host still shows briefly
	gwbwStaleAfter      = 90 * time.Second // a component missing this long is treated as gone
)

func NewGatewayBandwidthConsumerService(s store.Store, r *redis.Client, coreID string) *GatewayBandwidthConsumerService {
	return &GatewayBandwidthConsumerService{
		store:   s,
		redis:   r,
		coreID:  coreID,
		latest:  make(map[string]seenStat),
		pending: make(map[string]*CounterBatch),
		streams: make(map[string]bool),
	}
}

// SetLeader wires the leader-election gate. Call once at boot.
func (s *GatewayBandwidthConsumerService) SetLeader(l leader.Election) { s.leader = l }

func (s *GatewayBandwidthConsumerService) Start(ctx context.Context) {
	if s.redis == nil {
		return
	}
	log.Println("Gateway Bandwidth Consumer started")
	go s.scanLoop(ctx)
	go s.persistLoop(ctx)
}

// scanLoop discovers the component stats streams and starts one consumer
// goroutine per newly seen stream. Consuming is unconditional (cheap); only
// persistence + the Redis mirror are leader-gated.
func (s *GatewayBandwidthConsumerService) scanLoop(ctx context.Context) {
	s.scan(ctx)
	t := time.NewTicker(gwbwScanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.scan(ctx)
		}
	}
}

func (s *GatewayBandwidthConsumerService) scan(ctx context.Context) {
	for _, k := range s.discoverStreams(ctx) {
		s.mu.Lock()
		seen := s.streams[k]
		if !seen {
			s.streams[k] = true
		}
		s.mu.Unlock()
		if !seen {
			go s.consume(ctx, k)
		}
	}
}

// discoverStreams enumerates the edge/beam/warp components and returns their
// stats stream keys. Edges + beams live in SETs; warp leaders are known by
// their per-leader :alive liveness key.
func (s *GatewayBandwidthConsumerService) discoverStreams(ctx context.Context) []string {
	var keys []string
	if edges, err := s.redis.SMembers(ctx, "sys:edges").Result(); err == nil {
		for _, id := range edges {
			keys = append(keys, "dylaris:edge:"+id+":stats")
		}
	} else {
		log.Printf("gateway bandwidth discovery: SMembers sys:edges: %v", err)
	}
	if beams, err := s.redis.SMembers(ctx, "sys:beams").Result(); err == nil {
		for _, id := range beams {
			keys = append(keys, "dylaris:beam:"+id+":stats")
		}
	} else {
		log.Printf("gateway bandwidth discovery: SMembers sys:beams: %v", err)
	}
	// Splice: one per edge HOST, and it is keyed by that hostname rather than
	// by an edge id, so it cannot be derived from sys:edges. Scanning its own
	// streams is the only way to find one - which also means a splice too old
	// to publish simply does not appear, instead of appearing empty.
	keys = append(keys, scanStatsStreams(ctx, s.redis, "dylaris:splice:*:stats")...)
	// Link: keyed by the link's node id (never its token).
	keys = append(keys, scanStatsStreams(ctx, s.redis, "dylaris:link:*:stats")...)

	// Warp: scan the per-leader alive keys (dylaris:warp:{id}:alive).
	var cursor uint64
	for {
		batch, next, err := s.redis.Scan(ctx, cursor, "dylaris:warp:*:alive", 100).Result()
		if err != nil {
			log.Printf("gateway bandwidth discovery: Scan dylaris:warp:*:alive: %v", err)
			break
		}
		for _, ak := range batch {
			id := strings.TrimSuffix(strings.TrimPrefix(ak, "dylaris:warp:"), ":alive")
			if id != "" {
				keys = append(keys, "dylaris:warp:"+id+":stats")
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return keys
}

// scanStatsStreams returns every key matching pattern. Used for the components
// that publish a stats stream without also being listed in a registry set.
func scanStatsStreams(ctx context.Context, rdb *redis.Client, pattern string) []string {
	var out []string
	var cursor uint64
	for {
		batch, next, err := rdb.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			log.Printf("gateway bandwidth discovery: Scan %s: %v", pattern, err)
			return out
		}
		out = append(out, batch...)
		if next == 0 {
			return out
		}
		cursor = next
	}
}

// carriesThroughput reports whether a component belongs in the BANDWIDTH view.
//
// The splice and the link publish to the same telemetry stream, but neither
// reports throughput of its own: the splice shares a host and a network
// namespace with an edge that already reports every byte, and the link
// deliberately ships without a system monitor. Persisting them would put rows
// of zeros into gateway_bandwidth_stats and, worse, make them appear as
// components in the panel's bandwidth view with a permanent 0 bps - a reader
// would have to know the architecture to tell that apart from an outage.
//
// They are still CONSUMED, because their counters are the point; this decides
// only what reaches the bandwidth view.
func carriesThroughput(component string) bool {
	switch component {
	case "edge", "warp", "beam":
		return true
	}
	return false
}

// Snapshot returns the latest sample of every live component.
//
// The long-term metrics collector reads through this rather than opening its
// own consumer group on the same streams. One reader, one set of groups: a
// second group per stream per Core would double the pending-entry bookkeeping
// on Redis for data this service already has in memory.
func (s *GatewayBandwidthConsumerService) Snapshot() []protocol.GatewayStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]protocol.GatewayStats, 0, len(s.latest))
	now := time.Now()
	for _, v := range s.latest {
		// Same staleness bound persistOnce applies. A component that stopped
		// publishing must not keep contributing its last reading to an average,
		// or a dead edge reads as a quiet one.
		if now.Sub(v.seen) > gwbwStaleAfter {
			continue
		}
		// Counters are deliberately stripped: what is left in `latest` is ONE
		// publish's delta, and reading it as the window's total is exactly the
		// defect DrainCounters exists to fix. Gauges and the typed fields are
		// instants, so the newest is the right answer for them.
		gs := v.gs
		gs.Counters = nil
		out = append(out, gs)
	}
	return out
}

// addCounters folds one publish's deltas into the pending batch. Caller holds
// the lock.
//
// The distinct-name cap is applied HERE as well as at record time. Accumulating
// first and bounding later would let a producer that folds a session id or an
// address into a metric name grow this map with TRAFFIC rather than with code,
// between two collector ticks, in a process that cannot drop it until the next
// one.
func (s *GatewayBandwidthConsumerService) addCounters(key string, gs protocol.GatewayStats) {
	if len(gs.Counters) == 0 {
		return
	}
	b := s.pending[key]
	if b == nil {
		b = &CounterBatch{Component: gs.Component, ID: gs.ID, Counters: map[string]int64{}}
		s.pending[key] = b
	}
	// The newest region wins: a component that moved region keeps counting, and
	// the batch is attributed where it currently runs.
	b.Region = gs.Region
	for name, v := range gs.Counters {
		if !protocol.ValidMetricName(name) {
			continue
		}
		if _, known := b.Counters[name]; !known && len(b.Counters) >= protocol.MaxCustomMetrics {
			continue
		}
		b.Counters[name] += v
	}
}

// DrainCounters returns every component's counted events since the previous
// call and CLEARS them, so each event is handed out exactly once.
//
// Drained rather than read because the deltas cannot be re-derived: the
// publisher zeroes its own counters as it sends them, which is what makes a
// restart harmless there and makes a second reader impossible here.
func (s *GatewayBandwidthConsumerService) DrainCounters() []CounterBatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CounterBatch, 0, len(s.pending))
	for _, b := range s.pending {
		out = append(out, *b)
	}
	s.pending = map[string]*CounterBatch{}
	return out
}

// consume reads one stream via a per-Core consumer group and updates the
// latest map. Each Core instance uses its own group (rather than sharing
// gwbwGroup) so every Core sees every message and builds the FULL live view,
// not just the slice XREADGROUP happened to hand it - the persist step is
// leader-gated, but a non-leader Core must still be ready with a complete map
// the moment it wins the election. It parses with protocol.ParseGatewayStats
// so an unknown-version record is ignored (acked, not stored).
func (s *GatewayBandwidthConsumerService) consume(ctx context.Context, streamKey string) {
	group := gwbwGroup + "-" + s.coreID
	s.redis.XGroupCreateMkStream(ctx, streamKey, group, "0")
	log.Printf("Gateway bandwidth consumer started for %s", streamKey)
	for {
		if ctx.Err() != nil {
			return
		}
		res, err := s.redis.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    group,
			Consumer: s.coreID,
			Streams:  []string{streamKey, ">"},
			Count:    50,
			Block:    5 * time.Second,
		}).Result()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// A stream that is GONE is not healed back into existence: a revoked
			// link's stream is deleted with it, and recreating it here kept a
			// dead kit's stream and this goroutine alive for good. Stop and
			// forget it; the next scan starts a consumer again if its component
			// publishes again (which is also how a Redis restart recovers).
			if n, xerr := s.redis.Exists(ctx, streamKey).Result(); xerr == nil && n == 0 {
				s.mu.Lock()
				delete(s.streams, streamKey)
				s.mu.Unlock()
				log.Printf("Gateway bandwidth consumer stopped for %s: the stream no longer exists", streamKey)
				return
			}
			// Self-heal a vanished group on a stream that still exists.
			// Idempotent: BUSYGROUP when the group still exists.
			s.redis.XGroupCreateMkStream(ctx, streamKey, group, "0")
			time.Sleep(5 * time.Second)
			continue
		}
		for _, st := range res {
			var ackIDs []string
			for _, msg := range st.Messages {
				ackIDs = append(ackIDs, msg.ID)
				data, ok := msg.Values["data"].(string)
				if !ok {
					continue
				}
				gs, known, perr := protocol.ParseGatewayStats([]byte(data))
				if perr != nil || !known || gs.ID == "" {
					continue // bad json or unknown version: ack + drop
				}
				s.mu.Lock()
				key := gs.Component + ":" + gs.ID
				s.latest[key] = seenStat{gs: gs, seen: time.Now()}
				s.addCounters(key, gs)
				s.mu.Unlock()
			}
			if len(ackIDs) > 0 {
				s.redis.XAck(ctx, streamKey, group, ackIDs...)
			}
		}
	}
}

// persistLoop runs the leader-gated downsample: every persistInterval it
// snapshots the latest map, mirrors the per-component + per-host view to Redis,
// and writes one row per component to gateway_bandwidth_stats.
func (s *GatewayBandwidthConsumerService) persistLoop(ctx context.Context) {
	t := time.NewTicker(gwbwPersistInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.leader != nil && !s.leader.IsLeader() {
				continue
			}
			s.persistOnce(ctx)
		}
	}
}

func (s *GatewayBandwidthConsumerService) persistOnce(ctx context.Context) {
	now := time.Now()
	s.mu.Lock()
	snapshot := make(map[string]protocol.GatewayStats, len(s.latest))
	var staleIDs []string
	for k, v := range s.latest {
		if now.Sub(v.seen) > gwbwStaleAfter {
			staleIDs = append(staleIDs, k)
			delete(s.latest, k)
			continue
		}
		snapshot[k] = v.gs
	}
	s.mu.Unlock()

	// Drop the mirror of a departed component so the panel stops showing it as
	// live; its host aggregate self-expires via the mirror TTL once we stop re-Setting it.
	for _, id := range staleIDs {
		s.redis.Del(ctx, "dylaris:gwbw:component:"+id)
	}
	if len(snapshot) == 0 {
		return
	}

	// Only the components that actually carry throughput reach the bandwidth
	// view; see carriesThroughput.
	for k, gs := range snapshot {
		if !carriesThroughput(gs.Component) {
			delete(snapshot, k)
		}
	}
	if len(snapshot) == 0 {
		return
	}

	rows := make([]models.GatewayBandwidthRow, 0, len(snapshot))
	for _, gs := range snapshot {
		rows = append(rows, models.GatewayBandwidthRow{
			Time: now, Component: gs.Component, ID: gs.ID, Host: gs.Host,
			Region: gs.Region, RxBps: gs.RxBps, TxBps: gs.TxBps, CapMbit: gs.CapMbit,
		})
		if b, err := json.Marshal(gs); err == nil {
			s.redis.Set(ctx, "dylaris:gwbw:component:"+gs.Component+":"+gs.ID, b, gwbwMirrorTTL)
		}
	}
	for host, agg := range aggregateByHost(snapshot) {
		if b, err := json.Marshal(agg); err == nil {
			s.redis.Set(ctx, "dylaris:gwbw:host:"+host, b, gwbwMirrorTTL)
		}
	}
	if err := s.store.InsertGatewayBandwidthBatch(rows); err != nil {
		log.Printf("gateway bandwidth persist error: %v", err)
	}
}

// hostAggregate is the summed live throughput of all components co-located on
// one swarm host, plus the resolved host bandwidth budget.
type hostAggregate struct {
	Host        string `json:"host"`
	RxBps       uint64 `json:"rxBps"`
	TxBps       uint64 `json:"txBps"`
	BudgetMbit  int    `json:"budgetMbit"`
	CapMismatch bool   `json:"capMismatch"`
}

// aggregateByHost groups the latest per-component stats by host and applies the
// host-budget rule: the budget is the MAX cap_mbit among the host's components
// (they describe the same shared uplink, so max tolerates a misconfig without
// triple-counting). CapMismatch flags a host whose components report DIFFERENT
// non-zero caps so the panel can warn. cap_mbit==0 (unset) contributes to
// neither the budget nor the mismatch check. Components with an empty host are
// skipped (they cannot be co-located).
func aggregateByHost(latest map[string]protocol.GatewayStats) map[string]hostAggregate {
	out := map[string]hostAggregate{}
	for _, gs := range latest {
		if gs.Host == "" {
			continue
		}
		a := out[gs.Host]
		a.Host = gs.Host
		a.RxBps += gs.RxBps
		a.TxBps += gs.TxBps
		if gs.CapMbit > 0 {
			if a.BudgetMbit != 0 && a.BudgetMbit != gs.CapMbit {
				a.CapMismatch = true
			}
			if gs.CapMbit > a.BudgetMbit {
				a.BudgetMbit = gs.CapMbit
			}
		}
		out[gs.Host] = a
	}
	return out
}
