package services

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakePointingResolver answers from maps and counts every call. A non-nil
// block makes every call wait for it to close or for the context to end.
type fakePointingResolver struct {
	cname   map[string]string // host -> canonical name (no trailing dot)
	a, aaaa map[string][]netip.Addr
	srv     map[string][]*net.SRV
	fail    map[string]error // "<TYPE> <host>" -> error
	block   chan struct{}

	mu    sync.Mutex
	calls map[string]int
	names map[string]bool
}

func newFakePointingResolver() *fakePointingResolver {
	return &fakePointingResolver{
		cname: map[string]string{}, a: map[string][]netip.Addr{}, aaaa: map[string][]netip.Addr{},
		srv: map[string][]*net.SRV{}, fail: map[string]error{},
		calls: map[string]int{}, names: map[string]bool{},
	}
}

func (f *fakePointingResolver) enter(ctx context.Context, typ, host string) error {
	f.mu.Lock()
	f.calls[typ]++
	f.names[host] = true
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.fail[typ+" "+host]
}

func (f *fakePointingResolver) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

func (f *fakePointingResolver) count(typ string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[typ]
}

func (f *fakePointingResolver) seen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.names)
}

// LookupCNAME behaves like Go's: the canonical name, the name itself when it
// has address records but no alias, and not-found otherwise.
func (f *fakePointingResolver) LookupCNAME(ctx context.Context, h string) (string, error) {
	if err := f.enter(ctx, "CNAME", h); err != nil {
		return "", err
	}
	if c, ok := f.cname[h]; ok {
		return c + ".", nil
	}
	if len(f.a[h])+len(f.aaaa[h]) > 0 {
		return h + ".", nil
	}
	return "", errNXDomain
}

func (f *fakePointingResolver) LookupA(ctx context.Context, h string) ([]netip.Addr, error) {
	if err := f.enter(ctx, "A", h); err != nil {
		return nil, err
	}
	if v := f.a[h]; len(v) > 0 {
		return v, nil
	}
	return nil, errNXDomain
}

func (f *fakePointingResolver) LookupAAAA(ctx context.Context, h string) ([]netip.Addr, error) {
	if err := f.enter(ctx, "AAAA", h); err != nil {
		return nil, err
	}
	if v := f.aaaa[h]; len(v) > 0 {
		return v, nil
	}
	return nil, errNXDomain
}

func (f *fakePointingResolver) LookupSRV(ctx context.Context, h string) ([]*net.SRV, error) {
	if err := f.enter(ctx, "SRV", h); err != nil {
		return nil, err
	}
	if v := f.srv[h]; len(v) > 0 {
		return v, nil
	}
	return nil, errNXDomain
}

type pointingClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *pointingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *pointingClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

const ptName = "play.example.com"

var (
	ptEU1     = netip.MustParseAddr("203.0.113.10")
	ptEU2     = netip.MustParseAddr("203.0.113.11")
	ptUS      = netip.MustParseAddr("198.51.100.20")
	ptForeign = netip.MustParseAddr("192.0.2.50")
	ptV6      = netip.MustParseAddr("2001:db8::1")
)

func ptEdges() []GatewayEdgeInfo {
	return []GatewayEdgeInfo{
		{EdgeID: "e1", IP: ptEU1.String(), Status: "online", Region: "eu", Wildcard: "*.eu.dylaris.com"},
		{EdgeID: "e2", IP: ptEU2.String(), Status: "online", Region: "eu", Wildcard: "*.eu.dylaris.com"},
		{EdgeID: "e3", IP: ptUS.String(), Status: "online", Region: "us", Wildcard: "*.us.dylaris.com"},
	}
}

