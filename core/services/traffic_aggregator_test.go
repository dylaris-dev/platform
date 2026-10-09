package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// trafficFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only the methods TrafficAggregator touches are
// overridden. Any other call would panic - the tests never make one.
type trafficFakeStore struct {
	store.Store

	owners     map[string]string
	ownersErr  error
	ownersCall int

	routes    []store.CoreLinkRoute
	routesErr error

	backupBytes    map[string]int64
	backupBytesErr error

	settings map[string]string

	addUsageCalls   []trafficUsageCall
	addUsageErrFor  map[string]error
	addRegionCalls  []trafficRegionCall
	addRegionErrFor map[string]error
	setBackupCalls  []trafficBackupCall
	setBackupErrFor map[string]error
}

type trafficUsageCall struct {
	tenant           string
	period           time.Time
	edge, relayBytes int64
}
type trafficRegionCall struct {
	tenant  string
	period  time.Time
	region  string
	kind    string
	product string
	bytes   int64
}
type trafficBackupCall struct {
	tenant string
	period time.Time
	bytes  int64
}

func (f *trafficFakeStore) TenantServerOwners() (map[string]string, error) {
	f.ownersCall++
	return f.owners, f.ownersErr
}

// Route-only addresses have no server row, so their counters are keyed on the
// owner and this is where the aggregator learns those owners exist.
func (f *trafficFakeStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) {
	return f.routes, f.routesErr
}

func (f *trafficFakeStore) TenantBackupBytes() (map[string]int64, error) {
	return f.backupBytes, f.backupBytesErr
}

func (f *trafficFakeStore) AddTrafficUsageRegion(userID string, period time.Time, region, kind, product string, bytes int64) error {
	f.addRegionCalls = append(f.addRegionCalls, trafficRegionCall{userID, period, region, kind, product, bytes})
	if f.addRegionErrFor != nil {
		return f.addRegionErrFor[region]
	}
	return nil
}

func (f *trafficFakeStore) AddTrafficUsage(userID string, period time.Time, edgeBytes, relayBytes int64) error {
	f.addUsageCalls = append(f.addUsageCalls, trafficUsageCall{userID, period, edgeBytes, relayBytes})
	if f.addUsageErrFor != nil {
		return f.addUsageErrFor[userID]
	}
	return nil
}

func (f *trafficFakeStore) SetTrafficBackupBytes(userID string, period time.Time, backupBytes int64) error {
	f.setBackupCalls = append(f.setBackupCalls, trafficBackupCall{userID, period, backupBytes})
	if f.setBackupErrFor != nil {
		return f.setBackupErrFor[userID]
	}
	return nil
}

func (f *trafficFakeStore) GetSetting(key string) (string, error) {
	v, ok := f.settings[key]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}

func newTrafficAggregatorTest(t *testing.T, fs *trafficFakeStore, byonEnabled bool) (*TrafficAggregator, *redis.Client) {
	t.Helper()
	rdb := newQueueTestRedis(t)
	if fs.settings == nil {
		fs.settings = map[string]string{}
	}
	fs.settings["feature_byon_enabled"] = "false"
	if byonEnabled {
		fs.settings["feature_byon_enabled"] = "true"
	}
	flags := NewFeatureFlags(fs)
	agg := NewTrafficAggregator(fs, rdb, flags)
	return agg, rdb
}

func mustHSet(t *testing.T, rdb *redis.Client, key string, fields map[string]interface{}) {
	t.Helper()
	if err := rdb.HSet(context.Background(), key, fields).Err(); err != nil {
		t.Fatalf("HSet %s: %v", key, err)
	}
}

func mustSet(t *testing.T, rdb *redis.Client, key, value string) {
	t.Helper()
	if err := rdb.Set(context.Background(), key, value, 0).Err(); err != nil {
		t.Fatalf("Set %s: %v", key, err)
	}
}

func assertRedisString(t *testing.T, rdb *redis.Client, key, want string) {
	t.Helper()
	got, err := rdb.Get(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("Get %s: %v", key, err)
	}
	if got != want {
		t.Errorf("%s = %q, want %q", key, got, want)
	}
}

func TestTrafficAggregator_BYONDisabled_NoOp(t *testing.T) {
	fs := &trafficFakeStore{owners: map[string]string{"srv-1": "tenant-a"}}
	agg, _ := newTrafficAggregatorTest(t, fs, false)

	agg.runOnce(context.Background())

	if fs.ownersCall != 0 {
		t.Errorf("TenantServerOwners called %d times, want 0 when BYON is disabled", fs.ownersCall)
	}
	if len(fs.addUsageCalls) != 0 {
		t.Errorf("expected no AddTrafficUsage calls, got %+v", fs.addUsageCalls)
	}
}

