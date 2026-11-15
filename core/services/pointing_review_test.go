package services

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// I1: the SRV target is a name the customer chose and is resolved without
// admission, so its answer must not tell "exists inside our network" from
// "does not exist". A private answer, a failed lookup and an edge-plus-
// private answer of the hop all read exactly like a target with no address.
func TestPointingSRVHopCannotProbeInternalNames(t *testing.T) {
	const internal = "tasks.dylaris_core"
	srv := []*net.SRV{{Target: internal + ".", Port: 25565}}
	cases := []struct {
		name  string
		setup func(f *fakePointingResolver)
	}{
		{"target does not exist", func(*fakePointingResolver) {}},
		{"target answers a private address", func(f *fakePointingResolver) {
			f.a[internal] = []netip.Addr{netip.MustParseAddr("10.0.1.5")}
		}},
		{"target lookup fails", func(f *fakePointingResolver) {
			f.fail["A "+internal] = errServFail
		}},
		{"target answers an edge and a private address", func(f *fakePointingResolver) {
			f.a[internal] = []netip.Addr{ptEU1, netip.MustParseAddr("10.0.1.5")}
		}},
	}
	var sentences []string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakePointingResolver()
			f.srv["_minecraft._tcp."+ptName] = srv
			tc.setup(f)
			c, _ := newTestPointing(f, ptEdges())
			got := c.Check(context.Background(), ptName, PurposeVoiceRegion, nil)
			if got.Status != PointingNotFound || len(got.Found) != 0 {
				t.Fatalf("got %+v, want not_found with nothing found", got)
			}
			sentences = append(sentences, PointingRefusal(got, ptName, nil, PurposeVoiceRegion))
		})
	}
	for _, s := range sentences[1:] {
		if s != sentences[0] {
			t.Fatalf("sentences differ, the hop is an oracle: %q vs %q", s, sentences[0])
		}
	}
}

// flipCNAME answers a two-hop chain the way Go's pure resolver does: the
// first hop or the end, depending on which parallel answer it read first.
type flipCNAME struct {
	*fakePointingResolver
	n atomic.Int32
}

func (f *flipCNAME) LookupCNAME(ctx context.Context, h string) (string, error) {
	if err := f.enter(ctx, "CNAME", h); err != nil {
		return "", err
	}
	if f.n.Add(1)%2 == 0 {
		return "mid.example.net.", nil
	}
	return "route.eu.dylaris.com.", nil
}

// I2: play -> mid -> route.eu (our name, A = our edge, AAAA at the end). For
// the voice purposes the verdict must not depend on which hop the resolver
// reports, and an AAAA at the end of the alias is not judged.
func TestPointingTwoHopChainIsDeterministic(t *testing.T) {
	f := &flipCNAME{fakePointingResolver: newFakePointingResolver()}
	f.a[ptName] = []netip.Addr{ptEU1}
	f.aaaa[ptName] = []netip.Addr{ptV6}
	if got := flipVerdicts(f, PurposeVoiceRegion); len(got) != 1 || got["ok/eu/cname"] != 40 {
		t.Fatalf("verdicts %v, want ok/eu/cname every time", got)
	}
	if f.n.Load() < 40 {
		t.Fatalf("CNAME asked %d times", f.n.Load())
	}
}

// flipVerdicts runs 40 fresh checks (no cache between them) and counts the
// verdicts as status/region/via.
func flipVerdicts(f *flipCNAME, purpose PointingPurpose) map[string]int {
	verdicts := map[string]int{}
	for i := 0; i < 40; i++ {
		c, _ := newTestPointing(f, ptEdges())
		got := c.Check(context.Background(), ptName, purpose, nil)
		verdicts[fmt.Sprintf("%s/%s/%s", got.Status, got.Region, got.Via)]++
	}
	return verdicts
}

