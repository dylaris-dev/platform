package services

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"dylaris-core/services/remotegw"
	"dylaris-core/services/remotegw/remotegwtest"
)

// The provider fails GET /players whole on purpose (no partial counts): a
// failed read answers the last good counts, not zero, and none before the
// first good read.
func TestRemotePlayerCountsKeepTheLastGood(t *testing.T) {
	fake := remotegwtest.NewFake(t)
	fake.AddRoute("link-1", "a.example.com", "mc-x", 25565)
	g := NewRemoteGateway(nil, func() *remotegw.Client { return remotegw.New(fake.URL(), fake.Key) })
	var mu sync.Mutex
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	step := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	ctx := context.Background()

	fake.FailNext(1, http.StatusInternalServerError, "internal", 0)
	if p := g.PlayerCounts(ctx); len(p.Items) != 0 || p.Java+p.Bedrock != 0 {
		t.Fatalf("before any good read: %+v", p)
	}
	step(remoteErrTTL)
	fake.SetPlayers("a.example.com", 3, 1)
	if p := g.PlayerCounts(ctx); p.Java != 3 || p.Bedrock != 1 || len(p.Items) != 1 {
		t.Fatalf("good read: %+v", p)
	}
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusTooManyRequests} {
		step(remotePlayersTTL)
		fake.FailNext(1, status, "", 0)
		if p := g.PlayerCounts(ctx); p.Java != 3 || p.Bedrock != 1 || !p.Stale || p.At.IsZero() {
			t.Fatalf("after a %d: %+v, want the last good counts, marked stale", status, p)
		}
		step(remoteErrTTL)
	}
}

// Review M-5: the last good counts are served for at most two minutes after
// the last good read, then the answer is empty: old numbers shown as current
// are worse than none.
func TestRemotePlayerCountsExpire(t *testing.T) {
	fake := remotegwtest.NewFake(t)
	fake.AddRoute("link-1", "a.example.com", "mc-x", 25565)
	fake.SetPlayers("a.example.com", 3, 1)
	g := NewRemoteGateway(nil, func() *remotegw.Client { return remotegw.New(fake.URL(), fake.Key) })
	var mu sync.Mutex
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	step := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	ctx := context.Background()
	if p := g.PlayerCounts(ctx); p.Java != 3 || p.Stale || p.At.IsZero() {
		t.Fatalf("good read: %+v", p)
	}
	fake.FailNext(100, http.StatusInternalServerError, "internal", 0)
	step(remotePlayersStaleMax - time.Second)
	if p := g.PlayerCounts(ctx); p.Java != 3 || !p.Stale {
		t.Fatalf("just inside the window: %+v", p)
	}
	step(remoteErrTTL + time.Second)
	if p := g.PlayerCounts(ctx); p.Java+p.Bedrock != 0 || len(p.Items) != 0 || p.Stale || !p.At.IsZero() {
		t.Fatalf("past the window: %+v, want nothing", p)
	}
}

// A slow provider never holds a panel request for long: the caller gets the
// last good counts after remotePlayersWait, the read goes on in the
// background and serves the next poll.
func TestRemotePlayerCountsDoNotWaitForASlowProvider(t *testing.T) {
	release := make(chan struct{})
	var calls int
	var mu sync.Mutex
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n > 1 {
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"asOf":"2026-11-14T12:00:00Z","total":{"java":2,"bedrock":0},"items":[{"domain":"a.example.com","java":2,"bedrock":0}]}`))
	}))
	defer slow.Close()
	defer close(release)
	g := NewRemoteGateway(nil, func() *remotegw.Client { return remotegw.New(slow.URL, func() string { return "k" }) })
	start := time.Now()
	var off time.Duration
	var offMu sync.Mutex
	g.now = func() time.Time { offMu.Lock(); defer offMu.Unlock(); return start.Add(off) }
	if p := g.PlayerCounts(context.Background()); p.Java != 2 {
		t.Fatalf("first read: %+v", p)
	}
	offMu.Lock()
	off = remotePlayersTTL + time.Second // past the cache window, inside the stale one
	offMu.Unlock()
	t0 := time.Now()
	p := g.PlayerCounts(context.Background())
	if took := time.Since(t0); took > remotePlayersWait+time.Second {
		t.Fatalf("a slow provider held the request for %v", took)
	}
	if p.Java != 2 {
		t.Fatalf("while the provider hangs: %+v, want the last good counts", p)
	}
}