func TestTrafficAggregator_NoTenants_NoOp(t *testing.T) {
	fs := &trafficFakeStore{owners: map[string]string{}}
	agg, _ := newTrafficAggregatorTest(t, fs, true)

	agg.runOnce(context.Background())

	if len(fs.addUsageCalls) != 0 {
		t.Errorf("expected no AddTrafficUsage calls with zero tenants, got %+v", fs.addUsageCalls)
	}
	if len(fs.setBackupCalls) != 0 {
		t.Errorf("expected no SetTrafficBackupBytes calls with zero tenants, got %+v", fs.setBackupCalls)
	}
}

func TestTrafficAggregator_ComputesDeltaAcrossEdgeAndRelay(t *testing.T) {
	fs := &trafficFakeStore{
		owners: map[string]string{
			"srv-uuid-1": "tenant-a",
			"srv-uuid-2": "tenant-b",
		},
		backupBytes: map[string]int64{},
	}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	ctx := context.Background()

	mustHSet(t, rdb, "dylaris:traffic:edge:srv-uuid-1", map[string]interface{}{"rx": "1000", "tx": "2000"}) // current 3000
	mustSet(t, rdb, "dylaris:traffic:agg:seen:edge:srv-uuid-1", "1000")                                     // seen 1000 -> delta 2000

	mustHSet(t, rdb, "dylaris:traffic:edge:srv-uuid-2", map[string]interface{}{"rx": "500"}) // current 500, no seen -> delta 500

	mustHSet(t, rdb, "dylaris:traffic:relay:srv-uuid-1", map[string]interface{}{"up": "100", "down": "200"}) // current 300, no seen -> delta 300

	// Not in owners -> must not be billed to anyone.
	mustHSet(t, rdb, "dylaris:traffic:edge:unknown-uuid", map[string]interface{}{"rx": "999"})

	agg.runOnce(ctx)

	byTenant := map[string]trafficUsageCall{}
	for _, c := range fs.addUsageCalls {
		byTenant[c.tenant] = c
	}
	if len(byTenant) != 2 {
		t.Fatalf("addUsageCalls = %+v, want exactly tenant-a and tenant-b", fs.addUsageCalls)
	}
	if a := byTenant["tenant-a"]; a.edge != 2000 || a.relayBytes != 300 {
		t.Errorf("tenant-a usage = %+v, want edge=2000 relay=300", a)
	}
	if b := byTenant["tenant-b"]; b.edge != 500 || b.relayBytes != 0 {
		t.Errorf("tenant-b usage = %+v, want edge=500 relay=0", b)
	}

	wantPeriod := monthStartUTC(time.Now())
	if !byTenant["tenant-a"].period.Equal(wantPeriod) {
		t.Errorf("period = %v, want %v", byTenant["tenant-a"].period, wantPeriod)
	}

	// Seen keys advance to the new "current" totals so the same bytes are not re-billed.
	assertRedisString(t, rdb, "dylaris:traffic:agg:seen:edge:srv-uuid-1", "3000")
	assertRedisString(t, rdb, "dylaris:traffic:agg:seen:edge:srv-uuid-2", "500")
	assertRedisString(t, rdb, "dylaris:traffic:agg:seen:relay:srv-uuid-1", "300")
}

func TestTrafficAggregator_NegativeDeltaTreatedAsCounterReset(t *testing.T) {
	fs := &trafficFakeStore{
		owners:      map[string]string{"srv-uuid-1": "tenant-a"},
		backupBytes: map[string]int64{},
	}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	ctx := context.Background()

	// seen (5000) > current (200): the counter key must have expired and been
	// recreated from zero - the whole current value is billed, not a negative delta.
	mustHSet(t, rdb, "dylaris:traffic:edge:srv-uuid-1", map[string]interface{}{"rx": "200"})
	mustSet(t, rdb, "dylaris:traffic:agg:seen:edge:srv-uuid-1", "5000")

	agg.runOnce(ctx)

	if len(fs.addUsageCalls) != 1 || fs.addUsageCalls[0].edge != 200 {
		t.Fatalf("addUsageCalls = %+v, want a single call with edge=200", fs.addUsageCalls)
	}
}

func TestTrafficAggregator_ZeroDeltaSkipped(t *testing.T) {
	fs := &trafficFakeStore{
		owners:      map[string]string{"srv-uuid-1": "tenant-a"},
		backupBytes: map[string]int64{},
	}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	ctx := context.Background()

	mustHSet(t, rdb, "dylaris:traffic:edge:srv-uuid-1", map[string]interface{}{"rx": "500"})
	mustSet(t, rdb, "dylaris:traffic:agg:seen:edge:srv-uuid-1", "500") // delta = 0

	agg.runOnce(ctx)

	if len(fs.addUsageCalls) != 0 {
		t.Errorf("expected no billing call for a zero delta, got %+v", fs.addUsageCalls)
	}
}