// newTestPointing builds a checker over a fixed edge list, an injected clock
// and an admission that admits every name.
func newTestPointing(res PointingResolver, edges []GatewayEdgeInfo) (*PointingChecker, *pointingClock) {
	clk := &pointingClock{t: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	c := NewPointingChecker(res, func(context.Context) []GatewayEdgeInfo { return edges })
	c.now = clk.Now
	c.SetAdmitted(func(context.Context, string) (bool, error) { return true, nil })
	return c, clk
}

type pointingRow struct {
	name    string
	host    string
	purpose PointingPurpose
	setup   func(f *fakePointingResolver)
	status  PointingStatus
	region  string
	via     string
	found   []string
	noCalls bool
}

func pointingRows() []pointingRow {
	srvTo := func(target string) []*net.SRV { return []*net.SRV{{Target: target + ".", Port: 25565}} }
	return []pointingRow{
		{name: "CNAME to our regional name", setup: func(f *fakePointingResolver) {
			f.cname[ptName] = "route.eu.dylaris.com"
		}, status: PointingOK, region: "eu", via: "cname"},
		{name: "CNAME to a foreign name whose A are our edges", setup: func(f *fakePointingResolver) {
			f.cname[ptName] = "lb.host.net"
			f.a[ptName] = []netip.Addr{ptEU1, ptEU2}
		}, status: PointingOK, region: "eu", via: "cname"},
		{name: "A all edges of one region", setup: func(f *fakePointingResolver) {
			f.a[ptName] = []netip.Addr{ptEU2, ptEU1}
		}, status: PointingOK, region: "eu", via: "a"},
		{name: "A one edge", setup: func(f *fakePointingResolver) {
			f.a[ptName] = []netip.Addr{ptEU1}
		}, status: PointingOK, region: "eu", via: "a"},
		{name: "partial", setup: func(f *fakePointingResolver) {
			f.a[ptName] = []netip.Addr{ptEU1, ptForeign}
		}, status: PointingPartial, found: []string{ptForeign.String()}},
		{name: "multi region", setup: func(f *fakePointingResolver) {
			f.a[ptName] = []netip.Addr{ptEU1, ptUS}
		}, status: PointingMultiRegion},
		{name: "elsewhere", setup: func(f *fakePointingResolver) {
			f.a[ptName] = []netip.Addr{ptForeign}
		}, status: PointingElsewhere, found: []string{ptForeign.String()}},
		{name: "AAAA beside edge A", setup: func(f *fakePointingResolver) {
			f.a[ptName] = []netip.Addr{ptEU1}
			f.aaaa[ptName] = []netip.Addr{ptV6}
		}, status: PointingIPv6},
		{name: "AAAA not judged with a CNAME to us for the voice region", purpose: PurposeVoiceRegion, setup: func(f *fakePointingResolver) {
			f.cname[ptName] = "route.eu.dylaris.com"
			f.aaaa[ptName] = []netip.Addr{ptV6}
		}, status: PointingOK, region: "eu", via: "cname"},
		{name: "AAAA judged with a CNAME to us for Bedrock", setup: func(f *fakePointingResolver) {
			f.cname[ptName] = "route.eu.dylaris.com"
			f.aaaa[ptName] = []netip.Addr{ptV6}
		}, status: PointingIPv6},
		{name: "SRV followed for the voice region", purpose: PurposeVoiceRegion, setup: func(f *fakePointingResolver) {
			f.srv["_minecraft._tcp."+ptName] = srvTo("mc.host.net")
			f.a["mc.host.net"] = []netip.Addr{ptUS}
		}, status: PointingOK, region: "us", via: "srv"},
		{name: "SRV to a regional name for the voice region", purpose: PurposeVoiceRegion, setup: func(f *fakePointingResolver) {
			f.srv["_minecraft._tcp."+ptName] = srvTo("s1.eu.dylaris.com")
		}, status: PointingOK, region: "eu", via: "srv"},
		{name: "SRV picks the lexically first target", purpose: PurposeVoiceRegion, setup: func(f *fakePointingResolver) {
			f.srv["_minecraft._tcp."+ptName] = []*net.SRV{{Target: "z.host.net.", Port: 25565}, {Target: "b.host.net.", Port: 25565}}
			f.a["b.host.net"] = []netip.Addr{ptEU1}
			f.a["z.host.net"] = []netip.Addr{ptUS}
		}, status: PointingOK, region: "eu", via: "srv"},
		{name: "SRV refused for Bedrock", purpose: PurposeBedrock, setup: func(f *fakePointingResolver) {
			f.srv["_minecraft._tcp."+ptName] = srvTo("mc.host.net")
			f.a["mc.host.net"] = []netip.Addr{ptEU1}
		}, status: PointingSRVOnly},
		{name: "SRV refused for a voice name", purpose: PurposeVoiceName, setup: func(f *fakePointingResolver) {
			f.srv["_minecraft._tcp."+ptName] = srvTo("mc.host.net")
			f.a["mc.host.net"] = []netip.Addr{ptEU1}
		}, status: PointingSRVOnly},
		{name: "SRV target with only SRV is not followed again", purpose: PurposeVoiceRegion, setup: func(f *fakePointingResolver) {
			f.srv["_minecraft._tcp."+ptName] = srvTo("t.host.net")
			f.srv["_minecraft._tcp.t.host.net"] = srvTo("mc.host.net")
			f.a["mc.host.net"] = []netip.Addr{ptEU1}
		}, status: PointingNotFound},
		{name: "nothing at all", status: PointingNotFound},
		{name: "IP literal", host: "203.0.113.10", status: PointingNotFound, noCalls: true},
		{name: "single label", host: "localhost", status: PointingNotFound, noCalls: true},
		{name: "longer than 253", host: strings.Repeat("a.", 127) + "com", status: PointingNotFound, noCalls: true},
	}
}

func runPointingRow(t *testing.T, row pointingRow) (Pointing, *fakePointingResolver) {
	t.Helper()
	f := newFakePointingResolver()
	if row.setup != nil {
		row.setup(f)
	}
	c, _ := newTestPointing(f, ptEdges())
	host := row.host
	if host == "" {
		host = ptName
	}
	purpose := row.purpose
	if purpose == "" {
		purpose = PurposeBedrock
	}
	return c.Check(context.Background(), host, purpose, nil), f
}

func TestPointingOutcomes(t *testing.T) {
	for _, row := range pointingRows() {
		t.Run(row.name, func(t *testing.T) {
			got, f := runPointingRow(t, row)
			if got.Status != row.status || got.Region != row.region || got.Via != row.via {
				t.Fatalf("got %+v, want status %s region %q via %q", got, row.status, row.region, row.via)
			}
			if len(got.Found) != 0 || len(row.found) != 0 {
				if !reflect.DeepEqual(got.Found, row.found) {
					t.Fatalf("found %v, want %v", got.Found, row.found)
				}
			}
			if row.noCalls && f.total() != 0 {
				t.Fatalf("made %d resolver calls, want none", f.total())
			}
		})
	}
	t.Run("SRV target hop asks the target once and no SRV of it", func(t *testing.T) {
		var row pointingRow
		for _, r := range pointingRows() {
			if r.name == "SRV target with only SRV is not followed again" {
				row = r
			}
		}
		_, f := runPointingRow(t, row)
		if f.count("SRV") != 1 {
			t.Fatalf("SRV calls %d, want 1 (the hop is not recursive)", f.count("SRV"))
		}
	})
}

func TestPointingBoundedLookups(t *testing.T) {
	for _, row := range pointingRows() {
		t.Run(row.name, func(t *testing.T) {
			_, f := runPointingRow(t, row)
			if n := f.total(); n > 7 {
				t.Fatalf("%d resolver calls, want at most 7", n)
			}
		})
	}
}

func TestPointingRegionalShortcutMakesNoLookup(t *testing.T) {
	f := newFakePointingResolver()
	c, _ := newTestPointing(f, ptEdges())
	var admissions atomic.Int32
	c.SetAdmitted(func(context.Context, string) (bool, error) { admissions.Add(1); return true, nil })
	got := c.Check(context.Background(), "MyServer.EU.dylaris.com.", PurposeBedrock, func() bool {
		t.Fatal("allow asked for a regional name")
		return false
	})
	if got.Status != PointingOK || got.Region != "eu" || got.Via != "regional" {
		t.Fatalf("got %+v, want ok/eu/regional", got)
	}
	if f.total() != 0 || admissions.Load() != 0 {
		t.Fatalf("resolver calls %d, admission calls %d, want none", f.total(), admissions.Load())
	}
	if region, ok := c.RegionOf(context.Background(), "myserver.eu.dylaris.com"); !ok || region != "eu" {
		t.Fatalf("RegionOf = %q %v, want eu", region, ok)
	}
	if _, ok := c.RegionOf(context.Background(), ptName); ok || f.total() != 0 {
		t.Fatalf("RegionOf of an own domain answered or resolved (%d calls)", f.total())
	}
}

func TestPointingPrivateAnswersNeverEchoed(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.0.0.5", "172.16.1.1", "192.168.1.1", "169.254.169.254",
		"100.64.1.1", "0.0.0.0", "fd00::1", "fe80::1", "::ffff:10.0.0.1"} {
		t.Run(s, func(t *testing.T) {
			f := newFakePointingResolver()
			f.a[ptName] = []netip.Addr{netip.MustParseAddr(s)}
			c, clk := newTestPointing(f, ptEdges())
			got := c.Check(context.Background(), ptName, PurposeBedrock, nil)
			if got.Status != PointingPrivate {
				t.Fatalf("got %+v, want private", got)
			}
			unmapped := netip.MustParseAddr(s).Unmap().String()
			msg := PointingRefusal(got, ptName, []string{"route.eu.dylaris.com"}, PurposeBedrock)
			for _, leak := range []string{s, unmapped} {
				if strings.Contains(msg, leak) || strings.Contains(fmt.Sprint(got.Found), leak) {
					t.Fatalf("private address %s echoed: %q / %v", leak, msg, got.Found)
				}
			}
			calls := f.total()
			clk.Add(29 * time.Second)
			again := c.Check(context.Background(), ptName, PurposeBedrock, nil)
			if again.Status != PointingPrivate || f.total() != calls {
				t.Fatalf("second call %+v with %d new calls, want a cached private", again, f.total()-calls)
			}
		})
	}
}

