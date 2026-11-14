package services

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// peakStore records the peak writes and keeps rows the way the SQL does:
// every column only ever raised (GREATEST).
type peakStore struct {
	store.Store
	routes   []store.CoreLinkRoute
	routeErr error
	raiseErr error
	rows     map[string]store.PlayerPeak // owner|day
	writes   int
	purged   []time.Time
}

func (p *peakStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) {
	return p.routes, p.routeErr
}

func (p *peakStore) RaisePlayerPeak(owner string, day time.Time, java, bedrock, total int) error {
	if p.raiseErr != nil {
		return p.raiseErr
	}
	p.writes++
	if p.rows == nil {
		p.rows = map[string]store.PlayerPeak{}
	}
	k := owner + "|" + day.Format("2006-01-02")
	r := p.rows[k]
	r.Day, r.Java, r.Bedrock, r.Total = day, max(r.Java, java), max(r.Bedrock, bedrock), max(r.Total, total)
	p.rows[k] = r
	return nil
}

func (p *peakStore) PurgePlayerPeaks(before time.Time) (int64, error) {
	p.purged = append(p.purged, before)
	return 0, nil
}

func (p *peakStore) row(owner, day string) store.PlayerPeak { return p.rows[owner+"|"+day] }

// newPeaks is a local-mode leader sampler at clock, fed counts by the test.
func newPeaks(st *peakStore, clock *time.Time, counts *map[string]RoutePlayers) *GatewayPlayerPeaksService {
	s := NewGatewayPlayerPeaksService(st, nil)
	s.SetLeader(&fakeLeader{leader: true})
	s.SetGatewayRemote(func() (bool, bool) { return false, true })
	s.now = func() time.Time { return *clock }
	s.players = func(context.Context) map[string]RoutePlayers { return *counts }
	return s
}

var peakRoutes = []store.CoreLinkRoute{
	{Domain: "a1.example.com", OwnerID: "owner-a"},
	{Domain: "a2.example.com", OwnerID: "owner-a"},
	{Domain: "b1.example.com", OwnerID: "owner-b"},
}

// Review Focus 7: a sample can only raise a stored value, column by column.
func TestPlayerPeaksOnlyRaise(t *testing.T) {
	captureLog(t) // keeps the expected error lines out of the test output
	clock := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	st := &peakStore{routes: peakRoutes}
	var counts map[string]RoutePlayers
	s := newPeaks(st, &clock, &counts)
	steps := []struct {
		counts         map[string]RoutePlayers
		java, bed, tot int
	}{
		{map[string]RoutePlayers{"a1.example.com": {Java: 5, Bedrock: 1}}, 5, 1, 6},
		{map[string]RoutePlayers{"a1.example.com": {Java: 2}}, 5, 1, 6},             // lower: nothing moves
		{map[string]RoutePlayers{"a1.example.com": {Java: 1, Bedrock: 4}}, 5, 4, 6}, // only bedrock rises
		{map[string]RoutePlayers{"a1.example.com": {Java: 4, Bedrock: 3}}, 5, 4, 7}, // only total rises
	}
	for i, step := range steps {
		counts = step.counts
		s.sample(context.Background())
		got := st.row("owner-a", "2026-11-14")
		if got.Java != step.java || got.Bedrock != step.bed || got.Total != step.tot {
			t.Fatalf("step %d: row %+v, want %d/%d/%d", i, got, step.java, step.bed, step.tot)
		}
	}
	// A restarted process (cold map) sending a lower sample cannot lower it.
	s2 := newPeaks(st, &clock, &counts)
	counts = map[string]RoutePlayers{"a1.example.com": {Java: 1}}
	s2.sample(context.Background())
	if got := st.row("owner-a", "2026-11-14"); got.Java != 5 || got.Bedrock != 4 || got.Total != 7 {
		t.Fatalf("after a cold restart: %+v", got)
	}
}