func TestTrafficAggregator_AddTrafficUsageError_LeavesSeenUntouchedForRetry(t *testing.T) {
	fs := &trafficFakeStore{
		owners:         map[string]string{"srv-uuid-1": "tenant-a"},
		backupBytes:    map[string]int64{},
		addUsageErrFor: map[string]error{"tenant-a": errors.New("db down")},
	}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	ctx := context.Background()

	mustHSet(t, rdb, "dylaris:traffic:edge:srv-uuid-1", map[string]interface{}{"rx": "1000"})
	mustSet(t, rdb, "dylaris:traffic:agg:seen:edge:srv-uuid-1", "200") // delta 800, but the DB write will fail

	agg.runOnce(ctx)

	if len(fs.addUsageCalls) != 1 {
		t.Fatalf("expected exactly one attempted AddTrafficUsage call, got %+v", fs.addUsageCalls)
	}
	// seen must remain at the pre-tick value so the same 800-byte delta is retried next tick.
	assertRedisString(t, rdb, "dylaris:traffic:agg:seen:edge:srv-uuid-1", "200")
}

func TestTrafficAggregator_SnapshotBackupStorage_UnionOfOwnersAndBackupTenants(t *testing.T) {
	fs := &trafficFakeStore{
		owners:      map[string]string{"srv-1": "tenant-a"},
		backupBytes: map[string]int64{"tenant-a": 500, "tenant-b": 999}, // tenant-b kept backups after deleting all servers
	}
	agg, _ := newTrafficAggregatorTest(t, fs, true)

	agg.runOnce(context.Background())

	byTenant := map[string]int64{}
	for _, c := range fs.setBackupCalls {
		byTenant[c.tenant] = c.bytes
	}
	if len(byTenant) != 2 {
		t.Fatalf("setBackupCalls = %+v, want both tenant-a and tenant-b", fs.setBackupCalls)
	}
	if byTenant["tenant-a"] != 500 || byTenant["tenant-b"] != 999 {
		t.Errorf("backup snapshot = %+v, want tenant-a=500 tenant-b=999", byTenant)
	}
}

func TestTrafficAggregator_TenantBackupBytesError_DoesNotBlockTrafficBilling(t *testing.T) {
	fs := &trafficFakeStore{
		owners:         map[string]string{"srv-uuid-1": "tenant-a"},
		backupBytesErr: errors.New("db down"),
	}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	ctx := context.Background()
	mustHSet(t, rdb, "dylaris:traffic:edge:srv-uuid-1", map[string]interface{}{"rx": "1000"})

	agg.runOnce(ctx)

	if len(fs.addUsageCalls) != 1 {
		t.Errorf("traffic billing must proceed even when the backup-bytes lookup fails, got %+v", fs.addUsageCalls)
	}
	if len(fs.setBackupCalls) != 0 {
		t.Errorf("expected no backup-snapshot calls when TenantBackupBytes errors, got %+v", fs.setBackupCalls)
	}
}

func TestSumByteFields(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		want   int64
	}{
		{"empty", map[string]string{}, 0},
		{"single field", map[string]string{"rx": "100"}, 100},
		{"multiple fields summed", map[string]string{"rx": "100", "tx": "250"}, 350},
		{"unparseable field ignored", map[string]string{"rx": "100", "label": "not-a-number"}, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sumByteFields(c.fields); got != c.want {
				t.Errorf("sumByteFields(%v) = %d, want %d", c.fields, got, c.want)
			}
		})
	}
}

func TestMonthStartUTC(t *testing.T) {
	loc := time.FixedZone("UTC+9", 9*60*60)
	in := time.Date(2026, 7, 15, 23, 30, 0, 0, loc) // 2026-07-15 14:30 UTC
	want := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if got := monthStartUTC(in); !got.Equal(want) {
		t.Errorf("monthStartUTC(%v) = %v, want %v", in, got, want)
	}
}

// A route-only address carries player traffic and has NO server behind it:
// Core writes server_uuid:"" for every one, so the edge meters it under
// "owner:<userID>" instead. This is the Core half of that - without it the
// counter exists in Redis and matches no tenant, which collect skips silently.
func TestTrafficAggregator_RouteOnlyBilledToItsOwner(t *testing.T) {
	fs := &trafficFakeStore{
		owners: map[string]string{},
		routes: []store.CoreLinkRoute{
			{Domain: "a.eu.dylaris.com", OwnerID: "tenant-r"},
		},
	}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	mustHSet(t, rdb, "dylaris:traffic:edge:owner:tenant-r", map[string]interface{}{"rx": 400, "tx": 600})

	agg.runOnce(context.Background())

	if len(fs.addUsageCalls) != 1 {
		t.Fatalf("want exactly one usage row, got %+v", fs.addUsageCalls)
	}
	c := fs.addUsageCalls[0]
	if c.tenant != "tenant-r" || c.edge != 1000 {
		t.Errorf("got tenant=%q edge=%d, want tenant-r / 1000", c.tenant, c.edge)
	}
	// The seen marker has to carry the full subject, or the next tick re-bills
	// the same bytes against a marker written for a different key.
	assertRedisString(t, rdb, "dylaris:traffic:agg:seen:edge:owner:tenant-r", "1000")
}