func TestPointingRegisteredEdgeIPIsOursWhateverItsRange(t *testing.T) {
	f := newFakePointingResolver()
	f.a[ptName] = []netip.Addr{netip.MustParseAddr("172.23.70.88")}
	edges := []GatewayEdgeInfo{{EdgeID: "wsl", IP: "172.23.70.88", Status: "online", Region: "eu", Wildcard: "*.eu.dylaris.test"}}
	c, _ := newTestPointing(f, edges)
	got := c.Check(context.Background(), ptName, PurposeBedrock, nil)
	if got.Status != PointingOK || got.Region != "eu" || got.Via != "a" {
		t.Fatalf("got %+v, want ok/eu/a", got)
	}
}

func TestPointingRecentlySeenEdgeStillOurs(t *testing.T) {
	f := newFakePointingResolver()
	for _, n := range []string{"a.example.com", "b.example.com", "c.example.com"} {
		f.a[n] = []netip.Addr{ptEU1}
	}
	var mu sync.Mutex
	edges := ptEdges()
	c, clk := newTestPointing(f, nil)
	c.edges = func(context.Context) []GatewayEdgeInfo {
		mu.Lock()
		defer mu.Unlock()
		return edges
	}
	if got := c.Check(context.Background(), "a.example.com", PurposeBedrock, nil); got.Status != PointingOK {
		t.Fatalf("with the edge registered: %+v", got)
	}
	// The edge restarts: its key is gone, its sibling is still there.
	mu.Lock()
	edges = ptEdges()[1:]
	mu.Unlock()
	clk.Add(5 * time.Minute)
	if got := c.Check(context.Background(), "b.example.com", PurposeBedrock, nil); got.Status != PointingOK || got.Region != "eu" {
		t.Fatalf("5 minutes after the edge left: %+v, want ok", got)
	}
	clk.Add(15 * time.Minute)
	if got := c.Check(context.Background(), "c.example.com", PurposeBedrock, nil); got.Status != PointingElsewhere {
		t.Fatalf("20 minutes after the edge left: %+v, want elsewhere", got)
	}
}