func TestPlayerPeaksSkipWritesAtOrBelowTheLastWritten(t *testing.T) {
	captureLog(t) // keeps the expected error lines out of the test output
	clock := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	st := &peakStore{routes: peakRoutes}
	counts := map[string]RoutePlayers{"a1.example.com": {Java: 3, Bedrock: 1}}
	s := newPeaks(st, &clock, &counts)
	for i := 0; i < 100; i++ {
		s.sample(context.Background())
	}
	if st.writes != 1 {
		t.Fatalf("100 identical samples: %d writes, want 1", st.writes)
	}
	counts = map[string]RoutePlayers{"a1.example.com": {Java: 2}}
	s.sample(context.Background())
	if st.writes != 1 {
		t.Fatalf("a lower sample wrote: %d writes", st.writes)
	}
	counts = map[string]RoutePlayers{"a1.example.com": {Java: 4, Bedrock: 1}}
	s.sample(context.Background())
	if st.writes != 2 {
		t.Fatalf("a higher sample: %d writes, want 2", st.writes)
	}
	// A failed write is not remembered as written: the next sample retries.
	st.raiseErr = errors.New("connection reset")
	counts = map[string]RoutePlayers{"a1.example.com": {Java: 9}}
	s.sample(context.Background())
	st.raiseErr = nil
	s.sample(context.Background())
	if st.writes != 3 || st.row("owner-a", "2026-11-14").Java != 9 {
		t.Fatalf("retry after a failed write: %d writes, row %+v", st.writes, st.row("owner-a", "2026-11-14"))
	}
}

// seedPeakSample registers an edge with one stats sample aged by ago.
func seedPeakSample(t *testing.T, rdb *redis.Client, edge, raw string, ago time.Duration) {
	t.Helper()
	ctx := context.Background()
	if err := rdb.Set(ctx, "edge:registry:"+edge, `{"edge_id":"`+edge+`"}`, 0).Err(); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(time.Now().Add(-ago).UnixMilli(), 10) + "-0"
	if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: "dylaris:edge:" + edge + ":stats", ID: id, Values: map[string]interface{}{"data": raw}}).Err(); err != nil {
		t.Fatal(err)
	}
}

// Review Focus 7: no fresh sample, no write. Through the real edge read.
func TestPlayerPeaksWriteNothingWithoutFreshSamples(t *testing.T) {
	captureLog(t) // keeps the expected error lines out of the test output
	cases := []struct {
		name  string
		setup func(t *testing.T, mr *miniredis.Miniredis, rdb *redis.Client)
	}{
		{"stale stream", func(t *testing.T, _ *miniredis.Miniredis, rdb *redis.Client) {
			seedPeakSample(t, rdb, "e1", `{"players_by_route":{"a1.example.com":{"j":50}}}`, time.Minute)
		}},
		{"no edge counts", func(t *testing.T, _ *miniredis.Miniredis, rdb *redis.Client) {
			seedPeakSample(t, rdb, "e1", `{"players_by_server":{"srv":3}}`, time.Second)
		}},
		{"redis errors", func(t *testing.T, mr *miniredis.Miniredis, _ *redis.Client) { mr.SetError("LOADING redis is loading") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
			t.Cleanup(func() { rdb.Close() })
			c.setup(t, mr, rdb)
			existing := store.PlayerPeak{Java: 7, Bedrock: 2, Total: 8}
			st := &peakStore{routes: peakRoutes, rows: map[string]store.PlayerPeak{"owner-a|2026-11-14": existing}}
			s := NewGatewayPlayerPeaksService(st, rdb)
			s.SetLeader(&fakeLeader{leader: true})
			s.SetGatewayRemote(func() (bool, bool) { return false, true })
			s.now = func() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) }
			s.sample(context.Background())
			if st.writes != 0 || st.row("owner-a", "2026-11-14") != existing {
				t.Fatalf("writes %d, row %+v", st.writes, st.row("owner-a", "2026-11-14"))
			}
		})
	}
	// A failed route lookup writes nothing either.
	clock := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	counts := map[string]RoutePlayers{"a1.example.com": {Java: 3}}
	st := &peakStore{routes: peakRoutes, routeErr: errors.New("db down")}
	newPeaks(st, &clock, &counts).sample(context.Background())
	if st.writes != 0 {
		t.Fatalf("route lookup error: %d writes", st.writes)
	}
}

func TestPlayerPeaksFollowerIdle(t *testing.T) {
	captureLog(t) // keeps the expected error lines out of the test output
	clock := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	st := &peakStore{routes: peakRoutes}
	counts := map[string]RoutePlayers{"a1.example.com": {Java: 3}}
	s := newPeaks(st, &clock, &counts)
	l := &fakeLeader{leader: false}
	s.SetLeader(l)
	s.sample(context.Background())
	if st.writes != 0 || len(st.purged) != 0 {
		t.Fatalf("follower: %d writes, %d purges", st.writes, len(st.purged))
	}
	// Leader, then leadership lost and won back the same day: the remembered
	// peak was dropped with the lease, so the new term writes once again.
	l.leader = true
	s.sample(context.Background())
	l.leader = false
	s.sample(context.Background())
	l.leader = true
	s.sample(context.Background())
	if st.writes != 2 {
		t.Fatalf("lease lost and regained: %d writes, want 2", st.writes)
	}
}

