package services

import (
	"bytes"
	"context"
	"dylaris-core/store"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type voiceFakeStore struct {
	store.Store
	rows     []store.VoicePort
	usage    []voiceUsageCall
	usageErr error
	current  map[int]*store.VoicePort
}

type voiceUsageCall struct {
	serverID int
	region   string
	rx, tx   int64
}

func (f *voiceFakeStore) AddVoiceUsage(serverID int, _ time.Time, region string, rx, tx int64) error {
	f.usage = append(f.usage, voiceUsageCall{serverID, region, rx, tx})
	return f.usageErr
}

func (f *voiceFakeStore) ListVoicePorts() ([]store.VoicePort, error) { return f.rows, nil }

// GetVoicePort answers from current when the server is in it (nil = deleted),
// else from rows: what the table holds by the time the reconcile re-reads.
func (f *voiceFakeStore) GetVoicePort(serverID int) (*store.VoicePort, error) {
	if c, ok := f.current[serverID]; ok {
		return c, nil
	}
	for _, r := range f.rows {
		if r.ServerID == serverID {
			return &r, nil
		}
	}
	return nil, nil
}

func newVoiceRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return mr, rdb
}

func testVoicePort() store.VoicePort {
	return store.VoicePort{
		ServerID: 7, ServerUUID: "11111111-2222-3333-4444-555555555555",
		Region: "eu", Port: 26000, Mod: "svc", InternalPort: VoiceInternalPort,
		UplinkKey: bytes.Repeat([]byte{0xab}, 32), Host: "a.eu.dylaris.com",
	}
}

func TestPublishVoiceWritesTheThreeKeys(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	p := testVoicePort()
	if err := PublishVoice(context.Background(), rdb, p); err != nil {
		t.Fatal(err)
	}
	hexKey := strings.Repeat("ab", 32)

	got, _ := mr.Get("voice:port:eu:26000")
	want := `{"v":1,"server_uuid":"11111111-2222-3333-4444-555555555555","key":"` + hexKey + `"}`
	if got != want {
		t.Fatalf("port key:\n got %s\nwant %s", got, want)
	}
	got, _ = mr.Get("dylaris:server:11111111-2222-3333-4444-555555555555:voice")
	want = `{"v":1,"region":"eu","port":26000,"internal_port":24454,"key":"` + hexKey + `","host":"a.eu.dylaris.com"}`
	if got != want {
		t.Fatalf("server key:\n got %s\nwant %s", got, want)
	}
	if ok, _ := mr.SIsMember("sys:index:voice:eu", "26000"); !ok {
		t.Fatal("port not in the region index")
	}
	for _, k := range []string{"voice:port:eu:26000", "dylaris:server:11111111-2222-3333-4444-555555555555:voice"} {
		if ttl := mr.TTL(k); ttl != 0 {
			t.Fatalf("%s has TTL %v, want none", k, ttl)
		}
	}
}

func TestReconcileRemovesOrphans(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	ctx := context.Background()
	p := testVoicePort()
	orphan := store.VoicePort{ServerUUID: "99999999-0000-0000-0000-000000000000", Region: "us", Port: 26005, UplinkKey: make([]byte, 32)}
	if err := PublishVoice(ctx, rdb, orphan); err != nil {
		t.Fatal(err)
	}
	// A row whose keys were lost must come back.
	svc := NewVoiceService(&voiceFakeStore{rows: []store.VoicePort{p}}, rdb)
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	for _, k := range []string{"voice:port:us:26005", "dylaris:server:99999999-0000-0000-0000-000000000000:voice"} {
		if mr.Exists(k) {
			t.Fatalf("orphan %s survived", k)
		}
	}
	if ok, _ := mr.SIsMember("sys:index:voice:us", "26005"); ok {
		t.Fatal("orphan index member survived")
	}
	if !mr.Exists("voice:port:eu:26000") || !mr.Exists("dylaris:server:"+p.ServerUUID+":voice") {
		t.Fatal("the row's keys were not rewritten")
	}
	if ok, _ := mr.SIsMember("sys:index:voice:eu", "26000"); !ok {
		t.Fatal("the row's index member was not rewritten")
	}
}

func TestReconcileIsANoOpForAFollower(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	ctx := context.Background()
	orphan := testVoicePort()
	if err := PublishVoice(ctx, rdb, orphan); err != nil {
		t.Fatal(err)
	}
	svc := NewVoiceService(&voiceFakeStore{}, rdb)
	svc.SetLeader(fakeLeader{leader: false})
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if !mr.Exists("voice:port:eu:26000") {
		t.Fatal("a follower swept keys")
	}
}