func TestPointingLookupFailureIsNeverANegative(t *testing.T) {
	failures := map[string]error{
		"timeout":   &net.DNSError{Err: "i/o timeout", IsTimeout: true},
		"servfail":  errServFail,
		"temporary": &net.DNSError{Err: "temporary failure", IsTemporary: true},
		"truncated": &net.DNSError{Err: "cannot unmarshal DNS message"},
		"cancelled": context.Canceled,
	}
	for _, typ := range []string{"CNAME", "A", "AAAA", "SRV"} {
		for label, ferr := range failures {
			t.Run(typ+" "+label, func(t *testing.T) {
				f := newFakePointingResolver()
				host := ptName
				if typ == "SRV" {
					host = "_minecraft._tcp." + ptName // reached only without an A answer
				} else {
					f.a[ptName] = []netip.Addr{ptForeign} // would be "elsewhere"
				}
				f.fail[typ+" "+host] = ferr
				c, _ := newTestPointing(f, ptEdges())
				for i := 0; i < 2; i++ {
					before := f.total()
					got := c.Check(context.Background(), ptName, PurposeBedrock, nil)
					if got.Status != PointingLookupFailed || got.Known() {
						t.Fatalf("call %d: got %+v, want lookup_failed", i, got)
					}
					if f.total() == before {
						t.Fatalf("call %d made no lookup: a failure was cached", i)
					}
				}
			})
		}
	}
}

func TestPointingEmptyEdgeRegistryIsUnknown(t *testing.T) {
	f := newFakePointingResolver()
	f.a[ptName] = []netip.Addr{ptForeign}
	var edges []GatewayEdgeInfo
	c, _ := newTestPointing(f, nil)
	c.edges = func(context.Context) []GatewayEdgeInfo { return edges }
	got := c.Check(context.Background(), ptName, PurposeBedrock, nil)
	if got.Status != PointingLookupFailed || f.total() != 0 {
		t.Fatalf("got %+v with %d calls, want lookup_failed and none", got, f.total())
	}
	edges = ptEdges()
	if got := c.Check(context.Background(), ptName, PurposeBedrock, nil); got.Status != PointingElsewhere {
		t.Fatalf("after the registry is back: %+v, want elsewhere (the unknown was not cached)", got)
	}
}