// A tenant who bought ONLY route-only owns no server, so TenantServerOwners is
// empty for them. runOnce used to return on that emptiness, which meant the one
// product whose traffic was unmetered was also the one the aggregator refused
// to run for. Both halves had to change.
func TestTrafficAggregator_RouteOnlyTenantWithNoServers(t *testing.T) {
	fs := &trafficFakeStore{
		owners: map[string]string{},
		routes: []store.CoreLinkRoute{{Domain: "solo.eu.dylaris.com", OwnerID: "tenant-solo"}},
	}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	mustHSet(t, rdb, "dylaris:traffic:edge:owner:tenant-solo", map[string]interface{}{"rx": 7, "tx": 3})

	agg.runOnce(context.Background())

	if len(fs.addUsageCalls) != 1 || fs.addUsageCalls[0].tenant != "tenant-solo" {
		t.Fatalf("route-only-only tenant was not billed: %+v", fs.addUsageCalls)
	}
}

// A counter whose route has been deleted must not invent a row. Same rule the
// server side already had: resolve or skip, never guess the tenant from the key.
func TestTrafficAggregator_UnknownOwnerSubjectSkipped(t *testing.T) {
	fs := &trafficFakeStore{
		owners: map[string]string{"srv-1": "tenant-a"},
		routes: nil,
	}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	mustHSet(t, rdb, "dylaris:traffic:edge:owner:tenant-gone", map[string]interface{}{"rx": 5, "tx": 5})

	agg.runOnce(context.Background())

	for _, c := range fs.addUsageCalls {
		if c.tenant == "tenant-gone" {
			t.Fatalf("billed a subject with no route behind it: %+v", c)
		}
	}
}

// Both kinds in one tick land on one row for the same tenant, because the
// allowance is sold per ACCOUNT, not per subject.
func TestTrafficAggregator_ServerAndRouteOnlySumIntoOneRow(t *testing.T) {
	fs := &trafficFakeStore{
		owners: map[string]string{"srv-1": "tenant-a"},
		routes: []store.CoreLinkRoute{{Domain: "a.eu.dylaris.com", OwnerID: "tenant-a"}},
	}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	mustHSet(t, rdb, "dylaris:traffic:edge:srv-1", map[string]interface{}{"rx": 100, "tx": 100})
	mustHSet(t, rdb, "dylaris:traffic:edge:owner:tenant-a", map[string]interface{}{"rx": 50, "tx": 50})

	agg.runOnce(context.Background())

	if len(fs.addUsageCalls) != 1 {
		t.Fatalf("want one row for the tenant, got %+v", fs.addUsageCalls)
	}
	if got := fs.addUsageCalls[0].edge; got != 300 {
		t.Errorf("edge bytes = %d, want 300 (200 server + 100 route-only)", got)
	}
}

// regionBytes is the whole breakdown in one function: whatever it returns must
// sum to what sumByteFields returns, or the split does not add up to the bill.
func TestRegionBytes(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]string
		want   map[string]int64
	}{
		{
			name:   "tagged fields split by region",
			fields: map[string]string{"rx:eu-central": "100", "tx:eu-central": "200", "rx:ap-southeast": "5"},
			want:   map[string]int64{"eu-central": 300, "ap-southeast": 5},
		},
		{
			// An edge that predates region tagging. Its bytes are real and are
			// in the total, so they must appear in the breakdown too.
			name:   "untagged fields land in unknown",
			fields: map[string]string{"rx": "10", "tx": "20"},
			want:   map[string]int64{"unknown": 30},
		},
		{
			// What a rolling edge upgrade looks like: one edge tagged, one not,
			// writing into the same subject's hash.
			name:   "mixed shapes coexist",
			fields: map[string]string{"rx": "10", "rx:eu-central": "40", "tx:eu-central": "60"},
			want:   map[string]int64{"unknown": 10, "eu-central": 100},
		},
		{
			name:   "unparseable fields are ignored, not counted as zero regions",
			fields: map[string]string{"rx:eu-central": "7", "note": "hello"},
			want:   map[string]int64{"eu-central": 7},
		},
		{
			name:   "empty region falls back to unknown",
			fields: map[string]string{"rx:": "9"},
			want:   map[string]int64{"unknown": 9},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := regionBytes(tt.fields)
			if len(got) != len(tt.want) {
				t.Fatalf("regionBytes = %v, want %v", got, tt.want)
			}
			var sum int64
			for r, v := range tt.want {
				if got[r] != v {
					t.Errorf("region %q = %d, want %d", r, got[r], v)
				}
				sum += v
			}
			// The invariant the breakdown rests on.
			if total := sumByteFields(tt.fields); total != sum {
				t.Errorf("breakdown sums to %d but sumByteFields says %d - the split would not match the bill", sum, total)
			}
		})
	}
}