// m4: our regional names have no AAAA, so for Bedrock and a white-label voice
// name (players connect to that name) any AAAA in the answer, at the end of an
// alias included, is ipv6. The voice region lookup keeps the alias exemption.
// Both verdicts are the same whichever hop the resolver reports (flipCNAME),
// and for a fixed one-hop alias to a foreign name.
func TestPointingBedrockAAAAAtTheEndOfAnAliasIsIPv6(t *testing.T) {
	for _, c := range []struct {
		purpose PointingPurpose
		want    string
	}{
		{PurposeBedrock, "ipv6//"},
		{PurposeVoiceRegion, "ok/eu/cname"},
		{PurposeVoiceName, "ipv6//"},
	} {
		t.Run(string(c.purpose), func(t *testing.T) {
			f := &flipCNAME{fakePointingResolver: newFakePointingResolver()}
			f.a[ptName] = []netip.Addr{ptEU1}
			f.aaaa[ptName] = []netip.Addr{ptV6}
			if got := flipVerdicts(f, c.purpose); len(got) != 1 || got[c.want] != 40 {
				t.Fatalf("two-hop verdicts %v, want %s every time", got, c.want)
			}

			one := newFakePointingResolver()
			one.cname[ptName] = "lb.host.net"
			one.a[ptName] = []netip.Addr{ptEU1}
			one.aaaa[ptName] = []netip.Addr{ptV6}
			chk, _ := newTestPointing(one, ptEdges())
			got := chk.Check(context.Background(), ptName, c.purpose, nil)
			if v := fmt.Sprintf("%s/%s/%s", got.Status, got.Region, got.Via); v != c.want {
				t.Fatalf("one-hop alias %s, want %s", v, c.want)
			}
		})
	}
}

// Without an alias the AAAA rule still holds: an AAAA beside our A on the
// customer's own name is theirs and leads elsewhere.
func TestPointingAAAAStillJudgedWithoutAnAlias(t *testing.T) {
	f := newFakePointingResolver()
	f.a[ptName] = []netip.Addr{ptEU1}
	f.aaaa[ptName] = []netip.Addr{ptV6}
	c, _ := newTestPointing(f, ptEdges())
	if got := c.Check(context.Background(), ptName, PurposeBedrock, nil); got.Status != PointingIPv6 {
		t.Fatalf("got %+v, want ipv6", got)
	}
}

// M-f: one of our edges plus a private address is partial, not ok (some
// clients would be sent to an address that is not on the internet) and not
// private (the name does reach us). The sentence says so without the address.
func TestPointingEdgePlusPrivateIsPartial(t *testing.T) {
	f := newFakePointingResolver()
	f.a[ptName] = []netip.Addr{ptEU1, netip.MustParseAddr("192.168.1.20")}
	c, _ := newTestPointing(f, ptEdges())
	got := c.Check(context.Background(), ptName, PurposeBedrock, nil)
	if got.Status != PointingPartial || len(got.Found) != 0 {
		t.Fatalf("got %+v, want partial with nothing echoed", got)
	}
	msg := PointingRefusal(got, ptName, nil, PurposeBedrock)
	if msg != ptName+" points to the gateway and also to an address that is not on the public internet. Remove the records that do not lead to the gateway, or use a CNAME to the address shown under Your own domains." {
		t.Fatalf("%q", msg)
	}
}

// M-a: Known is a whitelist, so a zero Pointing is unknown, and the refusal
// sentence of an unknown verdict is empty: no caller can refuse on it by
// accident. The retry sentences live in PointingUnknownText.
func TestPointingUnknownIsNeverARefusal(t *testing.T) {
	for _, p := range []Pointing{{}, {Status: "bogus"}, {Status: PointingLookupFailed}, {Status: PointingRateLimited}} {
		if p.Known() {
			t.Fatalf("%+v reads as known", p)
		}
		if msg := PointingRefusal(p, ptName, nil, PurposeBedrock); msg != "" {
			t.Fatalf("%+v has a refusal %q", p, msg)
		}
	}
	if got := PointingUnknownText(Pointing{Status: PointingLookupFailed}, ptName); got != "The lookup of "+ptName+" failed right now. Nothing was changed. Try again in a moment." {
		t.Fatalf("lookup_failed text %q", got)
	}
	if got := PointingUnknownText(Pointing{}, ptName); got != "The lookup of "+ptName+" failed right now. Nothing was changed. Try again in a moment." {
		t.Fatalf("zero value text %q", got)
	}
	if got := PointingUnknownText(Pointing{Status: PointingRateLimited}, ptName); got != "Too many DNS checks. Wait a minute." {
		t.Fatalf("rate_limited text %q", got)
	}
	for _, s := range []PointingStatus{PointingOK, PointingNotFound, PointingElsewhere, PointingPartial, PointingMultiRegion, PointingIPv6, PointingSRVOnly, PointingPrivate} {
		p := Pointing{Status: s}
		if !p.Known() || PointingUnknownText(p, ptName) != "" {
			t.Fatalf("%s: known %v, unknown text %q", s, p.Known(), PointingUnknownText(p, ptName))
		}
	}
}