func TestPointingUnadmittedNameMakesNoLookup(t *testing.T) {
	cases := []struct {
		name  string
		admit func(context.Context, string) (bool, error)
		unset bool
	}{
		{name: "no route on the name", admit: func(context.Context, string) (bool, error) { return false, nil }},
		{name: "admission could not be read", admit: func(context.Context, string) (bool, error) { return false, errors.New("redis down") }},
		{name: "admission never wired", unset: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakePointingResolver()
			f.a[ptName] = []netip.Addr{ptEU1}
			c, _ := newTestPointing(f, ptEdges())
			if tc.unset {
				c.admitted = nil
			} else {
				c.SetAdmitted(tc.admit)
			}
			got := c.Check(context.Background(), ptName, PurposeBedrock, func() bool {
				t.Fatal("allow asked for a name that was not admitted")
				return false
			})
			if got.Status != PointingLookupFailed || f.total() != 0 {
				t.Fatalf("got %+v with %d calls, want lookup_failed and none", got, f.total())
			}
			c.SetAdmitted(func(context.Context, string) (bool, error) { return true, nil })
			if got := c.Check(context.Background(), ptName, PurposeBedrock, nil); got.Status != PointingOK {
				t.Fatalf("once admitted: %+v, want ok (the refusal was not cached)", got)
			}
		})
	}
}

func TestPointingHardDeadline(t *testing.T) {
	never := func(ctx context.Context) { <-ctx.Done() }
	cases := []struct {
		name  string
		setup func(c *PointingChecker, f *fakePointingResolver)
	}{
		{"resolver never answers", func(_ *PointingChecker, f *fakePointingResolver) { f.block = make(chan struct{}) }},
		{"edge registry read hangs", func(c *PointingChecker, _ *fakePointingResolver) {
			c.edges = func(ctx context.Context) []GatewayEdgeInfo { never(ctx); return ptEdges() }
		}},
		{"admission read hangs", func(c *PointingChecker, _ *fakePointingResolver) {
			c.SetAdmitted(func(ctx context.Context, _ string) (bool, error) { never(ctx); return true, nil })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakePointingResolver()
			f.a[ptName] = []netip.Addr{ptEU1}
			c, _ := newTestPointing(f, ptEdges())
			tc.setup(c, f)
			start := time.Now()
			done := make(chan Pointing, 1)
			go func() { done <- c.Check(context.Background(), ptName, PurposeBedrock, nil) }()
			select {
			case got := <-done:
				if got.Status != PointingLookupFailed {
					t.Fatalf("got %+v, want lookup_failed", got)
				}
				if el := time.Since(start); el > pointingTimeout+100*time.Millisecond {
					t.Fatalf("took %v, want at most %v", el, pointingTimeout+100*time.Millisecond)
				}
			case <-time.After(pointingTimeout + time.Second):
				t.Fatal("Check did not return: no total deadline")
			}
		})
	}
}