func TestTrafficAggregator_SplitsEdgeBytesByRegion(t *testing.T) {
	fs := &trafficFakeStore{owners: map[string]string{"srv-1": "tenant-a"}}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	mustHSet(t, rdb, "dylaris:traffic:edge:srv-1", map[string]interface{}{
		"rx:eu-central": 100, "tx:eu-central": 300, "rx:ap-southeast": 600,
	})

	agg.runOnce(context.Background())

	if len(fs.addUsageCalls) != 1 || fs.addUsageCalls[0].edge != 1000 {
		t.Fatalf("total row wrong: %+v", fs.addUsageCalls)
	}
	got := map[string]int64{}
	for _, c := range fs.addRegionCalls {
		got[c.region] += c.bytes
	}
	if got["eu-central"] != 400 || got["ap-southeast"] != 600 {
		t.Fatalf("region rows = %v, want eu-central 400 / ap-southeast 600", got)
	}
	var sum int64
	for _, v := range got {
		sum += v
	}
	if sum != fs.addUsageCalls[0].edge {
		t.Errorf("region rows sum to %d but the billed total is %d", sum, fs.addUsageCalls[0].edge)
	}
}

// The upgrade case, and the one that would have been a real defect: a subject
// already being counted has a total seen marker but no region markers yet.
// Treating the missing markers as deltas would post its entire historical
// counter into the breakdown on the first tick, so the split would claim
// hundreds of GB in a month whose bill moved by a few MB.
func TestTrafficAggregator_ExistingSubjectSeedsRegionsWithoutBilling(t *testing.T) {
	fs := &trafficFakeStore{owners: map[string]string{"srv-1": "tenant-a"}}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	mustHSet(t, rdb, "dylaris:traffic:edge:srv-1", map[string]interface{}{"rx:eu-central": 900, "tx:eu-central": 100})
	// This Core has billed this subject before, up to 950 bytes.
	mustSet(t, rdb, "dylaris:traffic:agg:seen:edge:srv-1", "950")

	agg.runOnce(context.Background())

	if len(fs.addUsageCalls) != 1 || fs.addUsageCalls[0].edge != 50 {
		t.Fatalf("total should have moved by 50, got %+v", fs.addUsageCalls)
	}
	if len(fs.addRegionCalls) != 0 {
		t.Fatalf("first tick must seed the region marker, not bill it: %+v", fs.addRegionCalls)
	}
	assertRedisString(t, rdb, "dylaris:traffic:agg:seen:edge:srv-1#eu-central", "1000")

	// Second tick: now it accumulates normally from the seeded point.
	fs.addRegionCalls = nil
	mustHSet(t, rdb, "dylaris:traffic:edge:srv-1", map[string]interface{}{"rx:eu-central": 1200})
	agg.runOnce(context.Background())
	if len(fs.addRegionCalls) != 1 || fs.addRegionCalls[0].bytes != 300 {
		t.Fatalf("second tick should bill the 300 new bytes to the region, got %+v", fs.addRegionCalls)
	}
}

// A brand-new subject has no total marker either, so it must NOT be seeded -
// its first region delta is its full counter, exactly as the total does.
func TestTrafficAggregator_NewSubjectBillsRegionsImmediately(t *testing.T) {
	fs := &trafficFakeStore{owners: map[string]string{"srv-new": "tenant-a"}}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	mustHSet(t, rdb, "dylaris:traffic:edge:srv-new", map[string]interface{}{"rx:eu-central": 40, "tx:eu-central": 60})

	agg.runOnce(context.Background())

	if len(fs.addRegionCalls) != 1 || fs.addRegionCalls[0].region != "eu-central" || fs.addRegionCalls[0].bytes != 100 {
		t.Fatalf("new subject should bill its regions at once, got %+v", fs.addRegionCalls)
	}
}

