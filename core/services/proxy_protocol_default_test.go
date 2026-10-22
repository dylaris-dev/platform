package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
	"dylaris-pkg/queue"
)

func eligibleDefaultInput() ProxyProtocolDefaultInput {
	return ProxyProtocolDefaultInput{
		NewServer:        true,
		RouteRequested:   true,
		InstallerType:    "paper",
		Version:          "1.21.4",
		RoutingMode:      "gateway",
		PlatformEnabled:  true,
		LinkClientAddr:   true,
		NodeWritesConfig: true,
	}
}

// Every condition is load-bearing on its own: each case breaks exactly one,
// so dropping any conjunct from the rule turns its case green.
func TestProxyProtocolDefaultOn_EveryConditionCounts(t *testing.T) {
	if !ProxyProtocolDefaultOn(eligibleDefaultInput()) {
		t.Fatal("a new, eligible Paper server must default to on")
	}
	cases := map[string]func(*ProxyProtocolDefaultInput){
		"existing server":          func(in *ProxyProtocolDefaultInput) { in.NewServer = false },
		"behind a platform proxy":  func(in *ProxyProtocolDefaultInput) { in.BehindProxy = true },
		"no route requested":       func(in *ProxyProtocolDefaultInput) { in.RouteRequested = false },
		"routing both":             func(in *ProxyProtocolDefaultInput) { in.RoutingMode = "both" },
		"routing ip_port":          func(in *ProxyProtocolDefaultInput) { in.RoutingMode = "ip_port" },
		"routing unset":            func(in *ProxyProtocolDefaultInput) { in.RoutingMode = "" },
		"platform setting off":     func(in *ProxyProtocolDefaultInput) { in.PlatformEnabled = false },
		"link without client-addr": func(in *ProxyProtocolDefaultInput) { in.LinkClientAddr = false },
		"node too old":             func(in *ProxyProtocolDefaultInput) { in.NodeWritesConfig = false },
		"fabric":                   func(in *ProxyProtocolDefaultInput) { in.InstallerType = "fabric" },
		"paper too old":            func(in *ProxyProtocolDefaultInput) { in.Version = "1.18.2" },
	}
	for name, breakIt := range cases {
		in := eligibleDefaultInput()
		breakIt(&in)
		if ProxyProtocolDefaultOn(in) {
			t.Errorf("%s: defaulted to on", name)
		}
	}
}

func TestProxyProtocolDefaultSoftware(t *testing.T) {
	cases := []struct {
		typ, version string
		want         bool
	}{
		{"paper", "1.19", true},
		{"paper", "1.19.4", true},
		{"paper", "1.21.11", true},
		{"paper", "26.1.2", true},
		{"paper", "1.18.2", false},
		{"paper", "1.8.8", false},
		{"paper", "", false},
		{"paper", "latest", false},
		{"velocity", "3.4.0-SNAPSHOT", true},
		{"waterfall", "1.21", true},
		{"waterfall", "1.13", true},
		{"waterfall", "1.12", false},
		{"bungeecord", "#2104", true},
		{"bungeecord", "1248", true},
		{"bungeecord", "#1247", false},
		{"bungeecord", "lastSuccessfulBuild", false},
		// Opt-in by owner decision: no writable setting, or not ours to write.
		{"vanilla", "1.21.4", false},
		{"fabric", "1.21.4", false},
		{"forge", "1.21.4", false},
		{"neoforge", "21.4.1", false},
		{"upload", "", false},
		{"upload-zip", "", false},
		{"modpack", "1.21.1", false},
		{"pack", "", false},
		{"technic", "", false},
		{"library", "", false},
		{"import", "", false},
		{"backup", "", false},
	}
	for _, c := range cases {
		if got := ProxyProtocolDefaultSoftware(c.typ, c.version); got != c.want {
			t.Errorf("%s %q = %v, want %v", c.typ, c.version, got, c.want)
		}
	}
}

type proxySwitchStore struct {
	store.Store
	servers map[string]*models.Server
	nodes   map[int]*models.Node
	err     error
	writes  []bool
}

func (f *proxySwitchStore) SetServerProxyProtocol(id int, enabled bool) error {
	if f.err != nil {
		return f.err
	}
	f.writes = append(f.writes, enabled)
	for _, s := range f.servers {
		if s.ID == id {
			s.ProxyProtocol = enabled
		}
	}
	return nil
}
func (f *proxySwitchStore) GetServerByUUID(uuid string) (*models.Server, error) {
	if s, ok := f.servers[uuid]; ok {
		c := *s
		return &c, nil
	}
	return nil, errors.New("not found")
}
func (f *proxySwitchStore) GetNodeByID(id int) (*models.Node, error) {
	if n, ok := f.nodes[id]; ok {
		return n, nil
	}
	return nil, errors.New("not found")
}

