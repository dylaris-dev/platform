package services

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// graceFixture is one managed tenant (100 GB included, 50 GB purchasable) with
// an issuer key, a beam owner subject and one server of their own.
func graceFixture(t *testing.T, consent bool) (*trafficFakeStore, *TrafficAggregator, *redis.Client, map[string]string) {
	t.Helper()
	fs := &trafficFakeStore{
		owners:       map[string]string{"mg-1": "mg-1"},
		issuerKeys:   []models.BeamIssuerKey{{ID: beamKidA1, OwnerID: "mg-1", PublicKey: make([]byte, 32)}},
		managedLimit: map[string]*models.TrafficLimit{"mg-1": {IncludedGB: i64(100), MaxPurchaseGB: i64(50)}},
		billing:      map[string]*store.UserBilling{"mg-1": {UserID: "mg-1", Status: "active", TrafficBillingEnabled: consent}},
		dataConsent:  map[string]bool{},
	}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	fs.settings[BeamTenantMaxConnsKey] = ""
	return fs, agg, rdb, map[string]string{"owner:mg-1": "mg-1", "srv-9": "mg-1"}
}

// issuerLimits reads bw_bps and daily_quota_bytes off the published record;
// -1 means the field is absent.
func issuerLimits(t *testing.T, rdb *redis.Client) (bw, quota int64) {
	t.Helper()
	raw, err := rdb.Get(context.Background(), BeamIssuerKeyPrefix+beamKidA1).Bytes()
	if err != nil {
		t.Fatalf("issuer record: %v", err)
	}
	var r struct {
		Bw    *int64 `json:"bw_bps"`
		Quota *int64 `json:"daily_quota_bytes"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	bw, quota = -1, -1
	if r.Bw != nil {
		bw = *r.Bw
	}
	if r.Quota != nil {
		quota = *r.Quota
	}
	return bw, quota
}

func TestGraceModeEntersAtIncludedWithoutConsent(t *testing.T) {
	for _, c := range []struct {
		name         string
		used         int64
		consent      bool
		grace, thrtl bool
	}{
		{"below included", 99 * poolGB, false, false, false},
		{"at included", 100 * poolGB, false, true, true},
		{"far past included", 900 * poolGB, false, true, true},
		{"consent at included", 100 * poolGB, true, false, false},
		{"consent at the store cap", 150 * poolGB, true, false, true},
	} {
		v := JudgeManagedPool(c.used, i64(100), i64(50), c.consent)
		if v.Grace != c.grace || v.Throttled != c.thrtl {
			t.Errorf("%s: grace %v throttled %v, want %v %v", c.name, v.Grace, v.Throttled, c.grace, c.thrtl)
		}
	}

	fs, agg, rdb, owners := graceFixture(t, false)
	ctx := context.Background()
	fs.usage = map[string]*store.TrafficUsage{"mg-1": {EdgeBytes: 60 * poolGB, RelayBytes: 39 * poolGB}}
	agg.evaluateDataThrottle(ctx, owners)
	if fs.usage["mg-1"].GraceSinceRelay != nil {
		t.Fatal("grace opened below the included amount")
	}

	fs.usage["mg-1"].RelayBytes = 40 * poolGB
	agg.evaluateDataThrottle(ctx, owners)
	if s := fs.usage["mg-1"].GraceSinceRelay; s == nil || *s != 40*poolGB {
		t.Fatalf("grace snapshot %v, want the 40 GB of relay bytes at entry", s)
	}
	if bw, quota := issuerLimits(t, rdb); bw != 6_250_000 || quota != 30_000_000_000 {
		t.Fatalf("grace record bw_bps %d daily_quota_bytes %d, want 6250000 / 30000000000", bw, quota)
	}
	if v, _ := rdb.Get(ctx, DataBandwidthKey("srv-9")).Int64(); v != GraceBps {
		t.Fatalf("own server data_bw %d, want the fixed grace %d", v, GraceBps)
	}
	// The setting does not move the grace speed.
	fs.settings[DataThrottleSettingKey] = "200"
	if err := PublishBeamIssuers(ctx, fs, rdb, 0, "mg-1"); err != nil {
		t.Fatal(err)
	}
	if bw, _ := issuerLimits(t, rdb); bw != GraceBps {
		t.Fatalf("grace bw_bps followed the setting: %d", bw)
	}
}

// Bytes after grace entry stay in RelayBytes (the stats) and out of the pool,
// so the pool never grows past where grace began.
func TestGraceBytesExcludedFromPoolButInStats(t *testing.T) {
	fs, agg, _, owners := graceFixture(t, false)
	ctx := context.Background()
	fs.usage = map[string]*store.TrafficUsage{"mg-1": {EdgeBytes: 60 * poolGB, RelayBytes: 40 * poolGB}}
	agg.evaluateDataThrottle(ctx, owners) // enters grace at 40 GB relay

	fs.usage["mg-1"].RelayBytes += 500 * poolGB // 500 GB of beam in grace mode
	agg.evaluateDataThrottle(ctx, owners)
	u := fs.usage["mg-1"]
	if u.RelayBytes != 540*poolGB {
		t.Fatalf("stats relay bytes %d, want the full 540 GB", u.RelayBytes)
	}
	if g := u.GraceRelayBytes(); g != 500*poolGB {
		t.Fatalf("grace bytes %d, want 500 GB", g)
	}
	v, _, err := ManagedPoolVerdictFor(fs, "mg-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if v.UsedGB != 100 || !v.Grace {
		t.Fatalf("pool %d GB grace %v, want 100 GB (players 60 + beam before grace 40) in grace", v.UsedGB, v.Grace)
	}
	if p := ManagedPoolBytes(u); p != 100*poolGB {
		t.Fatalf("pool bytes %d, want 100 GB", p)
	}
}

// Consent ends grace; its bytes stay out of the pool for the month, so the
// store's consent baseline and every later bill see only what came after.
func TestGraceNeverBilledAfterConsent(t *testing.T) {
	fs, agg, rdb, owners := graceFixture(t, false)
	ctx := context.Background()
	fs.usage = map[string]*store.TrafficUsage{"mg-1": {EdgeBytes: 60 * poolGB, RelayBytes: 40 * poolGB}}
	agg.evaluateDataThrottle(ctx, owners)
	fs.usage["mg-1"].RelayBytes += 500 * poolGB

	fs.billing["mg-1"].TrafficBillingEnabled = true
	agg.evaluateDataThrottle(ctx, owners)
	u := fs.usage["mg-1"]
	if u.GraceSinceRelay != nil || u.GraceExcludedBytes != 500*poolGB {
		t.Fatalf("after consent: since %v excluded %d, want closed with 500 GB kept out", u.GraceSinceRelay, u.GraceExcludedBytes)
	}
	baseline := ManagedPoolBytes(u)
	if baseline != 100*poolGB {
		t.Fatalf("consent baseline %d, want 100 GB without the grace bytes", baseline)
	}
	if bw, quota := issuerLimits(t, rdb); bw != -1 || quota != -1 {
		t.Fatalf("consent inside the store cap still limited: bw %d quota %d", bw, quota)
	}

	u.RelayBytes += 30 * poolGB // billable now
	if got := ManagedPoolBytes(u) - baseline; got != 30*poolGB {
		t.Fatalf("billable growth %d, want only the 30 GB after consent", got)
	}
	// Consent off again later the same month: a new spell, the old one kept.
	fs.billing["mg-1"].TrafficBillingEnabled = false
	agg.evaluateDataThrottle(ctx, owners)
	u.RelayBytes += 10 * poolGB
	if g := u.GraceRelayBytes(); g != 510*poolGB {
		t.Fatalf("grace bytes %d after a second spell, want 510 GB", g)
	}
}

// With consent nothing changes: no grace spell, no daily quota, the configured
// throttle only past included + max purchase.
func TestConsentSkipsGrace(t *testing.T) {
	fs, agg, rdb, owners := graceFixture(t, true)
	ctx := context.Background()
	fs.usage = map[string]*store.TrafficUsage{"mg-1": {EdgeBytes: 100 * poolGB, RelayBytes: 20 * poolGB}}
	agg.evaluateDataThrottle(ctx, owners)
	if err := PublishBeamIssuers(ctx, fs, rdb, 0, "mg-1"); err != nil {
		t.Fatal(err)
	}
	if fs.usage["mg-1"].GraceSinceRelay != nil {
		t.Fatal("grace opened with consent")
	}
	if bw, quota := issuerLimits(t, rdb); bw != -1 || quota != -1 {
		t.Fatalf("consent past included: bw %d quota %d, want neither", bw, quota)
	}

	fs.usage["mg-1"].RelayBytes = 50 * poolGB // at the store cap
	agg.evaluateDataThrottle(ctx, owners)
	if bw, quota := issuerLimits(t, rdb); bw != DataThrottleDefaultMbit*125_000 || quota != -1 {
		t.Fatalf("consent at the store cap: bw %d quota %d, want the setting and no quota", bw, quota)
	}
	if fs.usage["mg-1"].GraceSinceRelay != nil {
		t.Fatal("grace opened with consent at the store cap")
	}
}
