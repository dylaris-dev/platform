package services

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dylaris-core/config"
	"dylaris-core/store"
)

type settingsOnlyStore struct {
	store.Store
	m   map[string]string
	err error
}

func (s settingsOnlyStore) GetSetting(key string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	v, ok := s.m[key]
	if !ok {
		return "", sql.ErrNoRows
	}
	return v, nil
}

func TestProviderConfigEnvWins(t *testing.T) {
	tests := []struct {
		name     string
		cfg      config.Config
		settings map[string]string
		want     ProviderConfig
	}{
		{"nothing set", config.Config{}, nil,
			ProviderConfig{URL: DefaultGatewayProviderURL}},
		{"env only", config.Config{GatewayProviderURL: "https://env.example", GatewayProviderKey: "envkey"},
			map[string]string{"gateway.provider": "remote"},
			ProviderConfig{Remote: true, URL: "https://env.example", Key: "envkey", KeyFromEnv: true, URLFromEnv: true}},
		{"setting only", config.Config{},
			map[string]string{"gateway.provider": "remote", "gateway.provider_url": "https://set.example", "gateway.provider_key": "setkey"},
			ProviderConfig{Remote: true, URL: "https://set.example", Key: "setkey"}},
		{"both: env wins per field", config.Config{GatewayProviderKey: "envkey"},
			map[string]string{"gateway.provider": "remote", "gateway.provider_url": "https://set.example", "gateway.provider_key": "setkey"},
			ProviderConfig{Remote: true, URL: "https://set.example", Key: "envkey", KeyFromEnv: true}},
		{"set-but-empty env counts as unset", config.Config{GatewayProviderURL: "  ", GatewayProviderKey: ""},
			map[string]string{"gateway.provider": "remote", "gateway.provider_url": "https://set.example", "gateway.provider_key": "setkey"},
			ProviderConfig{Remote: true, URL: "https://set.example", Key: "setkey"}},
		{"mode remote without key", config.Config{},
			map[string]string{"gateway.provider": "remote"},
			ProviderConfig{URL: DefaultGatewayProviderURL}},
		{"key without mode stays local", config.Config{GatewayProviderKey: "envkey"}, nil,
			ProviderConfig{URL: DefaultGatewayProviderURL, Key: "envkey", KeyFromEnv: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := LoadProviderConfig(settingsOnlyStore{m: tt.settings}, &tt.cfg)
			if err != nil || got != tt.want {
				t.Errorf("LoadProviderConfig = %+v, %v, want %+v", got, err, tt.want)
			}
		})
	}
}

func TestLoadProviderConfigReadError(t *testing.T) {
	boom := errors.New("db down")
	if _, err := LoadProviderConfig(settingsOnlyStore{err: boom}, &config.Config{}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the read error", err)
	}
	// Env pins URL and key, but the mode still comes from the setting.
	cfg := &config.Config{GatewayProviderURL: "u", GatewayProviderKey: "k"}
	if _, err := LoadProviderConfig(settingsOnlyStore{err: boom}, cfg); !errors.Is(err, boom) {
		t.Errorf("mode read error not reported: %v", err)
	}
}

// recordingProvider captures the arguments of every call as one string.
type recordingProvider struct {
	GatewayProvider
	calls []string
}

func (r *recordingProvider) rec(s string) { r.calls = append(r.calls, s) }