func TestApplyServerProxyProtocol_WritesAllThree(t *testing.T) {
	g, rdb, _ := newHubBridgeTestGateway(t)
	st := &proxySwitchStore{}
	ctx := context.Background()
	if err := ApplyServerProxyProtocol(ctx, st, rdb, g, 7, "srv-7", true); err != nil {
		t.Fatal(err)
	}
	if n, _ := rdb.Exists(ctx, ServerProxyProtocolKey("srv-7")).Result(); n != 1 {
		t.Fatal("node key not written")
	}
	if err := ApplyServerProxyProtocol(ctx, st, rdb, g, 7, "srv-7", false); err != nil {
		t.Fatal(err)
	}
	if n, _ := rdb.Exists(ctx, ServerProxyProtocolKey("srv-7")).Result(); n != 0 {
		t.Fatal("node key not removed")
	}
	msgs := readHubQueueMessages(t, rdb)
	if len(st.writes) != 2 || !st.writes[0] || st.writes[1] || len(msgs) != 2 ||
		msgs[0].Action != "set_server_proxy_protocol" || !msgs[0].ProxyProtocol || msgs[1].ProxyProtocol {
		t.Fatalf("store writes %v, hub %+v", st.writes, msgs)
	}
}

// The column is the truth: when it cannot be written, nothing else is.
func TestApplyServerProxyProtocol_StoreFailureStopsFirst(t *testing.T) {
	g, rdb, _ := newHubBridgeTestGateway(t)
	st := &proxySwitchStore{err: errors.New("db down")}
	err := ApplyServerProxyProtocol(context.Background(), st, rdb, g, 7, "srv-7", true)
	if !errors.Is(err, ErrProxyProtocolStore) {
		t.Fatalf("err = %v, want ErrProxyProtocolStore", err)
	}
	if n, _ := rdb.Exists(context.Background(), ServerProxyProtocolKey("srv-7")).Result(); n != 0 {
		t.Fatal("node key written after the store failed")
	}
	if len(readHubQueueMessages(t, rdb)) != 0 {
		t.Fatal("hub told after the store failed")
	}
}

func newProxyReportService(t *testing.T) (*SetupResultService, *proxySwitchStore) {
	g, rdb, _ := newHubBridgeTestGateway(t)
	st := &proxySwitchStore{
		servers: map[string]*models.Server{"srv-7": {ID: 7, UUID: "srv-7", NodeID: 3, ProxyProtocol: true}},
		nodes:   map[int]*models.Node{3: {ID: 3, Token: "node-3"}},
	}
	if err := rdb.Set(context.Background(), ServerProxyProtocolKey("srv-7"), "1", 0).Err(); err != nil {
		t.Fatal(err)
	}
	return NewSetupResultService(st, rdb, nil, g), st
}

func boolp(b bool) *bool { return &b }

// track records a request from the new-server default: the one kind a "no"
// switches off.
func track(t *testing.T, s *SetupResultService) {
	t.Helper()
	if err := TrackProxyProtocolInstall(context.Background(), s.redis, "srv-7", "main", true); err != nil {
		t.Fatal(err)
	}
}

// A "no" for a server that was ALREADY on - a reinstall that failed on a
// download error - leaves the switch on: its config was set up before, and
// switching off would refuse every player it still expects through the header.
// The sub-server is not marked either. Pub/Sub and the sweep alike.
func TestProxyProtocolNoOnAServerAlreadyOnKeepsTheSwitch(t *testing.T) {
	ctx := context.Background()
	for _, viaSweep := range []bool{false, true} {
		s, st := newProxyReportService(t)
		if err := TrackProxyProtocolInstall(ctx, s.redis, "srv-7", "lobby", false); err != nil {
			t.Fatal(err)
		}
		if viaSweep {
			s.redis.Set(ctx, ProxyProtocolResultKey("srv-7"), "0", time.Hour)
			s.sweepProxyProtocolResults(ctx, time.Now())
		} else {
			s.apply(queue.SetupResultsChannel("node-3"), setupReport{ServerUUID: "srv-7", SubServer: "lobby", ProxyProtocol: boolp(false)})
		}
		if len(st.writes) != 0 || !st.servers["srv-7"].ProxyProtocol {
			t.Fatalf("sweep=%v: switched a server that was already on: %v", viaSweep, st.writes)
		}
		if n, _ := s.redis.Exists(ctx, ServerProxyProtocolKey("srv-7")).Result(); n != 1 {
			t.Fatalf("sweep=%v: node key removed", viaSweep)
		}
		if ok, _ := ProxyProtocolSubConfigured(ctx, s.redis, "srv-7", "lobby"); ok {
			t.Fatalf("sweep=%v: an unconfirmed sub-server was marked", viaSweep)
		}
		if ok, _ := s.redis.HExists(ctx, proxyProtocolPendingKey, "srv-7").Result(); ok {
			t.Fatalf("sweep=%v: answered request still pending", viaSweep)
		}
	}
}