func TestPointingSlowResolverCannotPileUpGoroutines(t *testing.T) {
	f := newFakePointingResolver()
	f.block = make(chan struct{}) // never answers
	c, _ := newTestPointing(f, ptEdges())
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	var known atomic.Int32
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if c.Check(context.Background(), fmt.Sprintf("s%d.example.com", i), PurposeBedrock, nil).Known() {
				known.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if known.Load() != 0 {
		t.Fatalf("%d checks answered a verdict against a resolver that never answers", known.Load())
	}
	if n := f.seen(); n > pointingSlots {
		t.Fatalf("%d checks reached the resolver, want at most %d", n, pointingSlots)
	}
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > baseline+3 {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines %d, baseline %d: lookups outlived their deadline", runtime.NumGoroutine(), baseline)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPointingSlotsFullIsUnknown(t *testing.T) {
	f := newFakePointingResolver()
	release := make(chan struct{})
	f.block = release
	c, _ := newTestPointing(f, ptEdges())
	var wg sync.WaitGroup
	for i := 0; i < pointingSlots; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.Check(context.Background(), fmt.Sprintf("busy%d.example.com", i), PurposeBedrock, nil)
		}(i)
	}
	for start := time.Now(); f.seen() < pointingSlots; {
		if time.Since(start) > 2*time.Second {
			close(release)
			t.Fatalf("only %d checks reached the resolver", f.seen())
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	got := c.Check(ctx, "ninth.example.com", PurposeBedrock, nil)
	el := time.Since(start)
	close(release)
	wg.Wait()
	if got.Status != PointingLookupFailed {
		t.Fatalf("ninth check got %+v, want lookup_failed", got)
	}
	if el > 500*time.Millisecond {
		t.Fatalf("ninth check waited %v for a slot, want an immediate answer", el)
	}
	f.a["tenth.example.com"] = []netip.Addr{ptEU1}
	if got := c.Check(context.Background(), "tenth.example.com", PurposeBedrock, nil); got.Status != PointingOK {
		t.Fatalf("after the slots freed: %+v, want ok", got)
	}
}

func TestPointingCacheAndSingleFlight(t *testing.T) {
	counting := func(n *atomic.Int32, answer bool) func() bool {
		return func() bool { n.Add(1); return answer }
	}
	t.Run("ok cached 60 s, a hit asks neither DNS nor allow", func(t *testing.T) {
		f := newFakePointingResolver()
		f.a[ptName] = []netip.Addr{ptEU1}
		c, clk := newTestPointing(f, ptEdges())
		var allows atomic.Int32
		c.Check(context.Background(), ptName, PurposeBedrock, counting(&allows, true))
		calls := f.total()
		clk.Add(59 * time.Second)
		if got := c.Check(context.Background(), ptName, PurposeBedrock, counting(&allows, true)); got.Status != PointingOK || f.total() != calls || allows.Load() != 1 {
			t.Fatalf("at 59 s: %+v, %d new calls, %d allows; want a silent hit", got, f.total()-calls, allows.Load())
		}
		clk.Add(2 * time.Second)
		c.Check(context.Background(), ptName, PurposeBedrock, counting(&allows, true))
		if f.total() == calls || allows.Load() != 2 {
			t.Fatal("at 61 s the ok answer was still cached")
		}
	})
	t.Run("negative cached 30 s, per purpose", func(t *testing.T) {
		f := newFakePointingResolver()
		f.a[ptName] = []netip.Addr{ptForeign}
		c, clk := newTestPointing(f, ptEdges())
		c.Check(context.Background(), ptName, PurposeBedrock, nil)
		calls := f.total()
		clk.Add(29 * time.Second)
		if c.Check(context.Background(), ptName, PurposeBedrock, nil); f.total() != calls {
			t.Fatal("at 29 s the negative answer was not cached")
		}
		if c.Check(context.Background(), ptName, PurposeVoiceName, nil); f.total() == calls {
			t.Fatal("another purpose shared the cache entry")
		}
		calls = f.total()
		clk.Add(2 * time.Second)
		if c.Check(context.Background(), ptName, PurposeBedrock, nil); f.total() == calls {
			t.Fatal("at 31 s the negative answer was still cached")
		}
	})
	t.Run("allow false is rate_limited and not cached", func(t *testing.T) {
		f := newFakePointingResolver()
		f.a[ptName] = []netip.Addr{ptEU1}
		c, _ := newTestPointing(f, ptEdges())
		var allows atomic.Int32
		if got := c.Check(context.Background(), ptName, PurposeBedrock, counting(&allows, false)); got.Status != PointingRateLimited || got.Known() || f.total() != 0 {
			t.Fatalf("got %+v with %d calls, want rate_limited and none", got, f.total())
		}
		if got := c.Check(context.Background(), ptName, PurposeBedrock, counting(&allows, true)); got.Status != PointingOK {
			t.Fatalf("next allowed call: %+v, want ok", got)
		}
	})
	t.Run("concurrent callers share one lookup", func(t *testing.T) {
		f := newFakePointingResolver()
		f.a[ptName] = []netip.Addr{ptEU1}
		release := make(chan struct{})
		f.block = release
		c, _ := newTestPointing(f, ptEdges())
		var allows atomic.Int32
		results := make(chan Pointing, 20)
		for i := 0; i < 20; i++ {
			go func() { results <- c.Check(context.Background(), ptName, PurposeBedrock, counting(&allows, true)) }()
		}
		early, cancel := context.WithCancel(context.Background())
		earlyDone := make(chan Pointing, 1)
		go func() { earlyDone <- c.Check(early, ptName, PurposeBedrock, nil) }()
		for start := time.Now(); allows.Load() < 20; {
			if time.Since(start) > 2*time.Second {
				t.Fatal("callers did not reach the lookup")
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		cancel()
		if got := <-earlyDone; got.Status != PointingLookupFailed {
			t.Fatalf("the caller that left got %+v, want lookup_failed", got)
		}
		close(release)
		for i := 0; i < 20; i++ {
			if got := <-results; got.Status != PointingOK {
				t.Fatalf("caller %d got %+v, want the shared ok", i, got)
			}
		}
		for _, typ := range []string{"CNAME", "A", "AAAA"} {
			if n := f.count(typ); n != 1 {
				t.Fatalf("%s asked %d times, want once", typ, n)
			}
		}
	})
}

func TestPointingCacheIsBounded(t *testing.T) {
	f := newFakePointingResolver()
	c, _ := newTestPointing(f, ptEdges())
	for i := 0; i < pointingCacheMax+50; i++ {
		c.Check(context.Background(), fmt.Sprintf("n%d.example.com", i), PurposeBedrock, nil)
	}
	c.mu.Lock()
	n := len(c.cache)
	c.mu.Unlock()
	if n > pointingCacheMax {
		t.Fatalf("cache holds %d entries, want at most %d", n, pointingCacheMax)
	}
}

func TestPointingRefusalSentences(t *testing.T) {
	const target = "route.eu.dylaris.com"
	noTarget := "the address shown under Your own domains"
	found := []string{"192.0.2.50"}
	cases := []struct {
		status  PointingStatus
		purpose PointingPurpose
		want    []string
		target  bool // the sentence names the CNAME target
	}{
		{PointingOK, PurposeBedrock, nil, false},
		{PointingNotFound, PurposeBedrock, []string{"does not resolve yet", "Bedrock players connect straight"}, true},
		{PointingNotFound, PurposeVoiceRegion, []string{"Voice chat needs to know which region", "does not point at the gateway yet"}, true},
		{PointingNotFound, PurposeVoiceName, []string{"does not resolve yet"}, true},
		{PointingElsewhere, PurposeBedrock, []string{"points to 192.0.2.50, not to the gateway", "cannot carry Bedrock"}, true},
		{PointingElsewhere, PurposeVoiceRegion, []string{"Voice chat needs to know which region"}, true},
		{PointingElsewhere, PurposeVoiceName, []string{"points to 192.0.2.50, not to the gateway"}, true},
		{PointingSRVOnly, PurposeBedrock, []string{"only through an SRV record", "Bedrock ignores SRV records"}, true},
		{PointingSRVOnly, PurposeVoiceName, []string{"only through an SRV record"}, true},
		{PointingPartial, PurposeBedrock, []string{"points to the gateway and also to 192.0.2.50"}, true},
		{PointingMultiRegion, PurposeVoiceName, []string{"more than one of the gateway's regions"}, true},
		{PointingIPv6, PurposeBedrock, []string{"IPv6 (AAAA) record"}, true},
		{PointingPrivate, PurposeBedrock, []string{"not on the public internet"}, false},
	}
	for _, tc := range cases {
		for _, withTarget := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s %s target=%v", tc.status, tc.purpose, withTarget), func(t *testing.T) {
				var targets []string
				if withTarget {
					targets = []string{target}
				}
				p := Pointing{Status: tc.status, Found: found}
				msg := PointingRefusal(p, ptName, targets, tc.purpose)
				if tc.status == PointingOK {
					if msg != "" {
						t.Fatalf("ok has a refusal: %q", msg)
					}
					return
				}
				for _, w := range tc.want {
					if !strings.Contains(msg, w) {
						t.Fatalf("%q lacks %q", msg, w)
					}
				}
				if tc.target && withTarget && !strings.Contains(msg, target) {
					t.Fatalf("%q does not name the target", msg)
				}
				if tc.target && !withTarget && !strings.Contains(msg, noTarget) {
					t.Fatalf("%q does not point at Your own domains", msg)
				}
				if strings.Contains(msg, " route ") || strings.HasSuffix(msg, " route.") {
					t.Fatalf("%q names the bare label", msg)
				}
				if tc.status == PointingPrivate && strings.Contains(msg, found[0]) {
					t.Fatalf("private lists addresses: %q", msg)
				}
			})
		}
	}
	t.Run("several regions are all named", func(t *testing.T) {
		msg := PointingRefusal(Pointing{Status: PointingMultiRegion}, ptName, []string{"route.eu.dylaris.com", "route.us.dylaris.com"}, PurposeBedrock)
		if !strings.Contains(msg, "route.eu.dylaris.com") || !strings.Contains(msg, "route.us.dylaris.com") {
			t.Fatalf("%q", msg)
		}
	})
}

// dnsStub is a one-socket UDP DNS server on 127.0.0.1 that records every
// question and answers from a table. Enough of RFC 1035 to drive Go's
// pure resolver: no compression, one question.
type dnsStub struct {
	conn net.PacketConn
	mu   sync.Mutex
	qs   []string // "<qtype> <qname>"
	// answer returns the answer records for a question; nil is NXDOMAIN.
	answer func(qname string, qtype uint16) []stubRR
}

type stubRR struct {
	name  string
	typ   uint16
	cname string
	addr  netip.Addr
}

func startDNSStub(t *testing.T, answer func(string, uint16) []stubRR) *dnsStub {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &dnsStub{conn: conn, answer: answer}
	t.Cleanup(func() { conn.Close() })
	go s.serve()
	return s
}

func (s *dnsStub) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.qs...)
}