func (r *recordingProvider) CreateServerRoute(id uint, owner, domain string, port int) error {
	r.rec(fmt.Sprint("CreateServerRoute|", id, "|", owner, "|", domain, "|", port))
	return nil
}
func (r *recordingProvider) SetServerProxyProtocol(uuid string, on bool) error {
	r.rec(fmt.Sprint("SetServerProxyProtocol|", uuid, "|", on))
	return nil
}
func (r *recordingProvider) SetCoreOwnedRouteProxyProtocol(domain string, on bool) error {
	r.rec(fmt.Sprint("SetCoreOwnedRouteProxyProtocol|", domain, "|", on))
	return nil
}
func (r *recordingProvider) CreateRouteViaLink(owner, domain, token, host string, port int) error {
	r.rec(fmt.Sprint("CreateRouteViaLink|", owner, "|", domain, "|", token, "|", host, "|", port))
	return nil
}
func (r *recordingProvider) DeleteCoreOwnedRoute(domain string) error {
	r.rec("DeleteCoreOwnedRoute|" + domain)
	return nil
}
func (r *recordingProvider) SuspendCoreOwnedRoute(domain, reason string) (string, error) {
	r.rec("SuspendCoreOwnedRoute|" + domain + "|" + reason)
	return "owner-s", nil
}
func (r *recordingProvider) UnsuspendCoreOwnedRoute(domain string) (string, error) {
	r.rec("UnsuspendCoreOwnedRoute|" + domain)
	return "owner-u", nil
}
func (r *recordingProvider) DeleteRoute(domain string) error {
	r.rec("DeleteRoute|" + domain)
	return nil
}
func (r *recordingProvider) DeleteServerRoutes(uuid string) error {
	r.rec("DeleteServerRoutes|" + uuid)
	return nil
}
func (r *recordingProvider) MigrateServerRoutes(id, node uint) error {
	r.rec(fmt.Sprint("MigrateServerRoutes|", id, "|", node))
	return nil
}
func (r *recordingProvider) LinkToken(n string) string { r.rec("LinkToken|" + n); return "lt" }
func (r *recordingProvider) DiscoveryProof(n string) string {
	r.rec("DiscoveryProof|" + n)
	return "dp"
}
func (r *recordingProvider) SetNodeLinkEnabled(token string, on bool) error {
	r.rec(fmt.Sprint("SetNodeLinkEnabled|", token, "|", on))
	return nil
}

// allCalls drives all 13 GatewayProvider methods with distinct argument values,
// so two swapped adjacent parameters change the recorded string.
var allCalls = []struct {
	want string
	call func(g GatewayProvider) error
}{
	{"CreateServerRoute|7|own|a.test|25565", func(g GatewayProvider) error { return g.CreateServerRoute(7, "own", "a.test", 25565) }},
	{"SetServerProxyProtocol|uuid-1|true", func(g GatewayProvider) error { return g.SetServerProxyProtocol("uuid-1", true) }},
	{"SetCoreOwnedRouteProxyProtocol|b.test|true", func(g GatewayProvider) error { return g.SetCoreOwnedRouteProxyProtocol("b.test", true) }},
	{"CreateRouteViaLink|own|c.test|tok|10.0.0.5|25566", func(g GatewayProvider) error {
		return g.CreateRouteViaLink("own", "c.test", "tok", "10.0.0.5", 25566)
	}},
	{"DeleteCoreOwnedRoute|d.test", func(g GatewayProvider) error { return g.DeleteCoreOwnedRoute("d.test") }},
	{"SuspendCoreOwnedRoute|e.test|why", func(g GatewayProvider) error {
		o, err := g.SuspendCoreOwnedRoute("e.test", "why")
		if o != "owner-s" {
			return errors.New("owner not passed through")
		}
		return err
	}},
	{"UnsuspendCoreOwnedRoute|f.test", func(g GatewayProvider) error {
		o, err := g.UnsuspendCoreOwnedRoute("f.test")
		if o != "owner-u" {
			return errors.New("owner not passed through")
		}
		return err
	}},
	{"DeleteRoute|g.test", func(g GatewayProvider) error { return g.DeleteRoute("g.test") }},
	{"DeleteServerRoutes|uuid-2", func(g GatewayProvider) error { return g.DeleteServerRoutes("uuid-2") }},
	{"MigrateServerRoutes|8|9", func(g GatewayProvider) error { return g.MigrateServerRoutes(8, 9) }},
	{"LinkToken|n1", func(g GatewayProvider) error {
		if g.LinkToken("n1") != "lt" {
			return errors.New("LinkToken result lost")
		}
		return nil
	}},
	{"DiscoveryProof|n2", func(g GatewayProvider) error {
		if g.DiscoveryProof("n2") != "dp" {
			return errors.New("DiscoveryProof result lost")
		}
		return nil
	}},
	{"SetNodeLinkEnabled|tok2|false", func(g GatewayProvider) error { return g.SetNodeLinkEnabled("tok2", false) }},
}