// A failing region write must not take the bill with it: the total is what the
// tenant is charged on, and it has already been written at that point.
func TestTrafficAggregator_RegionWriteFailureKeepsTheTotal(t *testing.T) {
	fs := &trafficFakeStore{
		owners:          map[string]string{"srv-1": "tenant-a"},
		addRegionErrFor: map[string]error{"eu-central": errors.New("boom")},
	}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	mustHSet(t, rdb, "dylaris:traffic:edge:srv-1", map[string]interface{}{"rx:eu-central": 500})

	agg.runOnce(context.Background())

	if len(fs.addUsageCalls) != 1 || fs.addUsageCalls[0].edge != 500 {
		t.Fatalf("the billing total must still be written: %+v", fs.addUsageCalls)
	}

	// And the markers must still advance. If a failing region write skipped
	// them, the next tick would re-read the same counter as new bytes and bill
	// the tenant a second time for traffic they used once - a broken breakdown
	// turning into a wrong invoice, which is the one thing the write order
	// exists to prevent.
	agg.runOnce(context.Background())
	if len(fs.addUsageCalls) != 1 {
		t.Fatalf("the same bytes were billed again after a region write failed: %+v", fs.addUsageCalls)
	}
}

// The case that made the kind column necessary. A customer whose players
// connect through a US edge still has their file transfers carried by a relay
// near the node, and every relay is in eu-central today. One account, two
// regions, two costs - and before the split the second one had nowhere to land.
func TestTrafficAggregator_PlayerAndDataTrafficLandInDifferentRegions(t *testing.T) {
	fs := &trafficFakeStore{owners: map[string]string{"srv-1": "tenant-a"}}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	mustHSet(t, rdb, "dylaris:traffic:edge:srv-1", map[string]interface{}{"rx:us-east": 200, "tx:us-east": 800})
	mustHSet(t, rdb, "dylaris:traffic:relay:srv-1", map[string]interface{}{"up:eu-central": 30, "down:eu-central": 70})

	agg.runOnce(context.Background())

	if len(fs.addUsageCalls) != 1 {
		t.Fatalf("want one total row, got %+v", fs.addUsageCalls)
	}
	if got := fs.addUsageCalls[0]; got.edge != 1000 || got.relayBytes != 100 {
		t.Fatalf("total row = edge %d / relay %d, want 1000 / 100", got.edge, got.relayBytes)
	}

	type cell struct{ region, kind string }
	got := map[cell]int64{}
	for _, c := range fs.addRegionCalls {
		got[cell{c.region, c.kind}] += c.bytes
	}
	if got[cell{"us-east", "edge"}] != 1000 {
		t.Errorf("player traffic did not land in us-east: %v", got)
	}
	if got[cell{"eu-central", "relay"}] != 100 {
		t.Errorf("data traffic did not land in eu-central: %v", got)
	}
	if len(got) != 2 {
		t.Errorf("want exactly two cells, got %v", got)
	}
}

// Per kind, the breakdown must sum to the number that kind is billed on.
// Anything else means an allowance checked against the split would disagree
// with the invoice built from the total.
func TestTrafficAggregator_BreakdownSumsToTheTotalPerKind(t *testing.T) {
	fs := &trafficFakeStore{owners: map[string]string{"srv-1": "tenant-a", "srv-2": "tenant-a"}}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	mustHSet(t, rdb, "dylaris:traffic:edge:srv-1", map[string]interface{}{"rx:eu-central": 10, "tx:ap-southeast": 90})
	mustHSet(t, rdb, "dylaris:traffic:edge:srv-2", map[string]interface{}{"rx": 400}) // untagged producer
	mustHSet(t, rdb, "dylaris:traffic:relay:srv-1", map[string]interface{}{"up:eu-central": 25})

	agg.runOnce(context.Background())

	perKind := map[string]int64{}
	for _, c := range fs.addRegionCalls {
		perKind[c.kind] += c.bytes
	}
	total := fs.addUsageCalls[0]
	if perKind["edge"] != total.edge {
		t.Errorf("edge breakdown sums to %d, billed total is %d", perKind["edge"], total.edge)
	}
	if perKind["relay"] != total.relayBytes {
		t.Errorf("relay breakdown sums to %d, billed total is %d", perKind["relay"], total.relayBytes)
	}
	// The untagged producer's bytes must be present, under "unknown".
	var unknown int64
	for _, c := range fs.addRegionCalls {
		if c.region == "unknown" {
			unknown += c.bytes
		}
	}
	if unknown != 400 {
		t.Errorf("untagged bytes = %d, want 400 under region \"unknown\"", unknown)
	}
}