func stubEncodeName(n string) []byte {
	var b []byte
	for _, l := range strings.Split(strings.TrimSuffix(n, "."), ".") {
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	return append(b, 0)
}

func (s *dnsStub) serve() {
	buf := make([]byte, 1500)
	for {
		n, from, err := s.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		q := buf[:n]
		if len(q) < 12 {
			continue
		}
		var labels []string
		i := 12
		for i < len(q) && q[i] != 0 {
			l := int(q[i])
			if i+1+l > len(q) {
				break
			}
			labels = append(labels, string(q[i+1:i+1+l]))
			i += 1 + l
		}
		if i+5 > len(q) {
			continue
		}
		qname := strings.Join(labels, ".") + "."
		qtype := binary.BigEndian.Uint16(q[i+1:])
		question := q[12 : i+5]
		s.mu.Lock()
		s.qs = append(s.qs, fmt.Sprintf("%d %s", qtype, qname))
		s.mu.Unlock()

		rrs := s.answer(qname, qtype)
		flags := uint16(0x8180) // response, recursion desired and available
		if rrs == nil {
			flags |= 3 // NXDOMAIN
		}
		resp := make([]byte, 12)
		copy(resp[0:2], q[0:2])
		binary.BigEndian.PutUint16(resp[2:], flags)
		binary.BigEndian.PutUint16(resp[4:], 1)
		binary.BigEndian.PutUint16(resp[6:], uint16(len(rrs)))
		resp = append(resp, question...)
		for _, rr := range rrs {
			resp = append(resp, stubEncodeName(rr.name)...)
			var rdata []byte
			switch rr.typ {
			case 1:
				a := rr.addr.As4()
				rdata = a[:]
			case 28:
				a := rr.addr.As16()
				rdata = a[:]
			case 5:
				rdata = stubEncodeName(rr.cname)
			}
			hdr := make([]byte, 10)
			binary.BigEndian.PutUint16(hdr[0:], rr.typ)
			binary.BigEndian.PutUint16(hdr[2:], 1)
			binary.BigEndian.PutUint32(hdr[4:], 60)
			binary.BigEndian.PutUint16(hdr[8:], uint16(len(rdata)))
			resp = append(resp, hdr...)
			resp = append(resp, rdata...)
		}
		s.conn.WriteTo(resp, from)
	}
}

func stubResolver(s *dnsStub) *netServerDomainResolver {
	addr := s.conn.LocalAddr().String()
	return &netServerDomainResolver{r: &net.Resolver{PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", addr)
		}}}
}

