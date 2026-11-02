package services

import (
	"context"
	"dylaris-core/metrics"
	"dylaris-core/models"
	"dylaris-core/store"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// metricsFakeStore embeds store.Store (nil) so it satisfies the full interface
// while implementing only what the collector calls.
type metricsFakeStore struct {
	store.Store
	nodes         []models.Node
	servers       []models.Server
	serversByNode map[int]int
	users         int
}

func (f *metricsFakeStore) ListNodes() ([]models.Node, error) { return f.nodes, nil }

// The collector enriches the node rows before reading their live figures, so
// this is now one of the methods it calls. serversByNode is keyed by node ID.
func (f *metricsFakeStore) CountServersByNode(id int) (int, error)    { return f.serversByNode[id], nil }
func (f *metricsFakeStore) CountServersPerNode() (map[int]int, error) { return f.serversByNode, nil }
func (f *metricsFakeStore) CountUsers() (int, error)                  { return f.users, nil }
func (f *metricsFakeStore) ListServers(string) ([]models.Server, error) {
	return f.servers, nil
}

type fakeLeader struct{ leader bool }

func (f fakeLeader) IsLeader() bool { return f.leader }

// captureStore records what the recorder flushed.
type captureStore struct{ rows []metrics.Row }

func (c *captureStore) Upsert(_ context.Context, rows []metrics.Row) error {
	c.rows = append(c.rows, rows...)
	return nil
}

func (c *captureStore) byMetric(metric string) []metrics.Row {
	var out []metrics.Row
	for _, r := range c.rows {
		if r.Key.Metric == metric {
			out = append(out, r)
		}
	}
	return out
}

func (c *captureStore) one(t *testing.T, metric string) metrics.Row {
	t.Helper()
	got := c.byMetric(metric)
	if len(got) != 1 {
		t.Fatalf("expected exactly one row for %q, got %d", metric, len(got))
	}
	return got[0]
}

// settingsMap is the smallest settingsReader FeatureFlags needs.
type settingsMap map[string]string

func (m settingsMap) GetSetting(k string) (string, error) { return m[k], nil }

func owner(s string) *string { return &s }

func newCollectorForTest(t *testing.T, st store.Store, on bool, isLeader bool) (*MetricsCollector, *captureStore) {
	t.Helper()
	capt := &captureStore{}
	rec := metrics.NewRecorder(capt, time.Hour)
	set := settingsMap{}
	if on {
		set[MetricsEnabledSetting] = "true"
	}
	c := NewMetricsCollector(st, nil, nil, fixedRecorder{rec}, NewFeatureFlags(set))
	c.SetLeader(fakeLeader{leader: isLeader})
	return c, capt
}

func TestCollectorRecordsNothingWhenNotLeader(t *testing.T) {
	// Every replica sees the same fleet. Without the gate a two-replica install
	// folds the same players into the same bucket twice and reports double -
	// which is exactly how the overage sweep once billed every customer per
	// replica.
	st := &metricsFakeStore{nodes: []models.Node{{Token: "n1", Status: "online"}}}
	c, capt := newCollectorForTest(t, st, true, false)

	c.sampleOnce(context.Background())
	if err := c.recorders.Recorder().Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(capt.rows) != 0 {
		t.Fatalf("a follower recorded %d rows", len(capt.rows))
	}
}

func TestCollectorRecordsNothingWhenTheFeatureIsOff(t *testing.T) {
	// Default off. Recording a year of history is a decision with a date on it,
	// not something that starts because the software supports it.
	st := &metricsFakeStore{nodes: []models.Node{{Token: "n1", Status: "online"}}}
	c, capt := newCollectorForTest(t, st, false, true)

	c.sampleOnce(context.Background())
	if err := c.recorders.Recorder().Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(capt.rows) != 0 {
		t.Fatalf("recorded %d rows with the feature off", len(capt.rows))
	}
}

func TestCollectorCountsTheThreeNodeKindsApart(t *testing.T) {
	st := &metricsFakeStore{
		users: 7,
		nodes: []models.Node{
			{Token: "p1", Status: "online"},
			{Token: "p2", Status: "offline"},
			{Token: "e1", Status: "online", Tags: "eu,external"},
			{Token: "b1", Status: "online", OwnerID: owner("u-1")},
			// Owned AND tagged external: the customer's, not the operator's.
			// Counting this as external would file a machine the operator does
			// not control under their own hardware.
			{Token: "b2", Status: "online", OwnerID: owner("u-2"), Tags: "external"},
		},
	}
	c, capt := newCollectorForTest(t, st, true, true)

	c.sampleOnce(context.Background())
	if err := c.recorders.Recorder().Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	// platform.nodes is OUR fleet - cluster plus external - and not every
	// machine on the books. It used to be all five, until it became the total
	// that platform.nodes_online is a fraction of: a customer's box being off
	// would otherwise show as this platform running at 4/5. The two customer
	// machines are counted in their own pair below.
	want := map[string]float64{
		"platform.nodes":             3,
		"platform.nodes_online":      2,
		"platform.nodes_platform":    2,
		"platform.nodes_external":    1,
		"platform.nodes_byon":        2,
		"platform.nodes_byon_online": 2,
		"platform.users":             7,
	}
	for metric, v := range want {
		if got := capt.one(t, metric).Bucket; got.Sum != v || got.Count != 1 {
			t.Errorf("%s = sum %v count %d, want sum %v count 1", metric, got.Sum, got.Count, v)
		}
	}
}

func TestAnOfflineNodeRecordsDownAndNotItsStaleReadings(t *testing.T) {
	// CPU and RAM on an offline node are the LAST values seen, not current
	// ones. Recording them puts a flatline into the average that reads like a
	// healthy idle machine - the graph would show the fleet coping while half
	// of it was unreachable.
	//
	// The load values arrive through the HEARTBEAT, so the test seeds one
	// rather than setting the struct fields. It used to set them by hand, which
	// is why it stayed green while production recorded a flat zero for every
	// node: those fields are not persisted, the collector read them off an
	// unenriched row, and no test ever went through the path that fills them.
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	// Both nodes report; only the offline one's reading is stale.
	mr.Set("dylaris:discovery:down", `{"cpuUsage":42,"ramTotal":100,"ramFree":40}`)
	mr.Set("dylaris:discovery:up", `{"cpuUsage":10,"ramTotal":100,"ramFree":40}`)

	st := &metricsFakeStore{nodes: []models.Node{
		{Token: "down", Status: "offline"},
		{Token: "up", Status: "online", Region: "eu"},
	}}
	capt := &captureStore{}
	c := NewMetricsCollector(st, rdb, nil,
		fixedRecorder{metrics.NewRecorder(capt, time.Hour)},
		NewFeatureFlags(settingsMap{MetricsEnabledSetting: "true"}))
	c.SetLeader(fakeLeader{leader: true})

	c.sampleOnce(context.Background())
	if err := c.recorders.Recorder().Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	ups := map[string]float64{}
	for _, r := range capt.byMetric("node.up") {
		ups[r.Key.Subject] = r.Bucket.Sum
	}
	if ups["down"] != 0 || ups["up"] != 1 {
		t.Fatalf("node.up = %v, want down:0 up:1", ups)
	}

	for _, m := range []string{"node.cpu_pct", "node.ram_pct", "node.ram_used_bytes"} {
		for _, r := range capt.byMetric(m) {
			if r.Key.Subject == "down" {
				t.Errorf("%s was recorded for an offline node", m)
			}
		}
	}
	// The live one keeps its region, so a per-region reading stays per-region -
	// and it carries the heartbeat's value, not the row's.
	got := capt.byMetric("node.cpu_pct")
	if len(got) != 1 || got[0].Key.Region != "eu" {
		t.Fatalf("live node cpu row wrong: %+v", got)
	}
	if got[0].Bucket.Sum != 10 {
		t.Errorf("node.cpu_pct = %v, want 10 from the heartbeat", got[0].Bucket.Sum)
	}
}

func TestUpIsRecordedAsANumberSoUptimeIsAnAverage(t *testing.T) {
	// Availability has to be a series rather than inferred from missing rows: a
	// gap is ambiguous between "the node was down" and "nothing was sampling",
	// and those are opposite claims to make to a buyer.
	st := &metricsFakeStore{nodes: []models.Node{{Token: "n", Status: "online"}}}
	c, capt := newCollectorForTest(t, st, true, true)

	c.sampleOnce(context.Background())
	st.nodes[0].Status = "offline"
	c.sampleOnce(context.Background())
	c.sampleOnce(context.Background())
	if err := c.recorders.Recorder().Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	row := capt.one(t, "node.up")
	if row.Bucket.Count != 3 || row.Bucket.Sum != 1 {
		t.Fatalf("node.up bucket = %+v, want 3 samples summing to 1", row.Bucket)
	}
	if got := row.Bucket.Avg(); got != 1.0/3.0 {
		t.Fatalf("uptime fraction = %v, want 1/3", got)
	}
	if row.Bucket.Min != 0 || row.Bucket.Max != 1 {
		t.Fatalf("min/max lost the range: %+v", row.Bucket)
	}
}

// The record is what somebody is eventually shown, and availability is the
// number they read hardest. A BYON machine is a customer's box; switching it
// off at night is not downtime of this platform, and averaging it into node.up
// would understate the uptime this fleet actually delivered - with somebody
// else's decisions.
func TestCustomerMachinesStayOutOfTheAvailabilitySeries(t *testing.T) {
	st := &metricsFakeStore{nodes: []models.Node{
		{Token: "p1", Status: "online"},
		{Token: "e1", Status: "online", Tags: "external"},
		// Two customer machines, both down. Neither may touch node.up.
		{Token: "b1", Status: "offline", OwnerID: owner("u-1")},
		{Token: "b2", Status: "offline", OwnerID: owner("u-2")},
	}}
	c, capt := newCollectorForTest(t, st, true, true)

	c.sampleOnce(context.Background())
	if err := c.recorders.Recorder().Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, r := range capt.byMetric("node.up") {
		if r.Key.Subject == "b1" || r.Key.Subject == "b2" {
			t.Errorf("customer node %q was recorded in node.up", r.Key.Subject)
		}
		if r.Bucket.Sum != 1 {
			t.Errorf("%s recorded as down; only our nodes belong here", r.Key.Subject)
		}
	}
	if got := len(capt.byMetric("node.up")); got != 2 {
		t.Fatalf("node.up covers %d machines, want 2 (cluster + external)", got)
	}
}

// They are counted, though. How much hardware customers brought is a real
// number about the business - it just cannot be allowed to contaminate an
// availability figure, which is why it is a gauge of its own rather than a
// per-machine `up` series the summary averages.
func TestCustomerMachinesAreStillCounted(t *testing.T) {
	st := &metricsFakeStore{nodes: []models.Node{
		{Token: "p1", Status: "online"},
		{Token: "b1", Status: "online", OwnerID: owner("u-1")},
		{Token: "b2", Status: "offline", OwnerID: owner("u-2")},
	}}
	c, capt := newCollectorForTest(t, st, true, true)

	c.sampleOnce(context.Background())
	if err := c.recorders.Recorder().Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	for metric, want := range map[string]float64{
		"platform.nodes":             1, // ours only: one cluster node
		"platform.nodes_online":      1,
		"platform.nodes_byon":        2,
		"platform.nodes_byon_online": 1,
	} {
		if got := capt.one(t, metric).Bucket.Sum; got != want {
			t.Errorf("%s = %v, want %v", metric, got, want)
		}
	}
}

// platform.nodes is what platform.nodes_online is a fraction OF, so the two
// have to describe the same set. Counting every machine in one and only ours in
// the other would make the ratio quietly wrong rather than obviously broken.
func TestTheFleetTotalAndItsOnlineCountDescribeTheSameSet(t *testing.T) {
	st := &metricsFakeStore{nodes: []models.Node{
		{Token: "p1", Status: "online"},
		{Token: "p2", Status: "online"},
		{Token: "e1", Status: "online", Tags: "external"},
		{Token: "b1", Status: "online", OwnerID: owner("u-1")},
	}}
	c, capt := newCollectorForTest(t, st, true, true)

	c.sampleOnce(context.Background())
	if err := c.recorders.Recorder().Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	total := capt.one(t, "platform.nodes").Bucket.Sum
	online := capt.one(t, "platform.nodes_online").Bucket.Sum
	platform := capt.one(t, "platform.nodes_platform").Bucket.Sum
	external := capt.one(t, "platform.nodes_external").Bucket.Sum

	if total != platform+external {
		t.Errorf("nodes (%v) != platform (%v) + external (%v)", total, platform, external)
	}
	if online > total {
		t.Errorf("online (%v) exceeds the total (%v); the two count different sets", online, total)
	}
	if total != 3 {
		t.Errorf("total = %v, want 3 - the customer node must not be in it", total)
	}
}