// Same server, same region, both kinds. The seen marker has to carry the KIND
// as well as the subject and the region, or the relay's marker overwrites the
// edge's and the next tick recomputes both deltas against the wrong number -
// silently re-billing traffic that was already counted, or losing it.
func TestTrafficAggregator_SeenMarkersDoNotCollideAcrossKinds(t *testing.T) {
	fs := &trafficFakeStore{owners: map[string]string{"srv-1": "tenant-a"}}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	mustHSet(t, rdb, "dylaris:traffic:edge:srv-1", map[string]interface{}{"rx:eu-central": 1000})
	mustHSet(t, rdb, "dylaris:traffic:relay:srv-1", map[string]interface{}{"up:eu-central": 7})

	agg.runOnce(context.Background())

	first := map[string]int64{}
	for _, c := range fs.addRegionCalls {
		first[c.kind] += c.bytes
	}
	if first["edge"] != 1000 || first["relay"] != 7 {
		t.Fatalf("first tick = %v, want edge 1000 / relay 7", first)
	}

	// Second tick WITH movement. A collided marker only shows up here: with the
	// counters unchanged the total delta is zero and the regions are never
	// re-examined, so the collision hides behind the short-circuit.
	fs.addRegionCalls = nil
	fs.addUsageCalls = nil
	mustHSet(t, rdb, "dylaris:traffic:edge:srv-1", map[string]interface{}{"rx:eu-central": 1500})
	mustHSet(t, rdb, "dylaris:traffic:relay:srv-1", map[string]interface{}{"up:eu-central": 9})

	agg.runOnce(context.Background())

	second := map[string]int64{}
	for _, c := range fs.addRegionCalls {
		second[c.kind] += c.bytes
	}
	total := fs.addUsageCalls[0]
	if second["edge"] != total.edge || second["relay"] != total.relayBytes {
		t.Fatalf("breakdown %v disagrees with the billed totals edge=%d relay=%d - the markers collided",
			second, total.edge, total.relayBytes)
	}
	if second["edge"] != 500 || second["relay"] != 2 {
		t.Errorf("second tick = %v, want edge 500 / relay 2", second)
	}
}

// A region that starts carrying traffic on a subject that was ALREADY being
// counted. Its marker is missing for the ordinary reason - the traffic is new -
// and it must be billed, not seeded away.
//
// This used to ask a per-SUBJECT question ("was this counted before regions?")
// once per region, so every region that appeared later lost its first delta and
// the breakdown fell permanently short of the total it breaks down.
func TestTrafficAggregator_ARegionAppearingLaterIsBilled(t *testing.T) {
	fs := &trafficFakeStore{owners: map[string]string{"srv-1": "tenant-a"}}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	mustHSet(t, rdb, "dylaris:traffic:edge:srv-1", map[string]interface{}{"rx:eu-central": 1000})
	agg.runOnce(context.Background())

	fs.addUsageCalls = nil
	fs.addRegionCalls = nil
	mustHSet(t, rdb, "dylaris:traffic:edge:srv-1", map[string]interface{}{"rx:ap-southeast": 500})
	agg.runOnce(context.Background())

	if len(fs.addUsageCalls) != 1 || fs.addUsageCalls[0].edge != 500 {
		t.Fatalf("total should have moved by 500: %+v", fs.addUsageCalls)
	}
	var regionSum int64
	for _, c := range fs.addRegionCalls {
		regionSum += c.bytes
		if c.region != "ap-southeast" {
			t.Errorf("unexpected region billed: %+v", c)
		}
	}
	if regionSum != fs.addUsageCalls[0].edge {
		t.Errorf("breakdown = %d but the billed total moved by %d", regionSum, fs.addUsageCalls[0].edge)
	}
}

// The seeding it replaces still has to happen exactly once, for a subject the
// aggregator was already counting before it knew about regions.
func TestTrafficAggregator_SeedsOnlyOnce(t *testing.T) {
	fs := &trafficFakeStore{owners: map[string]string{"srv-1": "tenant-a"}}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	mustHSet(t, rdb, "dylaris:traffic:edge:srv-1", map[string]interface{}{"rx:eu-central": 1000})
	mustSet(t, rdb, "dylaris:traffic:agg:seen:edge:srv-1", "950") // counted before regions existed

	agg.runOnce(context.Background())
	if len(fs.addRegionCalls) != 0 {
		t.Fatalf("the historical counter must be seeded, not billed: %+v", fs.addRegionCalls)
	}

	// From here on the subject is initialised, so a NEW region bills normally.
	fs.addUsageCalls = nil
	fs.addRegionCalls = nil
	mustHSet(t, rdb, "dylaris:traffic:edge:srv-1", map[string]interface{}{"rx:us-east": 300})
	agg.runOnce(context.Background())
	if len(fs.addRegionCalls) != 1 || fs.addRegionCalls[0].region != "us-east" || fs.addRegionCalls[0].bytes != 300 {
		t.Fatalf("a region appearing after seeding must be billed: %+v", fs.addRegionCalls)
	}
}