func TestSwitchingGatewayDelegates(t *testing.T) {
	remote := &recordingProvider{}
	// Local is a nil *RedisGateway, so a call that wrongly went local would panic.
	g := NewSwitchingGateway(nil, remote, func() (bool, bool) { return true, true })
	for _, c := range allCalls {
		if err := c.call(g); err != nil {
			t.Fatalf("%s: %v", c.want, err)
		}
	}
	if len(remote.calls) != len(allCalls) {
		t.Fatalf("remote saw %d calls, want %d: %v", len(remote.calls), len(allCalls), remote.calls)
	}
	for i, c := range allCalls {
		if remote.calls[i] != c.want {
			t.Errorf("call %d = %q, want %q", i, remote.calls[i], c.want)
		}
	}
}

func TestSwitchingGatewayFlipsPerCall(t *testing.T) {
	remote := &recordingProvider{}
	on := true
	g := NewSwitchingGateway(nil, remote, func() (bool, bool) { return on, true })
	if err := g.DeleteRoute("a.test"); err != nil || len(remote.calls) != 1 {
		t.Fatalf("remote call: %v %v", err, remote.calls)
	}
	on = false
	if p, err := g.pick(); err != nil || p != GatewayProvider(g.local) {
		t.Errorf("flipping the func did not switch to local: %T %v", p, err)
	}
}

func TestSwitchingGatewayNeverGuesses(t *testing.T) {
	cases := []struct {
		name string
		g    *SwitchingGateway
		want error
	}{
		{"remote without provider", NewSwitchingGateway(nil, nil, func() (bool, bool) { return true, true }), ErrGatewayRemote},
		{"mode unknown", NewSwitchingGateway(nil, &recordingProvider{}, func() (bool, bool) { return false, false }), ErrGatewayProviderUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.g.DeleteRoute("a.test"); !errors.Is(err, c.want) {
				t.Errorf("DeleteRoute = %v, want %v", err, c.want)
			}
			if _, err := c.g.SuspendCoreOwnedRoute("a.test", "x"); !errors.Is(err, c.want) {
				t.Errorf("SuspendCoreOwnedRoute = %v, want %v", err, c.want)
			}
			if c.g.LinkToken("1") != "" || c.g.DiscoveryProof("1") != "" {
				t.Error("string methods must return empty")
			}
		})
	}
}

func TestCachedProviderMode(t *testing.T) {
	clock := time.Unix(1000, 0)
	now := func() time.Time { return clock }
	calls, val, fail := 0, false, true
	c, reset := cachedProviderMode(5*time.Second, now, func() (bool, error) {
		calls++
		if fail {
			return false, errors.New("db down")
		}
		return val, nil
	})

	// First read fails: unknown, and the next read waits a second.
	if r, k := c(); r || k {
		t.Fatalf("first failing read = %v,%v, want false,false", r, k)
	}
	if _, k := c(); k || calls != 1 {
		t.Fatalf("cold retry inside a second (calls=%d)", calls)
	}
	clock = clock.Add(1100 * time.Millisecond)
	if _, k := c(); k || calls != 2 {
		t.Fatalf("no cold retry after a second (calls=%d)", calls)
	}
	// Recovery.
	clock = clock.Add(1100 * time.Millisecond)
	fail, val = false, true
	if r, k := c(); !r || !k || calls != 3 {
		t.Fatalf("after recovery = %v,%v (calls=%d)", r, k, calls)
	}
	// Inside the ttl nothing is re-read.
	val = false
	if r, _ := c(); !r || calls != 3 {
		t.Fatalf("answer changed inside the ttl (calls=%d)", calls)
	}
	// Past the ttl a failing read keeps the last good answer, for another ttl.
	clock = clock.Add(6 * time.Second)
	fail = true
	if r, k := c(); !r || !k || calls != 4 {
		t.Fatalf("error dropped the last good answer: %v,%v (calls=%d)", r, k, calls)
	}
	fail = false
	if r, k := c(); !r || !k || calls != 4 {
		t.Fatalf("re-read inside the ttl after a failure: %v,%v (calls=%d)", r, k, calls)
	}
	clock = clock.Add(6 * time.Second)
	if r, k := c(); r || !k || calls != 5 {
		t.Fatalf("after second recovery = %v,%v (calls=%d)", r, k, calls)
	}
	// A reset re-reads inside the ttl, so a saved setting applies at once.
	val = true
	reset()
	if r, k := c(); !r || !k || calls != 6 {
		t.Fatalf("after reset = %v,%v (calls=%d), want a fresh read", r, k, calls)
	}
}

