package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRedisAddrFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok := loadRedisAddr(dir); ok {
		t.Fatal("an absent file loaded as an address")
	}
	if err := saveRedisAddr(dir, "redis:6379", time.Now()); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got, ok := loadRedisAddr(dir); !ok || got != "redis:6379" {
		t.Fatalf("load = %q, %v; want redis:6379, true", got, ok)
	}
	// A later answer replaces the file rather than failing on it.
	if err := saveRedisAddr(dir, "10.0.0.10:6379", time.Now()); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if got, _ := loadRedisAddr(dir); got != "10.0.0.10:6379" {
		t.Fatalf("after overwrite load = %q, want 10.0.0.10:6379", got)
	}
	// Written through a temp file and renamed: nothing may be left beside it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != redisAddrFileName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only %s", names, redisAddrFileName)
	}
	// Windows has no Unix permission bits: os.Stat reports 0666 there whatever
	// the file was created with, so the mode is only checkable elsewhere.
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, redisAddrFileName))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %o, want 600", fi.Mode().Perm())
		}
	}
}

// The next boot trusts this file before anything else, so a file it cannot
// read as exactly what it wrote must count as no file at all.
func TestRedisAddrFileIgnoresWhatItCannotTrust(t *testing.T) {
	cases := map[string]string{
		"not json":      "redis:6379",
		"other version": `{"v":2,"addr":"redis:6379","written_at":"2026-09-11T00:00:00Z"}`,
		"no version":    `{"addr":"redis:6379"}`,
		"empty address": `{"v":1,"addr":"  ","written_at":"2026-09-11T00:00:00Z"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, redisAddrFileName), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got, ok := loadRedisAddr(dir); ok {
				t.Errorf("loaded %q from %q, want it ignored", got, body)
			}
		})
	}
}

func TestResolveBootRedisAddr(t *testing.T) {
	cases := []struct {
		name     string
		env      string
		external bool
		cached   string
		wantAddr string
		wantSrc  redisAddrSource
	}{
		{
			// The file holds what Core last said, and Core wins once it has
			// answered - the variable is a fallback, never the winner.
			name: "the cached answer beats REDIS_ADDR", env: "redis:6379", cached: "10.0.0.10:6379",
			wantAddr: "10.0.0.10:6379", wantSrc: redisAddrFromFile,
		},
		{
			name: "REDIS_ADDR when nothing is cached", env: "redis:6379",
			wantAddr: "redis:6379", wantSrc: redisAddrFromEnv,
		},
		{
			name:     "neither: Core has to be asked",
			wantAddr: "", wantSrc: redisAddrFromCore,
		},
		{
			// Exactly the behaviour before: loopback to warp, whatever a file says.
			name: "the warp proxy ignores the cache", external: true, cached: "10.0.0.10:6379",
			wantAddr: "127.0.0.1:25571", wantSrc: redisAddrFromProxy,
		},
		{
			name: "an explicit REDIS_ADDR on an external node is not the proxy", env: "10.20.0.5:6379", external: true,
			wantAddr: "10.20.0.5:6379", wantSrc: redisAddrFromEnv,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nodeAddr, viaProxy := resolveNodeAddr(tc.env, tc.external, warpProxyRedisPort)
			addr, src := resolveBootRedisAddr(nodeAddr, viaProxy, tc.cached)
			if addr != tc.wantAddr || src != tc.wantSrc {
				t.Errorf("got %q from %q, want %q from %q", addr, src, tc.wantAddr, tc.wantSrc)
			}
		})
	}
}

// addrRecorder stands in for Redis and the disk: it records what was probed
// and what was written, and fails the probe when told to.
type addrRecorder struct {
	mu       sync.Mutex
	probes   []string
	saves    []string
	probeErr error
}

func testAddrState(rec *addrRecorder, start string, onDisk bool) *redisAddrState {
	s := &redisAddrState{
		probe: func(_ context.Context, addr string, _ []byte) error {
			rec.mu.Lock()
			defer rec.mu.Unlock()
			rec.probes = append(rec.probes, addr)
			return rec.probeErr
		},
		save: func(addr string) error {
			rec.mu.Lock()
			defer rec.mu.Unlock()
			rec.saves = append(rec.saves, addr)
			return nil
		},
	}
	s.set(start, onDisk)
	return s
}

func TestOfferPromotesOnlyAValidatedAddress(t *testing.T) {
	ctx := context.Background()

	t.Run("the same address is a no-op", func(t *testing.T) {
		rec := &addrRecorder{}
		s := testAddrState(rec, "redis:6379", true)
		s.offer(ctx, "redis:6379", nil)
		if len(rec.probes) != 0 || len(rec.saves) != 0 {
			t.Errorf("probed %v, saved %v; want neither", rec.probes, rec.saves)
		}
	})

	t.Run("empty is Core naming none, not nowhere", func(t *testing.T) {
		rec := &addrRecorder{}
		s := testAddrState(rec, "redis:6379", true)
		s.offer(ctx, "", nil)
		if got := s.current(); got != "redis:6379" {
			t.Errorf("current = %q, want the address it had", got)
		}
		if len(rec.probes) != 0 {
			t.Errorf("probed %v for an empty answer", rec.probes)
		}
	})

	t.Run("a different address that validates is promoted and cached", func(t *testing.T) {
		rec := &addrRecorder{}
		s := testAddrState(rec, "redis:6379", true)
		s.offer(ctx, "10.0.0.10:6379", nil)
		if got := s.current(); got != "10.0.0.10:6379" {
			t.Errorf("current = %q, want the validated address", got)
		}
		if len(rec.probes) != 1 || rec.probes[0] != "10.0.0.10:6379" {
			t.Errorf("probed %v, want exactly the candidate", rec.probes)
		}
		if len(rec.saves) != 1 || rec.saves[0] != "10.0.0.10:6379" {
			t.Errorf("saved %v, want the promoted address", rec.saves)
		}
	})

	t.Run("a different address that fails validation is kept out", func(t *testing.T) {
		rec := &addrRecorder{probeErr: errors.New("WRONGPASS")}
		s := testAddrState(rec, "redis:6379", true)
		s.offer(ctx, "10.0.0.10:6379", nil)
		if got := s.current(); got != "redis:6379" {
			t.Errorf("current = %q; the old address must stay in use", got)
		}
		if len(rec.saves) != 0 {
			t.Errorf("saved %v for an address that failed validation", rec.saves)
		}
	})

	t.Run("a boot with no address takes Core's answer, and caches it only after use", func(t *testing.T) {
		rec := &addrRecorder{}
		s := testAddrState(rec, "", false)
		s.offer(ctx, "redis:6379", nil)
		if got := s.current(); got != "redis:6379" {
			t.Fatalf("current = %q, want Core's answer", got)
		}
		if len(rec.probes) != 0 || len(rec.saves) != 0 {
			t.Fatalf("probed %v, saved %v; there is nothing to keep and nothing proven yet", rec.probes, rec.saves)
		}
		s.persistIfNew()
		s.persistIfNew()
		if len(rec.saves) != 1 || rec.saves[0] != "redis:6379" {
			t.Errorf("saved %v, want it written once after its first use", rec.saves)
		}
	})

	t.Run("an address read from the file is not written back", func(t *testing.T) {
		rec := &addrRecorder{}
		s := testAddrState(rec, "redis:6379", true)
		s.persistIfNew()
		if len(rec.saves) != 0 {
			t.Errorf("saved %v; it came from the file", rec.saves)
		}
	})

	t.Run("REDIS_ADDR is cached after its first use", func(t *testing.T) {
		rec := &addrRecorder{}
		s := testAddrState(rec, "redis:6379", false)
		s.persistIfNew()
		if len(rec.saves) != 1 || rec.saves[0] != "redis:6379" {
			t.Errorf("saved %v, want the variable's value cached", rec.saves)
		}
	})
}

// Two Core replicas answer the same node, and usually say the same thing. The
// second answer must find the first one's promotion, not validate again.
func TestTwoCoresWithTheSameAnswerValidateOnce(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	s := &redisAddrState{
		probe: func(context.Context, string, []byte) error {
			if calls.Add(1) == 1 {
				close(entered)
			}
			<-release
			return nil
		},
		save: func(string) error { return nil },
	}
	s.set("redis:6379", true)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.offer(context.Background(), "10.0.0.10:6379", nil) }()
	<-entered
	go func() { defer wg.Done(); s.offer(context.Background(), "10.0.0.10:6379", nil) }()
	// Long enough for the second answer to reach the lock the first holds.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Fatalf("validated %d times, want 1", n)
	}
	if got := s.current(); got != "10.0.0.10:6379" {
		t.Errorf("current = %q, want the promoted address", got)
	}
}

func TestFollowsCoreRedisAddr(t *testing.T) {
	cases := []struct {
		name          string
		clusterSecret string
		viaProxy      bool
		want          bool
	}{
		{name: "in-cluster node", clusterSecret: "s", want: true},
		// The escape hatch for a port collision: Core never answers an owned
		// node, so a cached copy of this would outrank every later edit of it.
		{name: "BYON node with an explicit REDIS_ADDR", clusterSecret: "", want: false},
		{name: "BYON node on the warp proxy", clusterSecret: "", viaProxy: true, want: false},
		{name: "external node holding the secret, on the warp proxy", clusterSecret: "s", viaProxy: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := followsCoreRedisAddr(tc.clusterSecret, tc.viaProxy); got != tc.want {
				t.Errorf("followsCoreRedisAddr(%q, %v) = %v, want %v", tc.clusterSecret, tc.viaProxy, got, tc.want)
			}
		})
	}
}

// A node that does not follow Core keeps what it resolved: the proxy's loopback
// (warp follows the real address) or a BYON machine's own REDIS_ADDR.
func TestANodeThatDoesNotFollowCoreIgnoresItsAddress(t *testing.T) {
	origState, origFollows := nodeRedis, redisAddrFollowsCore
	t.Cleanup(func() { nodeRedis, redisAddrFollowsCore = origState, origFollows })
	redisAddrFollowsCore = false

	for _, start := range []string{"127.0.0.1:25571", "10.20.0.5:6380"} {
		rec := &addrRecorder{}
		nodeRedis = testAddrState(rec, start, false)

		noteCoreRedisAddr(context.Background(), "redis:6379", nil)

		if got := nodeRedis.current(); got != start {
			t.Errorf("current = %q, want %q", got, start)
		}
		if len(rec.probes) != 0 || len(rec.saves) != 0 {
			t.Errorf("probed %v, saved %v; this node must ignore Core's field", rec.probes, rec.saves)
		}
	}
}

// The address go-redis passes the Dialer is Options.Addr, frozen when the client
// was built. A promote only takes effect if the Dialer reads the current address
// at the moment of each dial instead.
func TestRedisDialerDialsTheCurrentAddressAtDialTime(t *testing.T) {
	listen := func() *net.TCPListener {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		return l.(*net.TCPListener)
	}
	accepts := func(l *net.TCPListener) bool {
		_ = l.SetDeadline(time.Now().Add(2 * time.Second))
		c, err := l.Accept()
		if err != nil {
			return false
		}
		c.Close()
		return true
	}
	a, b := listen(), listen()

	var mu sync.Mutex
	cur := a.Addr().String()
	dial := redisDialer(func() string {
		mu.Lock()
		defer mu.Unlock()
		return cur
	})

	for _, want := range []*net.TCPListener{a, b} {
		mu.Lock()
		cur = want.Addr().String()
		mu.Unlock()
		// A name that cannot resolve: if the Dialer used it, the dial would fail.
		conn, err := dial(context.Background(), "tcp", "frozen.invalid:6379")
		if err != nil {
			t.Fatalf("dial with current %s: %v", want.Addr(), err)
		}
		conn.Close()
		if !accepts(want) {
			t.Errorf("no connection reached %s, the current address", want.Addr())
		}
	}
}
