package services

import (
	"context"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

func TestJudgeDataTraffic(t *testing.T) {
	gb := int64(1_000_000_000)
	inc := func(n int64) *int64 { return &n }
	cases := []struct {
		name      string
		used      int64
		included  *int64
		units     *int64
		consent   bool
		warn, thr bool
	}{
		{"no allowance", 500 * gb, nil, inc(1), false, false, false},
		{"below 90 %", 89 * gb, inc(100), inc(1), false, false, false},
		{"at 90 %", 90 * gb, inc(100), inc(1), false, true, false},
		{"at 100 %", 100 * gb, inc(100), inc(1), false, true, true},
		{"units multiply", 150 * gb, inc(100), inc(2), false, false, false},
		{"consent bills instead", 500 * gb, inc(100), inc(1), true, false, false},
		{"zero allowance", 1, inc(0), inc(1), false, true, true},
		{"zero units", 1, inc(100), inc(0), false, true, true},
		{"unmetered account", 500 * gb, inc(100), nil, false, false, false},
	}
	for _, c := range cases {
		v := JudgeDataTraffic(c.used, c.included, c.units, c.consent)
		if v.Warn != c.warn || v.Throttled != c.thr {
			t.Errorf("%s: warn %v throttled %v, want %v %v", c.name, v.Warn, v.Throttled, c.warn, c.thr)
		}
	}
}

// An account that never bought or was granted anything is not metered; one
// cap of either kind makes it a customer, and a live grant counts as one unit.
func TestTrafficUnits(t *testing.T) {
	now := time.Now()
	n := func(v int64) *int64 { return &v }
	later := now.Add(time.Hour)
	earlier := now.Add(-time.Hour)
	cases := []struct {
		name string
		b    *store.UserBilling
		want *int64
	}{
		{"no billing row", nil, nil},
		{"nothing bought (admin, hand-made account)", &store.UserBilling{}, nil},
		{"route-only customer", &store.UserBilling{MaxLinks: n(1)}, n(1)},
		{"both products", &store.UserBilling{MaxNodes: n(2), MaxLinks: n(1)}, n(3)},
		{"live BYON grant", &store.UserBilling{ManualByonExpiresAt: &later}, n(1)},
		{"expired grant", &store.UserBilling{ManualByonExpiresAt: &earlier}, nil},
	}
	for _, c := range cases {
		got := TrafficUnits(c.b, now)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s: units %v, want %v", c.name, deref(got), deref(c.want))
		}
	}
}

// Over the data allowance without consent: every server of the tenant gets
// the throttle key and the tenant one notification; consent releases it.
func TestDataThrottleEvaluator(t *testing.T) {
	hundred := int64(100)
	fs := &trafficFakeStore{
		owners:      map[string]string{"srv-1": "tenant-a", "srv-2": "tenant-a", "srv-3": "tenant-b"},
		dataLimit:   &models.TrafficLimit{IncludedGB: &hundred},
		dataUsage:   map[string][]store.RegionUsage{"tenant-a": {{Region: "eu-central", Kind: store.TrafficKindRelay, Bytes: 120_000_000_000}}},
		dataConsent: map[string]bool{},
	}
	agg, rdb := newTrafficAggregatorTest(t, fs, true)
	ctx := context.Background()

	agg.evaluateDataThrottle(ctx, fs.owners)
	for _, uuid := range []string{"srv-1", "srv-2"} {
		if v, _ := rdb.Get(ctx, DataBandwidthKey(uuid)).Int64(); v != DataThrottleDefaultMbit*125_000 {
			t.Fatalf("%s throttle = %d, want %d bytes/s", uuid, v, DataThrottleDefaultMbit*125_000)
		}
	}
	if n, _ := rdb.Exists(ctx, DataBandwidthKey("srv-3")).Result(); n != 0 {
		t.Fatal("a tenant under its allowance was throttled")
	}
	if !IsDataThrottled(ctx, rdb, "tenant-a") {
		t.Fatal("throttled tenant not listed")
	}
	agg.evaluateDataThrottle(ctx, fs.owners)
	// Straight past 75 and 90 to 120 %: only the level reached is announced,
	// once, in the panel and by mail.
	if len(fs.notifications) != 1 || len(fs.mails) != 1 || fs.mails[0].userID != "tenant-a" || fs.mails[0].subject != "Data traffic limit reached" {
		t.Fatalf("notifications %v mails %+v, want the 100 %% one once each, to tenant-a", fs.notifications, fs.mails)
	}

	fs.dataConsent["tenant-a"] = true
	agg.evaluateDataThrottle(ctx, fs.owners)
	if n, _ := rdb.Exists(ctx, DataBandwidthKey("srv-1"), DataBandwidthKey("srv-2")).Result(); n != 0 || IsDataThrottled(ctx, rdb, "tenant-a") {
		t.Fatal("consent did not lift the throttle")
	}
}

// The configured speed is honoured but never below the minimum.
func TestDataThrottleMbit(t *testing.T) {
	fs := &trafficFakeStore{settings: map[string]string{}}
	if got := DataThrottleMbit(fs); got != DataThrottleDefaultMbit {
		t.Fatalf("default %d", got)
	}
	fs.settings[DataThrottleSettingKey] = "5"
	if got := DataThrottleMbit(fs); got != DataThrottleMinMbit {
		t.Fatalf("below the minimum gave %d", got)
	}
	fs.settings[DataThrottleSettingKey] = "200"
	if got := DataThrottleMbit(fs); got != 200 {
		t.Fatalf("got %d", got)
	}
}