// TestNetPointingResolverAsksAbsoluteNames drives the production resolver
// against a stub and pins what the check relies on: the name goes out
// absolute (no search domain), the canonical name of a one-hop CNAME is its
// target, and a name with no address records is a definitive not-found.
func TestNetPointingResolverAsksAbsoluteNames(t *testing.T) {
	s := startDNSStub(t, func(qname string, qtype uint16) []stubRR {
		switch qname {
		case "play.example.com.":
			chain := []stubRR{{name: qname, typ: 5, cname: "route.eu.dylaris.com."}}
			if qtype == 1 {
				chain = append(chain, stubRR{name: "route.eu.dylaris.com.", typ: 1, addr: ptEU1})
			}
			return chain
		case "chain.example.com.":
			chain := []stubRR{
				{name: qname, typ: 5, cname: "mid.example.net."},
				{name: "mid.example.net.", typ: 5, cname: "route.eu.dylaris.com."},
			}
			switch qtype {
			case 1:
				chain = append(chain, stubRR{name: "route.eu.dylaris.com.", typ: 1, addr: ptEU1})
			case 5:
				chain = chain[:1] // a CNAME query answers the first hop only
			}
			return chain
		case "srvonly.example.com.":
			return []stubRR{} // NOERROR, no data
		}
		return nil
	})
	var res PointingResolver = stubResolver(s)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	canon, err := res.LookupCNAME(ctx, ptName)
	if err != nil || normaliseHost(canon) != "route.eu.dylaris.com" {
		t.Fatalf("LookupCNAME = %q, %v; want route.eu.dylaris.com", canon, err)
	}
	for _, q := range s.asked() {
		if !strings.HasSuffix(q, " "+ptName+".") {
			t.Fatalf("asked %q: the name did not go out absolute", q)
		}
	}
	if len(s.asked()) == 0 {
		t.Fatal("the stub was never asked")
	}
	// Measured: for a two-hop chain Go answers the first hop OR the end,
	// depending on which query returns first (go1.26.11 on Linux gave the
	// first hop in 4 of 20 runs). So only a ONE-hop CNAME is judged by name;
	// a longer chain is judged by its A answers (row "CNAME to a foreign name
	// whose A are our edges"). Pinned so a change in either direction shows.
	if canon, err := res.LookupCNAME(ctx, "chain.example.com"); err != nil ||
		(normaliseHost(canon) != "route.eu.dylaris.com" && normaliseHost(canon) != "mid.example.net") {
		t.Fatalf("two-hop chain: LookupCNAME = %q, %v; want the first hop or the end of the chain", canon, err)
	}
	if _, err := res.LookupCNAME(ctx, "srvonly.example.com"); err == nil || lookupFailed(err) {
		t.Fatalf("a name without address records: err %v, want a definitive not-found", err)
	}
	if _, err := res.LookupCNAME(ctx, "missing.example.com"); err == nil || lookupFailed(err) {
		t.Fatalf("NXDOMAIN: err %v, want a definitive not-found", err)
	}
}