func TestPlayerPeaksIdleInRemoteOrUnknownMode(t *testing.T) {
	captureLog(t) // keeps the expected error lines out of the test output
	cases := []struct {
		name          string
		remote, known bool
	}{
		{"remote", true, true},
		{"unknown", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clock := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
			st := &peakStore{routes: peakRoutes}
			counts := map[string]RoutePlayers{"a1.example.com": {Java: 3}}
			s := newPeaks(st, &clock, &counts)
			s.SetGatewayRemote(func() (bool, bool) { return c.remote, c.known })
			s.sample(context.Background())
			if st.writes != 0 || len(st.purged) != 0 {
				t.Fatalf("%d writes, %d purges", st.writes, len(st.purged))
			}
		})
	}
}

func TestPlayerPeaksSumPerOwnerAcrossAddresses(t *testing.T) {
	captureLog(t) // keeps the expected error lines out of the test output
	clock := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	routes := append([]store.CoreLinkRoute{}, peakRoutes...)
	susp := clock.Add(-time.Hour)
	routes = append(routes, store.CoreLinkRoute{Domain: "a-susp.example.com", OwnerID: "owner-a", SuspendedAt: &susp})
	st := &peakStore{routes: routes}
	counts := map[string]RoutePlayers{
		"a1.example.com":     {Java: 2, Bedrock: 1},
		"a2.example.com":     {Java: 3},
		"b1.example.com":     {Bedrock: 4},
		"orphan.example.com": {Java: 99}, // no route row: nobody's
		"a-susp.example.com": {Java: 50}, // suspended: not counted, like the admin view
	}
	newPeaks(st, &clock, &counts).sample(context.Background())
	if got := st.row("owner-a", "2026-11-14"); got.Java != 5 || got.Bedrock != 1 || got.Total != 6 {
		t.Errorf("owner-a = %+v, want 5/1/6", got)
	}
	if got := st.row("owner-b", "2026-11-14"); got.Java != 0 || got.Bedrock != 4 || got.Total != 4 {
		t.Errorf("owner-b = %+v, want 0/4/4", got)
	}
	if len(st.rows) != 2 {
		t.Errorf("rows %v, want exactly the two owners", st.rows)
	}
}

func TestPlayerPeaksDayRolloverResetsAndPurges(t *testing.T) {
	captureLog(t) // keeps the expected error lines out of the test output
	clock := time.Date(2026, 11, 14, 23, 59, 59, 0, time.UTC)
	st := &peakStore{routes: peakRoutes}
	counts := map[string]RoutePlayers{"a1.example.com": {Java: 3}}
	s := newPeaks(st, &clock, &counts)
	s.sample(context.Background())
	s.sample(context.Background()) // same day, same count: no second write, no second purge
	clock = time.Date(2026, 11, 15, 0, 0, 1, 0, time.UTC)
	s.sample(context.Background()) // new day: the same count is a new row
	if st.writes != 2 || st.row("owner-a", "2026-11-14").Java != 3 || st.row("owner-a", "2026-11-15").Java != 3 {
		t.Fatalf("writes %d rows %v", st.writes, st.rows)
	}
	want := []time.Time{
		time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -playerPeakKeepDays),
		time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -playerPeakKeepDays),
	}
	if len(st.purged) != 2 || !st.purged[0].Equal(want[0]) || !st.purged[1].Equal(want[1]) {
		t.Fatalf("purges %v, want %v", st.purged, want)
	}
	// A clock in another zone still files the sample under its UTC day.
	berlin := time.FixedZone("CET", 3600)
	clock = time.Date(2026, 11, 16, 0, 30, 0, 0, berlin) // 2026-11-15 23:30 UTC
	counts = map[string]RoutePlayers{"a1.example.com": {Java: 8}}
	s.sample(context.Background())
	if st.row("owner-a", "2026-11-15").Java != 8 || len(st.rows) != 2 {
		t.Fatalf("zoned clock: rows %v", st.rows)
	}
}

// Start returns on a nil Redis and the loop ends with its context.
func TestPlayerPeaksStopsWithTheContext(t *testing.T) {
	captureLog(t) // keeps the expected error lines out of the test output
	NewGatewayPlayerPeaksService(&peakStore{}, nil).Start(context.Background())
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	s := NewGatewayPlayerPeaksService(&peakStore{}, rdb)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.done = done
	s.Start(ctx)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the sampler did not stop with its context")
	}
}