// The limiter paces a server's bytes at its published rate, across calls.
func TestDataLimiterPaces(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx := context.Background()
	rdb.Set(ctx, DataBandwidthKey("srv"), 100_000, 0) // 100 KB/s
	d := NewDataLimiter(rdb)
	start := time.Now()
	d.Wait(ctx, "srv", 50_000)
	d.Wait(ctx, "srv", 50_000) // waits for the first 0.5 s
	if el := time.Since(start); el < 400*time.Millisecond {
		t.Fatalf("100 KB at 100 KB/s took %v, want about 0.5 s before the second chunk", el)
	}
	start = time.Now()
	d.Wait(ctx, "other", 10_000_000)
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("an unthrottled server waited")
	}
}

// Tab bytes land in the server's relay counter as data traffic.
func TestTabTrafficMeterFlushes(t *testing.T) {
	rdb := newQueueTestRedis(t)
	m := NewTabTrafficMeter(rdb, func(int) string { return "eu-central" })
	m.Add("srv", 7, 100, 900)
	m.Add("srv", 7, 0, 100)
	m.flush(context.Background())
	got, _ := rdb.HGetAll(context.Background(), "dylaris:traffic:relay:srv").Result()
	if got["tab_up:eu-central"] != "100" || got["tab_down:eu-central"] != "1000" {
		t.Fatalf("relay counter %v", got)
	}
}

// 75, 90 and 100 % each announce once as the month goes on, the mail carries
// the notice's text, and a level that was jumped over never arrives late.
func TestTrafficNoticesStepThroughTheLevels(t *testing.T) {
	hundred := int64(100)
	usage := func(gb int64) map[string][]store.RegionUsage {
		return map[string][]store.RegionUsage{"tenant-a": {{Region: "eu-central", Kind: store.TrafficKindRelay, Bytes: gb * 1_000_000_000}}}
	}
	fs := &trafficFakeStore{
		owners:      map[string]string{"srv-1": "tenant-a"},
		dataLimit:   &models.TrafficLimit{IncludedGB: &hundred},
		dataUsage:   usage(74),
		dataConsent: map[string]bool{},
	}
	agg, _ := newTrafficAggregatorTest(t, fs, true)
	ctx := context.Background()

	steps := []struct {
		gb      int64
		subject string // "" = nothing new
	}{
		{74, ""},
		{75, "Data traffic at 75 %"},
		{80, ""},
		{90, "Data traffic at 90 %"},
		{95, ""},
		{100, "Data traffic limit reached"},
		{130, ""},
	}
	sent := 0
	for _, st := range steps {
		fs.dataUsage = usage(st.gb)
		agg.evaluateDataThrottle(ctx, fs.owners)
		agg.evaluateDataThrottle(ctx, fs.owners) // a second tick at the same level says nothing
		if st.subject != "" {
			sent++
		}
		if len(fs.mails) != sent || len(fs.notifications) != sent {
			t.Fatalf("at %d GB: %d mails and %d notices, want %d of each (%+v)", st.gb, len(fs.mails), len(fs.notifications), sent, fs.mails)
		}
		if st.subject != "" {
			m := fs.mails[sent-1]
			if m.userID != "tenant-a" || m.subject != st.subject || m.body == "" {
				t.Fatalf("at %d GB: mail %+v, want %q to tenant-a with the notice text", st.gb, m, st.subject)
			}
		}
	}
}

// Jumping from under 75 % to 92 % announces 90 only; falling back under 90
// later in the tick sequence must not bring the 75 % message out after it.
func TestTrafficNoticeSkippedLevelNeverArrivesLate(t *testing.T) {
	hundred := int64(100)
	usage := func(gb int64) map[string][]store.RegionUsage {
		return map[string][]store.RegionUsage{"tenant-a": {{Region: "eu-central", Kind: store.TrafficKindRelay, Bytes: gb * 1_000_000_000}}}
	}
	fs := &trafficFakeStore{
		owners:      map[string]string{"srv-1": "tenant-a"},
		dataLimit:   &models.TrafficLimit{IncludedGB: &hundred},
		dataUsage:   usage(92),
		dataConsent: map[string]bool{},
	}
	agg, _ := newTrafficAggregatorTest(t, fs, true)
	ctx := context.Background()

	agg.evaluateDataThrottle(ctx, fs.owners)
	// The allowance was raised: the same bytes are now 76 %.
	bigger := int64(121)
	fs.dataLimit = &models.TrafficLimit{IncludedGB: &bigger}
	agg.evaluateDataThrottle(ctx, fs.owners)

	if len(fs.mails) != 1 || fs.mails[0].subject != "Data traffic at 90 %" {
		t.Fatalf("mails %+v, want only the 90 %% one", fs.mails)
	}
}

func TestJudgeNoticeAt75Percent(t *testing.T) {
	hundred, one := int64(100), int64(1)
	for _, c := range []struct {
		gb                     int64
		notice, warn, throttle bool
	}{{74, false, false, false}, {75, true, false, false}, {90, true, true, false}, {100, true, true, true}} {
		v := JudgeDataTraffic(c.gb*1_000_000_000, &hundred, &one, false)
		if v.Notice != c.notice || v.Warn != c.warn || v.Throttled != c.throttle {
			t.Errorf("data %d GB: %+v", c.gb, v)
		}
		m := JudgeManagedPool(c.gb*1_000_000_000, &hundred, nil, false)
		if m.Notice != c.notice || m.Warn != c.warn || m.Throttled != c.throttle {
			t.Errorf("managed %d GB: %+v", c.gb, m)
		}
	}
}