// Which product moved the bytes, read off the counter subject.
//
// Nothing on the wire carries it: Core writes an "owner:<id>" subject for a
// route-only address because such a tenant has no server of ours to key on, and
// a bare server UUID for a server on a machine they own. That difference is the
// whole signal, which is why it is derived here and no producer changed.
func TestTrafficProductComesFromTheSubject(t *testing.T) {
	cases := []struct{ subject, want string }{
		{"owner:11111111-1111-1111-1111-111111111111", store.TrafficProductRoute},
		{"3f2b9c7e-0000-4444-8888-aaaaaaaaaaaa", store.TrafficProductBYON},
		{"", store.TrafficProductUnknown},
	}
	for _, c := range cases {
		if got := trafficProductOf(c.subject); got != c.want {
			t.Errorf("trafficProductOf(%q) = %q, want %q", c.subject, got, c.want)
		}
	}
}

// The breakdown row has to carry the product, and the two products must not
// collapse into one another.
//
// They share a (region, kind) cell - both are player traffic at the same edge -
// so before the product joined the cell key the upsert folded whichever arrived
// second onto the first row. That is not a missing split but a WRONG one: one
// product's bytes reported as the other's.
func TestTheBreakdownSeparatesTheTwoProducts(t *testing.T) {
	const owner = "11111111-1111-1111-1111-111111111111"
	fs := &trafficFakeStore{
		owners: map[string]string{"srv-a": owner},
		routes: []store.CoreLinkRoute{{Domain: "mine.example", OwnerID: owner}},
	}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	mustHSet(t, rdb, "dylaris:traffic:edge:srv-a", map[string]interface{}{"rx:eu-central": "5000000000"})
	mustHSet(t, rdb, "dylaris:traffic:edge:owner:"+owner, map[string]interface{}{"rx:eu-central": "3000000000"})

	agg.runOnce(context.Background())

	got := map[string]int64{}
	for _, c := range fs.addRegionCalls {
		if c.region != "eu-central" || c.kind != store.TrafficKindEdge {
			t.Fatalf("unexpected cell %s/%s", c.region, c.kind)
		}
		got[c.product] += c.bytes
	}
	want := map[string]int64{store.TrafficProductBYON: 5000000000, store.TrafficProductRoute: 3000000000}
	for product, wantBytes := range want {
		if got[product] != wantBytes {
			t.Errorf("product %q got %d bytes, want %d (rows: %+v)", product, got[product], wantBytes, fs.addRegionCalls)
		}
	}
	// And the bill still sees the sum of both, unchanged by the split.
	if len(fs.addUsageCalls) != 1 || fs.addUsageCalls[0].edge != 8000000000 {
		t.Errorf("the billing total is %+v, want one call of 8000000000 edge bytes", fs.addUsageCalls)
	}
}

// Only a missing marker is zero. A marker that could not be read was taken as
// 0, which billed the counter's whole life into this month a second time.
func TestTrafficAggregator_UnreadableSeenIsNotZero(t *testing.T) {
	fs := &trafficFakeStore{
		owners:      map[string]string{"srv-uuid-1": "tenant-a"},
		backupBytes: map[string]int64{},
	}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	ctx := context.Background()

	mustHSet(t, rdb, "dylaris:traffic:edge:srv-uuid-1", map[string]interface{}{"rx": "900000"})
	// A read that fails: the marker is not a string.
	mustHSet(t, rdb, "dylaris:traffic:agg:seen:edge:srv-uuid-1", map[string]interface{}{"x": "1"})

	agg.runOnce(ctx)

	if len(fs.addUsageCalls) != 0 {
		t.Fatalf("billed %+v from a marker that could not be read", fs.addUsageCalls)
	}
}

// A marker that is not a count says nothing about what was billed. Skipping it
// every tick billed the subject never again; it starts over from the counter.
func TestTrafficAggregator_ACorruptSeenStartsOver(t *testing.T) {
	fs := &trafficFakeStore{
		owners:      map[string]string{"srv-uuid-1": "tenant-a"},
		backupBytes: map[string]int64{},
	}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	ctx := context.Background()

	mustHSet(t, rdb, "dylaris:traffic:edge:srv-uuid-1", map[string]interface{}{"rx": "900000"})
	mustSet(t, rdb, "dylaris:traffic:agg:seen:edge:srv-uuid-1", "not-a-number")
	agg.runOnce(ctx)
	if len(fs.addUsageCalls) != 0 {
		t.Fatalf("billed %+v from a corrupt marker", fs.addUsageCalls)
	}

	mustHSet(t, rdb, "dylaris:traffic:edge:srv-uuid-1", map[string]interface{}{"rx": "900100"})
	agg.runOnce(ctx)
	if len(fs.addUsageCalls) != 1 || fs.addUsageCalls[0].edge != 100 {
		t.Fatalf("addUsageCalls = %+v, want the 100 bytes since", fs.addUsageCalls)
	}
}