func TestVoiceRouteFor(t *testing.T) {
	const uuid = "11111111-2222-3333-4444-555555555555"
	eu := GatewayEdgeInfo{Status: "online", Region: "eu", Wildcard: "*.eu.dylaris.com"}
	us := GatewayEdgeInfo{Status: "online", Region: "us", Wildcard: "*.us.dylaris.com"}
	offlineUS := us
	offlineUS.Status = "offline"
	route := func(d string) GatewayRoute { return GatewayRoute{Domain: d, ServerUUID: uuid} }

	cases := []struct {
		name               string
		routes             []GatewayRoute
		edges              []GatewayEdgeInfo
		wantHost, wantRegn string
		wantOK             bool
	}{
		{"a domain one label under the wildcard matches", []GatewayRoute{route("a.eu.dylaris.com")}, []GatewayEdgeInfo{eu}, "a.eu.dylaris.com", "eu", true},
		{"a custom domain gives no match", []GatewayRoute{route("play.example.com")}, []GatewayEdgeInfo{eu}, "", "", false},
		{"two labels under the wildcard give no match", []GatewayRoute{route("a.b.eu.dylaris.com")}, []GatewayEdgeInfo{eu}, "", "", false},
		{"an offline edge is ignored", []GatewayRoute{route("a.us.dylaris.com")}, []GatewayEdgeInfo{offlineUS}, "", "", false},
		{"another server's route is ignored", []GatewayRoute{{Domain: "a.eu.dylaris.com", ServerUUID: "other"}}, []GatewayEdgeInfo{eu}, "", "", false},
		{"two routes give the lexically first", []GatewayRoute{route("z.us.dylaris.com"), route("b.eu.dylaris.com")}, []GatewayEdgeInfo{eu, us}, "b.eu.dylaris.com", "eu", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, region, ok := VoiceRouteFor(c.routes, c.edges, uuid)
			if host != c.wantHost || region != c.wantRegn || ok != c.wantOK {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", host, region, ok, c.wantHost, c.wantRegn, c.wantOK)
			}
		})
	}
}

func TestLiveVoiceRelaysSkipsExpiredRegistry(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	mr.SAdd("sys:voices", "voice-a", "voice-b")
	mr.Set("voice:registry:voice-a", `{"voice_id":"voice-a","region":"eu","ip":"1.2.3.4","wire_version":1}`)
	mr.SetTTL("voice:registry:voice-a", 10*time.Second)
	mr.Set("voice:registry:voice-b", `{"voice_id":"voice-b","region":"us","ip":"5.6.7.8","wire_version":1}`)
	mr.SetTTL("voice:registry:voice-b", 10*time.Second)
	mr.FastForward(5 * time.Second)
	mr.SetTTL("voice:registry:voice-a", 10*time.Second)
	mr.FastForward(6 * time.Second) // voice-b expired

	got, err := LiveVoiceRelays(context.Background(), rdb)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (VoiceRelay{ID: "voice-a", Region: "eu", IP: "1.2.3.4"}) {
		t.Fatalf("got %+v, want only voice-a", got)
	}
}

// The fake has no TenantServerOwners at all: a platform server, on a node no
// tenant owns, is metered all the same.
func TestMeterWritesPerServerUsageForPlatformServers(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	p := testVoicePort()
	mr.HSet("dylaris:traffic:edge:"+p.ServerUUID, "rx:eu", "1000", "vrx:eu", "200", "vtx:eu", "100")
	fs := &voiceFakeStore{rows: []store.VoicePort{p}}
	svc := NewVoiceService(fs, rdb)

	svc.Meter(context.Background())
	if len(fs.usage) != 1 || fs.usage[0] != (voiceUsageCall{7, "eu", 200, 100}) {
		t.Fatalf("usage = %+v, want server 7 eu rx 200 tx 100", fs.usage)
	}
	if got := mr.HGet(voiceSeenPrefix+p.ServerUUID, "vrx:eu"); got != "200" {
		t.Fatalf("seen vrx:eu = %q, want 200", got)
	}

	fs.usage = nil
	svc.Meter(context.Background())
	if len(fs.usage) != 0 {
		t.Fatalf("an unchanged counter wrote usage: %+v", fs.usage)
	}
}

func TestMeterLeavesSeenOnAFailedWrite(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	p := testVoicePort()
	mr.HSet("dylaris:traffic:edge:"+p.ServerUUID, "vrx:eu", "200", "vtx:eu", "100")
	fs := &voiceFakeStore{rows: []store.VoicePort{p}, usageErr: errors.New("db down")}

	NewVoiceService(fs, rdb).Meter(context.Background())
	if mr.Exists(voiceSeenPrefix + p.ServerUUID) {
		t.Fatal("seen advanced past a failed write")
	}
}