// A database stall must not turn every caller into a settings read under the
// shared mutex: one read per ttl after a good answer, one per second before.
func TestCachedProviderModeUnderStall(t *testing.T) {
	clock := time.Unix(1000, 0)
	now := func() time.Time { return clock }
	calls, fail := 0, false
	c, _ := cachedProviderMode(5*time.Second, now, func() (bool, error) {
		calls++
		if fail {
			return false, errors.New("db stall")
		}
		return true, nil
	})
	if r, k := c(); !r || !k {
		t.Fatal("first read")
	}
	fail = true
	clock = clock.Add(6 * time.Second)
	for i := 0; i < 1000; i++ {
		if r, k := c(); !r || !k {
			t.Fatalf("call %d lost the last good answer", i)
		}
		clock = clock.Add(time.Millisecond) // 1000 calls over one second
	}
	if calls != 2 {
		t.Fatalf("settings reads during the outage = %d, want 1 (plus the first good read)", calls-1)
	}

	cold, coldCalls := time.Unix(5000, 0), 0
	cc, _ := cachedProviderMode(5*time.Second, func() time.Time { return cold }, func() (bool, error) {
		coldCalls++
		return false, errors.New("db down")
	})
	for i := 0; i < 1000; i++ {
		if _, k := cc(); k {
			t.Fatal("cold failure read as known")
		}
		cold = cold.Add(3 * time.Millisecond) // 1000 calls over three seconds
	}
	if coldCalls > 3 {
		t.Fatalf("cold-start reads = %d over three seconds, want at most one per second", coldCalls)
	}
}

// A settings read that hangs (a stale pooled connection) holds up the one
// caller doing it; every other caller answers at once.
func TestCachedProviderModeNeverBlocksOnAHungRead(t *testing.T) {
	var clockMu sync.Mutex
	clock := time.Unix(1000, 0)
	now := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	var calls atomic.Int32
	hang := make(chan struct{})
	block := false
	c, _ := cachedProviderMode(5*time.Second, now, func() (bool, error) {
		calls.Add(1)
		if block {
			<-hang
		}
		return true, nil
	})
	if r, k := c(); !r || !k {
		t.Fatal("first read")
	}
	block = true
	clockMu.Lock()
	clock = clock.Add(6 * time.Second)
	clockMu.Unlock()
	go c() // the refresher; it hangs
	for calls.Load() != 2 {
		time.Sleep(time.Millisecond)
	}
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r, k := c(); !r || !k {
				t.Error("a caller lost the last good answer")
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(hang)
		t.Fatal("callers are waiting behind the hung read")
	}
	if time.Since(start) > time.Second || calls.Load() != 2 {
		t.Fatalf("callers waited %v, reads %d; want at once and one read in flight", time.Since(start), calls.Load())
	}
	close(hang)

	// Cold start: nothing good yet, the others answer unknown at once.
	coldHang := make(chan struct{})
	var coldCalls atomic.Int32
	cc, _ := cachedProviderMode(5*time.Second, now, func() (bool, error) {
		coldCalls.Add(1)
		<-coldHang
		return true, nil
	})
	go cc()
	for coldCalls.Load() != 1 {
		time.Sleep(time.Millisecond)
	}
	answered := make(chan bool, 1)
	go func() { _, k := cc(); answered <- k }()
	select {
	case k := <-answered:
		if k || coldCalls.Load() != 1 {
			t.Fatalf("cold caller during a hung read: known %v, reads %d", k, coldCalls.Load())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cold caller waited behind the hung read")
	}
	close(coldHang)
}
