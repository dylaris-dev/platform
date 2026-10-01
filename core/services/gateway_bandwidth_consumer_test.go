package services

import (
	"context"
	"fmt"

	"dylaris-pkg/protocol"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestAggregateByHost(t *testing.T) {
	latest := map[string]protocol.GatewayStats{
		"edge-eu-1": {Component: "edge", ID: "edge-eu-1", Host: "web-eu-1", RxBps: 1000, TxBps: 2000, CapMbit: 10000},
		"beam-eu-1": {Component: "beam", ID: "beam-eu-1", Host: "web-eu-1", RxBps: 500, TxBps: 700, CapMbit: 10000},
		"warp-eu-1": {Component: "warp", ID: "warp-eu-1", Host: "web-eu-2", RxBps: 300, TxBps: 400, CapMbit: 1000},
		"nohost":    {Component: "edge", ID: "x", Host: "", RxBps: 9, TxBps: 9, CapMbit: 5},
	}
	got := aggregateByHost(latest)

	if len(got) != 2 {
		t.Fatalf("hosts = %d, want 2 (empty host dropped): %+v", len(got), got)
	}
	h1 := got["web-eu-1"]
	if h1.RxBps != 1500 || h1.TxBps != 2700 {
		t.Errorf("web-eu-1 sum = rx %d tx %d, want 1500/2700", h1.RxBps, h1.TxBps)
	}
	if h1.BudgetMbit != 10000 {
		t.Errorf("web-eu-1 budget = %d, want 10000 (max of co-located caps)", h1.BudgetMbit)
	}
	if h1.CapMismatch {
		t.Error("web-eu-1 caps agree (10000/10000), CapMismatch should be false")
	}
}

func TestAggregateByHostCapMismatch(t *testing.T) {
	latest := map[string]protocol.GatewayStats{
		"a": {Host: "h", RxBps: 1, CapMbit: 10000},
		"b": {Host: "h", RxBps: 1, CapMbit: 1000}, // disagrees
		"c": {Host: "h", RxBps: 1, CapMbit: 0},    // unset: ignored for budget + mismatch
	}
	got := aggregateByHost(latest)["h"]
	if got.BudgetMbit != 10000 {
		t.Errorf("budget = %d, want 10000 (max)", got.BudgetMbit)
	}
	if !got.CapMismatch {
		t.Error("differing non-zero caps must set CapMismatch")
	}
}

func TestOnlyThroughputCarryingComponentsReachTheBandwidthView(t *testing.T) {
	// The splice and the link publish to the same telemetry stream but carry no
	// throughput of their own: the splice shares a host and a network namespace
	// with an edge that already reports every byte, and the link ships without a
	// system monitor. Letting them through would put a component into the
	// bandwidth view showing a permanent 0 bps, which a reader cannot tell from
	// an outage - and would add rows of zeros to the history table.
	for _, c := range []string{"edge", "warp", "beam"} {
		if !carriesThroughput(c) {
			t.Errorf("%s reports throughput and was excluded from the bandwidth view", c)
		}
	}
	for _, c := range []string{"splice", "link", "", "something-new"} {
		if carriesThroughput(c) {
			t.Errorf("%q was let into the bandwidth view and reports no throughput", c)
		}
	}
}

func TestSnapshotDropsAComponentThatStoppedPublishing(t *testing.T) {
	// The same staleness bound persistOnce applies. Without it a dead edge keeps
	// contributing its last reading to every average, so an outage reads as a
	// quiet period - which is the opposite of what happened.
	s := NewGatewayBandwidthConsumerService(nil, nil, "core-1")
	s.latest["edge:live"] = seenStat{
		gs:   protocol.GatewayStats{Component: "edge", ID: "live"},
		seen: time.Now(),
	}
	s.latest["edge:gone"] = seenStat{
		gs:   protocol.GatewayStats{Component: "edge", ID: "gone"},
		seen: time.Now().Add(-2 * gwbwStaleAfter),
	}

	got := s.Snapshot()
	if len(got) != 1 || got[0].ID != "live" {
		t.Fatalf("snapshot = %+v, want only the live component", got)
	}
}

func TestSnapshotIsACopyRatherThanTheLiveMap(t *testing.T) {
	// The consumer goroutines write into latest while the collector reads. A
	// snapshot that shared the map would be a data race on every sample.
	s := NewGatewayBandwidthConsumerService(nil, nil, "core-1")
	s.latest["edge:a"] = seenStat{gs: protocol.GatewayStats{Component: "edge", ID: "a"}, seen: time.Now()}
	got := s.Snapshot()
	got[0].ID = "mutated"
	if s.latest["edge:a"].gs.ID != "a" {
		t.Fatal("mutating the snapshot changed the consumer's own state")
	}
}

// publish is what one component sending a telemetry record does to the
// consumer, minus Redis. The accumulation lives on this path, so the test has
// to use it rather than writing the maps by hand - a test that built the
// pending batch itself would pass against the code that dropped nine records
// out of ten.
func (s *GatewayBandwidthConsumerService) publishForTest(gs protocol.GatewayStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := gs.Component + ":" + gs.ID
	s.latest[key] = seenStat{gs: gs, seen: time.Now()}
	s.addCounters(key, gs)
}

// Every publish between two drains is counted, not just the last one.
//
// The components publish a counter as a DELTA every 3 seconds and the metrics
// collector drains every 30, so keeping only the newest record threw away nine
// events out of ten. Measured in production on 2026-09-03: the two splices
// logged 8 dropped players in a window where the long-term record held 1.
func TestEveryCounterPublishBetweenTwoDrainsIsCounted(t *testing.T) {
	s := NewGatewayBandwidthConsumerService(nil, nil, "core-1")
	for i := 0; i < 10; i++ {
		s.publishForTest(protocol.GatewayStats{
			Component: "splice", ID: "host-1", Region: "eu",
			Counters: map[string]int64{"players_dropped": 1, "sessions_opened": 3},
			Gauges:   map[string]float64{"active_sessions": float64(i)},
		})
	}

	got := s.DrainCounters()
	if len(got) != 1 {
		t.Fatalf("batches = %d, want 1: %+v", len(got), got)
	}
	b := got[0]
	if b.Component != "splice" || b.ID != "host-1" || b.Region != "eu" {
		t.Errorf("batch identity = %+v", b)
	}
	if b.Counters["players_dropped"] != 10 {
		t.Errorf("players_dropped = %d, want 10 (one per publish); a value of 1 is the newest record alone",
			b.Counters["players_dropped"])
	}
	if b.Counters["sessions_opened"] != 30 {
		t.Errorf("sessions_opened = %d, want 30", b.Counters["sessions_opened"])
	}

	// Drained, not read: the publisher zeroed its own counters as it sent them,
	// so handing the same events out twice would invent them.
	if again := s.DrainCounters(); len(again) != 0 {
		t.Errorf("a second drain returned %+v, want nothing", again)
	}

	// The gauge is unaffected: sampling the newest is correct for an instant,
	// and Snapshot keeps serving it.
	snap := s.Snapshot()
	if len(snap) != 1 || snap[0].Gauges["active_sessions"] != 9 {
		t.Errorf("snapshot gauges = %+v, want the newest reading", snap)
	}
	// ...and it must not carry a lone delta that a caller could mistake for the
	// window's total.
	if snap[0].Counters != nil {
		t.Errorf("snapshot still exposes counters: %+v", snap[0].Counters)
	}
}

// A producer that folds a session id into a metric name cannot grow the pending
// batch without bound between two drains. The cap is enforced while
// accumulating, not only while recording: recording happens once a tick, and by
// then the map has already been built.
func TestAccumulationIsBoundedByTheMetricNameCap(t *testing.T) {
	s := NewGatewayBandwidthConsumerService(nil, nil, "core-1")
	for i := 0; i < protocol.MaxCustomMetrics*3; i++ {
		s.publishForTest(protocol.GatewayStats{
			Component: "splice", ID: "host-1",
			Counters: map[string]int64{fmt.Sprintf("session_%d", i): 1},
		})
	}
	got := s.DrainCounters()
	if len(got) != 1 {
		t.Fatalf("batches = %d, want 1", len(got))
	}
	if n := len(got[0].Counters); n > protocol.MaxCustomMetrics {
		t.Errorf("accumulated %d distinct names, cap is %d", n, protocol.MaxCustomMetrics)
	}
	// A name that cannot be a series must not occupy one of the slots either.
	s2 := NewGatewayBandwidthConsumerService(nil, nil, "core-1")
	s2.publishForTest(protocol.GatewayStats{
		Component: "splice", ID: "h",
		Counters: map[string]int64{"bad name!": 5, "good_name": 7},
	})
	b := s2.DrainCounters()
	if len(b) != 1 || b[0].Counters["good_name"] != 7 {
		t.Fatalf("valid counter lost: %+v", b)
	}
	if _, bad := b[0].Counters["bad name!"]; bad {
		t.Error("an invalid metric name was accumulated")
	}
}

// A link writes only its own stream but named itself in the record: posting
// as a real edge replaced that edge's counters.
func TestALinkStreamSpeaksOnlyAsThatLink(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	data, _ := protocol.MarshalGatewayStats(protocol.GatewayStats{
		TS: time.Now().Unix(), Component: "edge", ID: "edge-1", Counters: map[string]int64{"handover_dropped": 50},
	})
	stream := "dylaris:link:tok-a:stats"
	rdb.XAdd(t.Context(), &redis.XAddArgs{Stream: stream, Values: map[string]interface{}{"data": string(data)}})

	s := NewGatewayBandwidthConsumerService(nil, rdb, "core-1")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go s.consume(ctx, stream)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		_, asEdge := s.latest["edge:edge-1"]
		_, asLink := s.latest["link:tok-a"]
		s.mu.Unlock()
		if asEdge {
			t.Fatal("a link's record was taken as an edge's")
		}
		if asLink {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the link's record never arrived")
}