func TestMeterCounterResetCountsCurrent(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	p := testVoicePort()
	mr.HSet("dylaris:traffic:edge:"+p.ServerUUID, "vrx:eu", "30", "vtx:eu", "500")
	mr.HSet(voiceSeenPrefix+p.ServerUUID, "vrx:eu", "900", "vtx:eu", "400")
	fs := &voiceFakeStore{rows: []store.VoicePort{p}}

	NewVoiceService(fs, rdb).Meter(context.Background())
	if len(fs.usage) != 1 || fs.usage[0] != (voiceUsageCall{7, "eu", 30, 100}) {
		t.Fatalf("usage = %+v, want rx 30 (reset) tx 100", fs.usage)
	}
}

func TestMeterIsANoOpForAFollower(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	p := testVoicePort()
	mr.HSet("dylaris:traffic:edge:"+p.ServerUUID, "vrx:eu", "200")
	fs := &voiceFakeStore{rows: []store.VoicePort{p}}
	svc := NewVoiceService(fs, rdb)
	svc.SetLeader(fakeLeader{leader: false})

	svc.Meter(context.Background())
	if len(fs.usage) != 0 {
		t.Fatalf("a follower metered: %+v", fs.usage)
	}
}

// An unchanged tick still renews the seen hash: the counter outlives it, and an
// expired seen hash would re-add the lifetime voice bytes.
func TestMeterRenewsSeenOnAnUnchangedTick(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	p := testVoicePort()
	mr.HSet("dylaris:traffic:edge:"+p.ServerUUID, "vrx:eu", "200", "vtx:eu", "100")
	mr.HSet(voiceSeenPrefix+p.ServerUUID, "vrx:eu", "200", "vtx:eu", "100")
	mr.SetTTL(voiceSeenPrefix+p.ServerUUID, time.Hour)
	fs := &voiceFakeStore{rows: []store.VoicePort{p}}

	NewVoiceService(fs, rdb).Meter(context.Background())
	if len(fs.usage) != 0 {
		t.Fatalf("an unchanged counter wrote usage: %+v", fs.usage)
	}
	if ttl := mr.TTL(voiceSeenPrefix + p.ServerUUID); ttl != trafficSeenTTL {
		t.Fatalf("seen TTL = %v, want renewed to %v", ttl, trafficSeenTTL)
	}
}

// The rows are listed before the revocations, which can take a while; a row
// deleted or moved meanwhile must not come back from that stale list.
func TestReconcileRechecksTheRowBeforePublishing(t *testing.T) {
	p := testVoicePort()
	moved := p
	moved.Port = 26001
	cases := []struct {
		name    string
		current *store.VoicePort
		want    []string
	}{
		{"a deleted row is not republished", nil, nil},
		{"a moved row's old port is not republished", &moved, []string{ServerVoiceKey(p.ServerUUID), "voice:port:eu:26001"}},
		{"an unchanged row is republished", &p, []string{ServerVoiceKey(p.ServerUUID), "voice:port:eu:26000"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mr, rdb := newVoiceRedis(t)
			ctx := context.Background()
			if c.current != nil && c.current.Port != p.Port {
				// The move published its own keys.
				if err := PublishVoice(ctx, rdb, *c.current); err != nil {
					t.Fatal(err)
				}
			}
			fs := &voiceFakeStore{rows: []store.VoicePort{p}, current: map[int]*store.VoicePort{p.ServerID: c.current}}
			if err := NewVoiceService(fs, rdb).Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			got := mr.Keys() // sorted
			var plain []string
			for _, k := range got {
				if !strings.HasPrefix(k, voiceIndexPrefix) {
					plain = append(plain, k)
				}
			}
			if strings.Join(plain, ",") != strings.Join(c.want, ",") {
				t.Fatalf("keys %v, want %v", plain, c.want)
			}
			if c.current == nil || c.current.Port != p.Port {
				if ok, _ := mr.SIsMember("sys:index:voice:eu", "26000"); ok {
					t.Fatal("the stale port is back in the index")
				}
			}
		})
	}
}

// The reconcile re-reads the row before publishing; what it publishes must be
// that fresh row, not the list's copy. Same port, but a host changed through
// the API in between must reach the relay as the new host.
func TestReconcilePublishesTheReReadRow(t *testing.T) {
	p := testVoicePort()
	fresh := p
	fresh.Host = "new.eu.dylaris.com"
	mr, rdb := newVoiceRedis(t)
	fs := &voiceFakeStore{rows: []store.VoicePort{p}, current: map[int]*store.VoicePort{p.ServerID: &fresh}}
	if err := NewVoiceService(fs, rdb).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := mr.Get(ServerVoiceKey(p.ServerUUID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"host":"new.eu.dylaris.com"`) {
		t.Fatalf("published %s, want the re-read host", got)
	}
}