// A node that could not write the config gets its server switched back off,
// through the shared path; a yes records the sub-server and changes nothing.
func TestSetupResult_ProxyProtocolReport(t *testing.T) {
	s, st := newProxyReportService(t)
	ctx := context.Background()
	track(t, s)
	s.apply(queue.SetupResultsChannel("node-3"), setupReport{ServerUUID: "srv-7", SubServer: "main", ProxyProtocol: boolp(true)})
	if len(st.writes) != 0 || !st.servers["srv-7"].ProxyProtocol {
		t.Fatalf("a yes changed the switch: %v", st.writes)
	}
	if ok, _ := ProxyProtocolSubConfigured(ctx, s.redis, "srv-7", "main"); !ok {
		t.Fatal("a yes did not record the sub-server")
	}
	if ok, _ := s.redis.HExists(ctx, proxyProtocolPendingKey, "srv-7").Result(); ok {
		t.Fatal("answered request still pending")
	}

	track(t, s)
	s.apply(queue.SetupResultsChannel("node-3"), setupReport{ServerUUID: "srv-7", SubServer: "main", ProxyProtocol: boolp(false)})
	if len(st.writes) != 1 || st.writes[0] || st.servers["srv-7"].ProxyProtocol {
		t.Fatalf("a no did not switch it off: %v", st.writes)
	}
	if n, _ := s.redis.Exists(ctx, ServerProxyProtocolKey("srv-7"), ProxyProtocolSubsKey("srv-7")).Result(); n != 0 {
		t.Fatal("node key or configured sub-servers left behind")
	}
	if msgs := readHubQueueMessages(t, s.redis); len(msgs) != 1 || msgs[0].ProxyProtocol {
		t.Fatalf("hub not told: %+v", msgs)
	}
}

// An answer to nothing pending (late, stray, or superseded by the owner's own
// switch) changes nothing.
func TestSetupResult_ProxyProtocolAnswerWithoutRequestIsIgnored(t *testing.T) {
	s, st := newProxyReportService(t)
	s.apply(queue.SetupResultsChannel("node-3"), setupReport{ServerUUID: "srv-7", SubServer: "main", ProxyProtocol: boolp(false)})
	if len(st.writes) != 0 || !st.servers["srv-7"].ProxyProtocol {
		t.Fatalf("an unrequested no switched the server: %v", st.writes)
	}
}

// Only the node that hosts the server is heard.
func TestSetupResult_ProxyProtocolReportFromAnotherNodeIsDropped(t *testing.T) {
	s, st := newProxyReportService(t)
	track(t, s)
	s.apply(queue.SetupResultsChannel("node-9"), setupReport{ServerUUID: "srv-7", SubServer: "main", ProxyProtocol: boolp(false)})
	if len(st.writes) != 0 || !st.servers["srv-7"].ProxyProtocol {
		t.Fatalf("a foreign node switched the server: %v", st.writes)
	}
}

// An answer left from an EARLIER install is not this install's answer.
func TestTrackDropsAStaleAnswer(t *testing.T) {
	ctx := context.Background()
	s, st := newProxyReportService(t)
	s.redis.Set(ctx, ProxyProtocolResultKey("srv-7"), "0", time.Hour)
	track(t, s)
	s.sweepProxyProtocolResults(ctx, time.Now())
	if len(st.writes) != 0 {
		t.Fatalf("a stale answer switched the server: %v", st.writes)
	}
}

// The durable answer is acted on when the message was lost.
func TestProxyProtocolSweep(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	s, st := newProxyReportService(t)
	track(t, s)
	s.sweepProxyProtocolResults(ctx, now) // no answer yet: nothing
	if len(st.writes) != 0 {
		t.Fatalf("switched without an answer: %v", st.writes)
	}
	s.redis.Set(ctx, ProxyProtocolResultKey("srv-7"), "0", time.Hour)
	s.sweepProxyProtocolResults(ctx, now)
	if len(st.writes) != 1 || st.writes[0] {
		t.Fatalf("a durable no did not switch it off: %v", st.writes)
	}
	if ok, _ := s.redis.HExists(ctx, proxyProtocolPendingKey, "srv-7").Result(); ok {
		t.Fatal("still pending")
	}
	s.sweepProxyProtocolResults(ctx, now) // answered once only
	if len(st.writes) != 1 {
		t.Fatalf("acted twice: %v", st.writes)
	}

	s2, st2 := newProxyReportService(t)
	track(t, s2)
	s2.redis.Set(ctx, ProxyProtocolResultKey("srv-7"), "1", time.Hour)
	s2.sweepProxyProtocolResults(ctx, now)
	if len(st2.writes) != 0 {
		t.Fatalf("a durable yes switched: %v", st2.writes)
	}
	if ok, _ := ProxyProtocolSubConfigured(ctx, s2.redis, "srv-7", "main"); !ok {
		t.Fatal("a durable yes did not record the sub-server")
	}

	s3, st3 := newProxyReportService(t)
	track(t, s3)
	s3.sweepProxyProtocolResults(ctx, now.Add(proxyProtocolNoAnswer+time.Minute))
	if ok, _ := s3.redis.HExists(ctx, proxyProtocolPendingKey, "srv-7").Result(); ok || len(st3.writes) != 0 {
		t.Fatalf("an unanswered request: pending %v, writes %v", ok, st3.writes)
	}
}
