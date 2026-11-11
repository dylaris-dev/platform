package services

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

const poolGB = int64(1_000_000_000)

// A managed tenant's relay bytes meter under relay:owner:<id>; the aggregator
// attributes them to the issuer owner and the verdict adds them to the edge
// bytes against the one "*" edge row.
func TestManagedPoolSumsEdgeAndRelay(t *testing.T) {
	fs := &trafficFakeStore{
		owners:     map[string]string{},
		issuerKeys: []models.BeamIssuerKey{{ID: beamKidA1, OwnerID: "mg-1"}},
	}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	mustHSet(t, rdb, trafficRelayPrefix+"owner:mg-1", map[string]interface{}{"up:eu-central": 30, "down:eu-central": 70})
	agg.runOnce(context.Background())
	if len(fs.addUsageCalls) != 1 || fs.addUsageCalls[0].tenant != "mg-1" || fs.addUsageCalls[0].relayBytes != 100 {
		t.Fatalf("usage calls %+v, want 100 relay bytes for mg-1", fs.addUsageCalls)
	}

	// 60 GB of players alone is inside 100 GB; with 50 GB of beam it is not.
	fs.managedLimit = map[string]*models.TrafficLimit{"mg-1": {IncludedGB: i64(100), MaxPurchaseGB: i64(50)}}
	for _, c := range []struct {
		name        string
		edge, relay int64
		throttled   bool
	}{
		{"edge alone inside", 60 * poolGB, 0, false},
		{"edge plus relay over", 60 * poolGB, 50 * poolGB, true},
		{"relay alone over", 0, 100 * poolGB, true},
	} {
		fs.usage = map[string]*store.TrafficUsage{"mg-1": {EdgeBytes: c.edge, RelayBytes: c.relay, ManagedRelayBytes: c.relay}}
		v, managed, err := ManagedPoolVerdictFor(fs, "mg-1", time.Now())
		if err != nil || !managed {
			t.Fatalf("%s: managed %v err %v", c.name, managed, err)
		}
		if v.Throttled != c.throttled || v.UsedGB != (c.edge+c.relay)/poolGB {
			t.Errorf("%s: throttled %v used %d, want %v %d", c.name, v.Throttled, v.UsedGB, c.throttled, (c.edge+c.relay)/poolGB)
		}
	}
}

func TestJudgeManagedPool(t *testing.T) {
	cases := []struct {
		name          string
		used          int64
		included, max *int64
		consent       bool
		warn, thr     bool
	}{
		{"no allowance", 900 * poolGB, nil, i64(50), true, false, false},
		{"consent: past included, inside the store cap", 120 * poolGB, i64(100), i64(50), true, false, false},
		{"consent: at the store cap", 150 * poolGB, i64(100), i64(50), true, true, true},
		{"consent: no store cap never slows", 900 * poolGB, i64(100), nil, true, false, false},
		{"consent: nothing for sale slows at included", 100 * poolGB, i64(100), i64(0), true, true, true},
		{"no consent slows at included", 100 * poolGB, i64(100), i64(50), false, true, true},
		{"no consent at 90 %", 90 * poolGB, i64(100), i64(50), false, true, false},
	}
	for _, c := range cases {
		v := JudgeManagedPool(c.used, c.included, c.max, c.consent)
		if v.Warn != c.warn || v.Throttled != c.thr {
			t.Errorf("%s: warn %v throttled %v, want %v %v", c.name, v.Warn, v.Throttled, c.warn, c.thr)
		}
	}
}

