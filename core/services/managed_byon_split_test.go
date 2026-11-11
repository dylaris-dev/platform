package services

import (
	"context"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

// managedPlusByon is mg-1 holding both: the managed pool (100 GB, no consent)
// and a BYON node with server srv-9 and a 100 GB data allowance (one unit).
// managedRelay is own-ticket relay bytes (product route), serverRelay the
// server's beam/tab bytes on our fleet (product byon).
func managedPlusByon(t *testing.T, edge, managedRelay, serverRelay int64) (*trafficFakeStore, *TrafficAggregator, map[string]string) {
	t.Helper()
	fs, agg, _, owners := graceFixture(t, false)
	one := int64(1)
	fs.billing["mg-1"].MaxNodes = &one
	fs.dataLimit = &models.TrafficLimit{IncludedGB: i64(100)}
	setSplit(fs, edge, managedRelay, serverRelay)
	return fs, agg, owners
}

// setSplit writes the totals and the region cells the way one aggregator tick
// leaves them, keeping an open grace spell.
func setSplit(fs *trafficFakeStore, edge, managedRelay, serverRelay int64) {
	u := fs.usage["mg-1"]
	if u == nil {
		u = &store.TrafficUsage{}
		if fs.usage == nil {
			fs.usage = map[string]*store.TrafficUsage{}
		}
		fs.usage["mg-1"] = u
	}
	u.EdgeBytes, u.RelayBytes, u.ManagedRelayBytes = edge, managedRelay+serverRelay, managedRelay
	fs.dataUsage = map[string][]store.RegionUsage{"mg-1": {
		{Region: "eu-central", Kind: store.TrafficKindRelay, Product: store.TrafficProductRoute, Bytes: managedRelay},
		{Region: "eu-central", Kind: store.TrafficKindRelay, Product: store.TrafficProductBYON, Bytes: serverRelay},
	}}
}

// I1: in grace mode the tenant's own servers are judged by the BYON data
// verdict on their own bytes, at the configured throttle, and none of those
// bytes are grace bytes or leave any number.
func TestManagedGraceLeavesByonServerDataAlone(t *testing.T) {
	fs, agg, owners := managedPlusByon(t, 60*poolGB, 40*poolGB, 50*poolGB)
	ctx := context.Background()
	agg.evaluateDataThrottle(ctx, owners) // pool 100 GB: grace; server data 50 of 100 GB
	if s := fs.usage["mg-1"].GraceSinceRelay; s == nil || *s != 40*poolGB {
		t.Fatalf("grace snapshot %v, want only the 40 GB of own-ticket relay bytes", s)
	}
	if agg.redis.Exists(ctx, DataBandwidthKey("srv-9")).Val() != 0 {
		t.Fatal("grace mode slowed a server whose own data allowance is not used up")
	}

	// The server pulls 500 GB more: BYON data bytes, not grace.
	setSplit(fs, 60*poolGB, 40*poolGB, 550*poolGB)
	agg.evaluateDataThrottle(ctx, owners)
	u := fs.usage["mg-1"]
	if g := u.GraceRelayBytes(); g != 0 {
		t.Fatalf("server bytes counted as grace: %d", g)
	}
	if p := ManagedPoolBytes(u); p != 100*poolGB {
		t.Fatalf("pool %d, want 100 GB: server bytes are not in the managed pool", p)
	}
	bv, err := DataTrafficVerdictFor(fs, "mg-1", time.Now())
	if err != nil || bv.UsedGB != 550 || !bv.Throttled {
		t.Fatalf("BYON verdict %+v err %v, want 550 GB of server data, throttled", bv, err)
	}
	if v, _ := agg.redis.Get(ctx, DataBandwidthKey("srv-9")).Int64(); v != DataThrottleDefaultMbit*125_000 {
		t.Fatalf("server data_bw %d, want the BYON throttle %d (not grace %d)", v, DataThrottleDefaultMbit*125_000, GraceBps)
	}
	if !IsDataThrottled(ctx, agg.redis, "mg-1") {
		t.Fatal("BYON throttle not recorded")
	}
	// Own-ticket grace bytes do grow the grace number and nothing else.
	setSplit(fs, 60*poolGB, 70*poolGB, 550*poolGB)
	if g := fs.usage["mg-1"].GraceRelayBytes(); g != 30*poolGB {
		t.Fatalf("grace %d, want the 30 GB of own-ticket bytes since entry", g)
	}
}

// The pool (Task 5 too) is edge + own-ticket relay bytes only.
func TestManagedPoolCountsOnlyOwnTicketRelay(t *testing.T) {
	fs, _, _ := managedPlusByon(t, 50*poolGB, 30*poolGB, 900*poolGB)
	v, managed, err := ManagedPoolVerdictFor(fs, "mg-1", time.Now())
	if err != nil || !managed {
		t.Fatalf("managed %v err %v", managed, err)
	}
	if v.UsedGB != 80 || v.Throttled {
		t.Fatalf("pool %d GB throttled %v, want 80 GB inside 100", v.UsedGB, v.Throttled)
	}
}

// Leaving managed closes the open spell, so its bytes stay out for the month.
func TestGraceSpellClosedWhenNoLongerManaged(t *testing.T) {
	fs, agg, _, owners := graceFixture(t, false)
	ctx := context.Background()
	fs.usage = map[string]*store.TrafficUsage{"mg-1": mgUsage(60*poolGB, 40*poolGB)}
	agg.evaluateDataThrottle(ctx, owners)
	addRelay(fs.usage["mg-1"], 200*poolGB)

	delete(fs.managedLimit, "mg-1") // plan cancelled: the store removes the row
	agg.evaluateDataThrottle(ctx, owners)
	u := fs.usage["mg-1"]
	if u.GraceSinceRelay != nil || u.GraceExcludedBytes != 200*poolGB {
		t.Fatalf("since %v excluded %d, want the spell closed with 200 GB kept out", u.GraceSinceRelay, u.GraceExcludedBytes)
	}
	if agg.redis.SIsMember(ctx, managedThrottledSet, "mg-1").Val() {
		t.Fatal("a cancelled managed pool stayed in the managed throttled set")
	}
}
