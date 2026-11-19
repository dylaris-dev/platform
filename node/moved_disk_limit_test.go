package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// A server that moved here ran with no disk limit: the move commands carried
// none and the node registered nothing for the arriving directory.
func TestAMovedServerKeepsItsDiskLimit(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	quota := NewQuotaSet(nil)
	const uuid = "moved-1"
	dir := t.TempDir()

	applyMovedDiskLimit(ctx, rdb, quota, dir, uuid, sentDiskLimit(`{"config":{"docker":{"diskLimit":2048}}}`), "")
	if got := loadDiskLimit(ctx, rdb, uuid); got != 2048 {
		t.Fatalf("limit after the move = %d, want 2048", got)
	}

	// An older Core sends no limit. That means "not told", and must not wipe
	// the limit this node already enforces.
	applyMovedDiskLimit(ctx, rdb, quota, dir, uuid, sentDiskLimit(`{"config":{}}`), "")
	if got := loadDiskLimit(ctx, rdb, uuid); got != 2048 {
		t.Fatalf("limit after a move that named none = %d, want the known 2048 kept", got)
	}

	// A failed move leaves no directory, and nothing is recorded for it.
	applyMovedDiskLimit(ctx, rdb, quota, dir+"/missing", "never-arrived", sentDiskLimit(`{"config":{"docker":{"diskLimit":512}}}`), "")
	if got := loadDiskLimit(ctx, rdb, "never-arrived"); got != 0 {
		t.Fatalf("a server that never arrived got limit %d", got)
	}
}

// Usage was measured only for running containers, and every upload check lets
// an upload through when it finds none - so a stopped server took any amount.
func TestStoppedServersPublishTheirDiskUsage(t *testing.T) {
	prevNodeID, prevMgr := nodeID, globalStorageMgr
	t.Cleanup(func() { nodeID, globalStorageMgr = prevNodeID, prevMgr })
	nodeID = "node-1"
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	root := t.TempDir()
	globalStorageMgr = NewStorageManager(root, nil)
	const stopped = "aaaaaaaa-1111-2222-3333-444444444444"
	const running = "bbbbbbbb-1111-2222-3333-444444444444"
	// The id the panel actually mints: "<ownerUUID>_<random>". Every real
	// server has this shape, and the sweep used to skip all of them.
	const minted = "cccccccc-1111-2222-3333-444444444444_232qs8ryxy3"
	for _, d := range []string{stopped, running, minted, "not-a-server"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, d, "world.dat"), make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	recordDiskLimit(ctx, rdb, stopped, 1)
	recordDiskLimit(ctx, rdb, minted, 1)

	publishStoppedDiskUsage(ctx, rdb, NewQuotaSet(nil), map[string]bool{running: true})

	raw, err := rdb.Get(ctx, "dylaris:server:"+stopped+":stats:disk").Result()
	if err != nil {
		t.Fatalf("no disk usage published for the stopped server: %v", err)
	}
	var usage DiskUsagePayload
	if err := json.Unmarshal([]byte(raw), &usage); err != nil || usage.Total < 4096 || usage.Limit != 1024*1024 {
		t.Fatalf("published usage = %s (%v), want total >= 4096 and the 1 MB limit", raw, err)
	}
	if mr.Exists("dylaris:server:" + running + ":stats:disk") {
		t.Error("the running server's own collector is the one that measures it")
	}
	if !mr.Exists("dylaris:server:" + minted + ":stats:disk") {
		t.Error("a server with the panel's minted id was skipped")
	}
	if mr.Exists("dylaris:server:not-a-server:stats:disk") {
		t.Error("a directory that is not a server was measured")
	}
}

// K45: the sweep measures stopped servers one after another, so a slow one
// delayed the next measurement of the others past their gauge's TTL; with the
// gauge gone every upload check let any amount through. Each sweep re-arms
// the gauges it is about to measure before it starts walking.
func TestStoppedDiskSweepRearmsGaugesFirst(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	const stopped = "aaaaaaaa-1111-2222-3333-444444444444"
	const running = "bbbbbbbb-1111-2222-3333-444444444444"
	const never = "cccccccc-1111-2222-3333-444444444444"
	for _, u := range []string{stopped, running} {
		rdb.Set(ctx, diskGaugeKey(u), `{"total":1}`, time.Minute)
	}

	rearmStoppedDiskGauges(ctx, rdb, []string{stopped, running, never}, map[string]bool{running: true})

	cases := []struct {
		uuid string
		want time.Duration
	}{
		{stopped, stoppedDiskUsageTTL},
		{running, time.Minute},
		{never, 0},
	}
	for _, c := range cases {
		if got := mr.TTL(diskGaugeKey(c.uuid)); got != c.want {
			t.Errorf("%s: ttl %v, want %v", c.uuid, got, c.want)
		}
	}
	if mr.Exists(diskGaugeKey(never)) {
		t.Error("re-arming created a gauge that was never measured")
	}
}

// T10: a running server's walk that outlasted its tick skipped the next one,
// and the gauge, kept alive only by the publish TTL, expired in the gap. Every
// disk tick re-arms the existing gauge, the skipped one included.
func TestRunningDiskTickRearmsGauge(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	const measured = "aaaaaaaa-1111-2222-3333-444444444444"
	const never = "cccccccc-1111-2222-3333-444444444444"
	rdb.Set(ctx, diskGaugeKey(measured), `{"total":1}`, time.Minute)

	cases := []struct {
		name      string
		uuid      string
		busy      bool
		wantStart bool
	}{
		{"idle", measured, false, true},
		{"walk still running", measured, true, false},
		{"never measured", never, false, true},
	}
	for _, c := range cases {
		mr.SetTTL(diskGaugeKey(measured), time.Minute)
		var measuring atomic.Bool
		measuring.Store(c.busy)
		if got := startDiskTick(ctx, rdb, c.uuid, &measuring); got != c.wantStart {
			t.Errorf("%s: start %v, want %v", c.name, got, c.wantStart)
		}
		if c.uuid == never {
			if mr.Exists(diskGaugeKey(never)) {
				t.Errorf("%s: re-arming created a gauge", c.name)
			}
			continue
		}
		if got := mr.TTL(diskGaugeKey(measured)); got != runningDiskUsageTTL {
			t.Errorf("%s: ttl %v, want %v", c.name, got, runningDiskUsageTTL)
		}
	}
}