// With consent the pool is billed up to the store cap, so the issuer record
// carries bw_bps only past included + max purchase, and loses it on release.
func TestManagedThrottleOnlyPastStoreCap(t *testing.T) {
	fs := &trafficFakeStore{
		owners:       map[string]string{"mg-1": "mg-1"},
		issuerKeys:   []models.BeamIssuerKey{{ID: beamKidA1, OwnerID: "mg-1", PublicKey: make([]byte, 32)}},
		managedLimit: map[string]*models.TrafficLimit{"mg-1": {IncludedGB: i64(100), MaxPurchaseGB: i64(50)}},
		billing:      map[string]*store.UserBilling{"mg-1": {UserID: "mg-1", Status: "active", TrafficBillingEnabled: true}},
		dataConsent:  map[string]bool{},
	}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	fs.settings[BeamTenantMaxConnsKey] = "" // the real store answers a missing setting with ""
	ctx := context.Background()
	owners := map[string]string{"owner:mg-1": "mg-1", "srv-9": "mg-1"}
	bw := func() (int64, bool) {
		t.Helper()
		raw, err := rdb.Get(ctx, BeamIssuerKeyPrefix+beamKidA1).Bytes()
		if err != nil {
			t.Fatalf("issuer record: %v", err)
		}
		var m map[string]json.RawMessage
		_ = json.Unmarshal(raw, &m)
		v, ok := m["bw_bps"]
		if !ok {
			return 0, false
		}
		var n int64
		_ = json.Unmarshal(v, &n)
		return n, true
	}
	if err := PublishBeamIssuers(ctx, fs, rdb, 0, "mg-1"); err != nil {
		t.Fatal(err)
	}

	fs.usage = map[string]*store.TrafficUsage{"mg-1": {EdgeBytes: 100 * poolGB, RelayBytes: 40 * poolGB, ManagedRelayBytes: 40 * poolGB}} // billed, not slowed
	agg.evaluateDataThrottle(ctx, owners)
	if _, ok := bw(); ok || IsDataThrottled(ctx, rdb, "mg-1") {
		t.Fatal("throttled inside the purchasable range")
	}

	fs.usage["mg-1"] = &store.TrafficUsage{EdgeBytes: 100 * poolGB, RelayBytes: 50 * poolGB, ManagedRelayBytes: 50 * poolGB} // at the store cap
	agg.evaluateDataThrottle(ctx, owners)
	if n, ok := bw(); !ok || n != DataThrottleDefaultMbit*125_000 {
		t.Fatalf("bw_bps = %d (present %v), want %d", n, ok, DataThrottleDefaultMbit*125_000)
	}
	// The pool's throttle paces the tenant's tickets only; their server on
	// our fleet follows the BYON data verdict, which is not over.
	if rdb.Exists(ctx, DataBandwidthKey("srv-9")).Val() != 0 {
		t.Fatal("the managed pool throttled the tenant's own server")
	}
	// The reconciler recomputes the same answer.
	if err := PublishBeamIssuers(ctx, fs, rdb, 0, "mg-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := bw(); !ok {
		t.Fatal("a reconcile dropped the throttle")
	}

	fs.usage["mg-1"] = &store.TrafficUsage{} // the month rolled
	agg.evaluateDataThrottle(ctx, owners)
	if _, ok := bw(); ok || IsDataThrottled(ctx, rdb, "mg-1") {
		t.Fatal("release left bw_bps in the issuer record")
	}
}

// A BYON tenant keeps the old data verdict: relay against the relay row times
// units, no pool, and its issuer-less account never gets a record.
func TestByonVerdictUnchanged(t *testing.T) {
	hundred := int64(100)
	fs := &trafficFakeStore{
		owners:      map[string]string{"srv-1": "byon-a"},
		dataLimit:   &models.TrafficLimit{IncludedGB: &hundred},
		dataUsage:   map[string][]store.RegionUsage{"byon-a": {{Region: "eu-central", Kind: store.TrafficKindRelay, Bytes: 120 * poolGB}}},
		usage:       map[string]*store.TrafficUsage{"byon-a": {EdgeBytes: 5000 * poolGB, RelayBytes: 120 * poolGB}},
		dataConsent: map[string]bool{},
	}
	if _, managed, err := ManagedPoolVerdictFor(fs, "byon-a", time.Now()); managed || err != nil {
		t.Fatalf("BYON read as managed (%v, %v)", managed, err)
	}
	want := JudgeDataTraffic(120*poolGB, &hundred, i64(1), false)
	got, err := DataTrafficVerdictFor(fs, "byon-a", time.Now())
	if err != nil || got != want {
		t.Fatalf("verdict %+v err %v, want %+v", got, err, want)
	}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	ctx := context.Background()
	agg.evaluateDataThrottle(ctx, fs.owners)
	if v, _ := rdb.Get(ctx, DataBandwidthKey("srv-1")).Int64(); v != DataThrottleDefaultMbit*125_000 {
		t.Fatalf("BYON throttle = %d", v)
	}
	if n, _ := rdb.Keys(ctx, BeamIssuerKeyPrefix+"*").Result(); len(n) != 0 {
		t.Fatalf("issuer records %v for a BYON tenant", n)
	}
}
