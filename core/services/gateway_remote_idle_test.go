package services

import (
	"context"
	"testing"
	"time"

	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// TestRemoteBackgroundServicesStayIdle runs one tick of every local-gateway
// background service with the mode remote, and with it unknown, and asserts
// the tick sent Redis nothing: there is no local gateway state to read, and an
// unknown mode is never taken for local. Where a tick is cheap to run locally,
// the same tick in local mode must reach Redis, so a passing idle case is not
// a tick that never reads Redis at all.
func TestRemoteBackgroundServicesStayIdle(t *testing.T) {
	ctx := context.Background()
	tenancyOn := NewFeatureFlags(stubSettings{m: map[string]string{"feature_byon_enabled": "true"}})
	tests := []struct {
		name string
		// tick builds the service on rdb with the given mode and runs one tick.
		tick func(rdb *redis.Client, mode func() (bool, bool))
		// localReads: the same tick in local mode must send Redis a command.
		localReads bool
	}{
		{"route republisher", func(rdb *redis.Client, mode func() (bool, bool)) {
			s := NewRouteRepublisher(&republishFakeStore{rows: []store.CoreLinkRoute{{Domain: "play.example.com", LinkToken: "tok", TargetHost: "10.0.0.1", TargetPort: 25565}}}, rdb)
			s.SetGatewayRemote(mode)
			s.adoptOnce(ctx)
			s.RunOnce(ctx)
		}, true},
		{"node link learner", func(rdb *redis.Client, mode func() (bool, bool)) {
			l := NewNodeLinkLearner(&nodeLinkFakeStore{}, NewRedisGateway(rdb, nil, "secret"))
			l.SetGatewayRemote(mode)
			l.RunOnce(ctx)
		}, true},
		{"bandwidth consumer scan", func(rdb *redis.Client, mode func() (bool, bool)) {
			s := NewGatewayBandwidthConsumerService(nil, rdb, "core-a")
			s.SetGatewayRemote(mode)
			s.scan(ctx)
		}, true},
		{"bandwidth consumer persist", func(rdb *redis.Client, mode func() (bool, bool)) {
			s := NewGatewayBandwidthConsumerService(nil, rdb, "core-a")
			s.SetGatewayRemote(mode)
			// A component last seen long ago: locally its mirror key is
			// deleted, before any store write is reached.
			s.latest["edge:e1"] = seenStat{}
			s.persistOnce(ctx)
		}, true},
		{"warp self-registration", func(rdb *redis.Client, mode func() (bool, bool)) {
			s := NewWarpSelfRegistrar(&selfRegStore{}, rdb)
			s.SetGatewayRemote(mode)
			s.RunOnce(ctx)
		}, true},
		// The rest need a full store to run locally; with the mode remote they
		// must return before touching it (a nil store would panic).
		{"traffic aggregator", func(rdb *redis.Client, mode func() (bool, bool)) {
			a := NewTrafficAggregator(nil, rdb, tenancyOn)
			a.SetGatewayRemote(mode)
			a.runOnce(ctx)
		}, false},
		{"warp rebalancer", func(rdb *redis.Client, mode func() (bool, bool)) {
			w := NewWarpRebalancer(nil, rdb, nil)
			w.SetGatewayRemote(mode)
			w.tick(ctx, time.Now())
		}, false},
		{"voice reconcile and meter", func(rdb *redis.Client, mode func() (bool, bool)) {
			v := NewVoiceService(nil, rdb)
			v.SetGatewayRemote(mode)
			if err := v.Reconcile(ctx); err != nil {
				t.Errorf("voice reconcile: %v", err)
			}
			v.Meter(ctx)
		}, false},
		// The Bedrock reconcile is not idle in remote mode: it publishes the
		// Floodgate keys downloaded from the provider for this platform's own
		// nodes (TestBedrockReconcileRemotePublishesAndSweeps; the unknown mode
		// is TestBedrockReconcileIdleWhenModeUnknown).
		{"bedrock meter", func(rdb *redis.Client, mode func() (bool, bool)) {
			b := NewBedrockService(nil, rdb)
			b.SetGatewayRemote(mode)
			b.Meter(ctx)
		}, false},
		{"metrics collector gateway sample", func(rdb *redis.Client, mode func() (bool, bool)) {
			c := NewMetricsCollector(nil, rdb, nil, nil, nil)
			c.SetGatewayRemote(mode)
			c.sampleGateway(ctx, time.Now())
		}, false},
	}
	modes := []struct {
		name string
		mode func() (bool, bool)
	}{
		{"remote", func() (bool, bool) { return true, true }},
		{"unknown", func() (bool, bool) { return false, false }},
	}
	for _, tt := range tests {
		for _, m := range modes {
			t.Run(tt.name+"/"+m.name, func(t *testing.T) {
				mr, rdb := newTestRedis(t)
				before := mr.CommandCount()
				tt.tick(rdb, m.mode)
				if n := mr.CommandCount() - before; n != 0 {
					t.Fatalf("one tick sent Redis %d command(s) with the gateway mode %s", n, m.name)
				}
			})
		}
		if tt.localReads {
			t.Run(tt.name+"/local control", func(t *testing.T) {
				mr, rdb := newTestRedis(t)
				before := mr.CommandCount()
				tt.tick(rdb, func() (bool, bool) { return false, true })
				if mr.CommandCount() == before {
					t.Fatal("the local tick sent Redis nothing, so the idle cases prove nothing")
				}
			})
		}
	}
}