// M-b: a nil checker is "could not check", never a panic.
func TestPointingNilCheckerIsUnknown(t *testing.T) {
	var c *PointingChecker
	if got := c.Check(context.Background(), ptName, PurposeBedrock, nil); got.Status != PointingLookupFailed {
		t.Fatalf("got %+v", got)
	}
	if _, ok := c.RegionOf(context.Background(), "a.eu.dylaris.com"); ok {
		t.Fatal("nil checker found a region")
	}
}

// M-e: when the edge read or the admission used up the caller's deadline, no
// shared lookup is started any more.
func TestPointingNoLookupAfterTheDeadline(t *testing.T) {
	f := newFakePointingResolver()
	f.a[ptName] = []netip.Addr{ptEU1}
	c, _ := newTestPointing(f, ptEdges())
	c.SetAdmitted(func(ctx context.Context, _ string) (bool, error) { <-ctx.Done(); return true, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if got := c.Check(ctx, ptName, PurposeBedrock, nil); got.Status != PointingLookupFailed {
		t.Fatalf("got %+v", got)
	}
	time.Sleep(100 * time.Millisecond)
	if f.total() != 0 {
		t.Fatalf("%d resolver calls after the deadline, want none", f.total())
	}
}

// Fairness: one owner holds at most pointingPerOwner of the shared slots; a
// further check of the same owner answers "could not check" at once, another
// owner still gets a lookup.
func TestPointingPerOwnerConcurrency(t *testing.T) {
	f := newFakePointingResolver()
	release := make(chan struct{})
	f.block = release
	c, _ := newTestPointing(f, ptEdges())
	var wg sync.WaitGroup
	for i := 0; i < pointingPerOwner; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.CheckFor(context.Background(), "owner-a", fmt.Sprintf("a%d.example.com", i), PurposeBedrock, nil)
		}(i)
	}
	for start := time.Now(); f.seen() < pointingPerOwner; {
		if time.Since(start) > 2*time.Second {
			close(release)
			t.Fatalf("only %d checks reached the resolver", f.seen())
		}
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	got := c.CheckFor(context.Background(), "owner-a", "a-more.example.com", PurposeBedrock, nil)
	if got.Status != PointingLookupFailed || time.Since(start) > 500*time.Millisecond {
		close(release)
		t.Fatalf("third check of one owner: %+v after %v, want lookup_failed at once", got, time.Since(start))
	}
	other := make(chan Pointing, 1)
	go func() { other <- c.CheckFor(context.Background(), "owner-b", "b.example.com", PurposeBedrock, nil) }()
	for start := time.Now(); f.seen() < pointingPerOwner+1; {
		if time.Since(start) > 2*time.Second {
			close(release)
			t.Fatal("another owner did not reach the resolver")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	<-other
	// The slots are released: owner-a may check again.
	f.a["a-again.example.com"] = []netip.Addr{ptEU1}
	if got := c.CheckFor(context.Background(), "owner-a", "a-again.example.com", PurposeBedrock, nil); got.Status != PointingOK {
		t.Fatalf("after release: %+v", got)
	}
}

// m1 (review 3-4): callers that leave at once do not free their owner's
// count; the lookups they started do, when they end. One owner sending eight
// distinct checks and dropping each connection holds pointingPerOwner slots,
// not all of them, and another owner still gets a lookup.
func TestPointingPerOwnerCapCountsRunningLookups(t *testing.T) {
	f := newFakePointingResolver()
	release := make(chan struct{})
	f.block = release
	defer close(release)
	c, _ := newTestPointing(f, ptEdges())
	saw := func(name string) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.names[name]
	}
	for i := 0; i < pointingSlots; i++ {
		name := fmt.Sprintf("a%d.example.com", i)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan Pointing, 1)
		go func() { done <- c.CheckFor(ctx, "owner-a", name, PurposeBedrock, nil) }()
		var got Pointing
	wait:
		for start := time.Now(); ; {
			select {
			case got = <-done:
				break wait
			default:
			}
			if saw(name) {
				cancel() // the caller leaves; its lookup keeps running
				got = <-done
				break wait
			}
			if time.Since(start) > 2*time.Second {
				t.Fatalf("check %d neither returned nor reached the resolver", i)
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
		if got.Status != PointingLookupFailed {
			t.Fatalf("check %d: %+v, want lookup_failed", i, got)
		}
	}
	if n := f.seen(); n != pointingPerOwner {
		t.Fatalf("one owner started %d lookups, want %d", n, pointingPerOwner)
	}
	if n := len(c.slots); n != pointingPerOwner {
		t.Fatalf("one owner holds %d slots, want %d", n, pointingPerOwner)
	}
	go c.CheckFor(context.Background(), "owner-b", "b.example.com", PurposeBedrock, nil)
	for start := time.Now(); !saw("b.example.com"); {
		if time.Since(start) > 2*time.Second {
			t.Fatal("another owner did not reach the resolver")
		}
		time.Sleep(time.Millisecond)
	}
}

// m3 (review 3-4): a failed SRV hop reads exactly like an absent or private
// target (TestPointingSRVHopCannotProbeInternalNames), but it is not cached:
// the next try asks again. Absent and private targets stay cached.
func TestPointingFailedSRVHopIsNotCached(t *testing.T) {
	const target = "mc.host.example.net"
	for _, c := range []struct {
		name   string
		setup  func(f *fakePointingResolver)
		cached bool
	}{
		{"target lookup fails", func(f *fakePointingResolver) { f.fail["A "+target] = errServFail }, false},
		{"target does not exist", func(*fakePointingResolver) {}, true},
		{"target answers a private address", func(f *fakePointingResolver) {
			f.a[target] = []netip.Addr{netip.MustParseAddr("10.0.1.5")}
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakePointingResolver()
			f.srv["_minecraft._tcp."+ptName] = []*net.SRV{{Target: target + ".", Port: 25565}}
			c.setup(f)
			chk, _ := newTestPointing(f, ptEdges())
			first := chk.Check(context.Background(), ptName, PurposeVoiceRegion, nil)
			srvs := f.count("SRV")
			second := chk.Check(context.Background(), ptName, PurposeVoiceRegion, nil)
			for _, got := range []Pointing{first, second} {
				if got.Status != PointingNotFound || len(got.Found) != 0 {
					t.Fatalf("got %+v, want not_found with nothing found", got)
				}
			}
			if asked := f.count("SRV") > srvs; asked == c.cached {
				t.Fatalf("second check asked DNS again: %v, want cached %v", asked, c.cached)
			}
		})
	}
}

// failGetHook fails the GET of every key containing match with err: a
// connection error is a Redis that drops mid-read, redis.Nil a key that
// expired between SCAN and GET.
type failGetHook struct {
	match string
	err   error
}

func (failGetHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h failGetHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "get" && strings.Contains(fmt.Sprint(cmd.Args()...), h.match) {
			cmd.SetErr(h.err)
			return h.err
		}
		return next(ctx, cmd)
	}
}
func (failGetHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// M-c: a registry read that did not complete gives the pointing check no
// list at all (unknown), while a key that expired between SCAN and GET is
// simply not there. The other readers keep the list they always had.
func TestGatewayEdgeRegistryIncompleteReadIsEmpty(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	mr.Set("edge:registry:e1", `{"edge_id":"e1","status":"online","region":"eu","wildcard":"*.eu.dylaris.com","ip":"203.0.113.10"}`)
	mr.Set("edge:registry:e2", `{"edge_id":"e2","status":"online","region":"us","wildcard":"*.us.dylaris.com","ip":"198.51.100.20"}`)
	ctx := context.Background()
	if got := GatewayEdgeRegistry(ctx, rdb); len(got) != 2 {
		t.Fatalf("healthy read: %d edges, want 2", len(got))
	}
	broken := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { broken.Close() })
	broken.AddHook(failGetHook{"e2", errors.New("connection reset by peer")})
	if got := GatewayEdgeRegistry(ctx, broken); got != nil {
		t.Fatalf("one failed GET: %v, want nil (unknown), not the partial list", got)
	}
	if got := listEdgeRegistry(ctx, broken); len(got) != 1 || got[0].EdgeID != "e1" {
		t.Fatalf("listEdgeRegistry must skip only the failed key: %v", got)
	}
	_, err := readEdgeRegistry(ctx, rdb, true)
	if err != nil {
		t.Fatalf("healthy read error %v", err)
	}
	if !strings.Contains(fmt.Sprint(readEdgeRegistryErr(ctx, broken)), "connection reset") {
		t.Fatal("a failed GET is not reported")
	}
	expired := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { expired.Close() })
	expired.AddHook(failGetHook{"e2", redis.Nil})
	if got, err := readEdgeRegistry(ctx, expired, true); err != nil || len(got) != 1 {
		t.Fatalf("expired keys: %v, %v; want an empty complete read", got, err)
	}
}

func readEdgeRegistryErr(ctx context.Context, rdb *redis.Client) error {
	_, err := readEdgeRegistry(ctx, rdb, true)
	return err
}
