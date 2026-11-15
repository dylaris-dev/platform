package services

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"dylaris-core/store"
)

// verifierFakeStore embeds store.Store (nil) so it satisfies the interface at
// compile time; only what the verifier touches is implemented.
type verifierFakeStore struct {
	store.Store
	pending  []store.CustomDomainClaim
	verified []int
	failed   []int
	nextFail string
	// current overrides what a re-read returns; nil means the listed claim.
	current *store.CustomDomainClaim
	tokens  []string
	rearmed []string
	// linksOff is the billing cutoff having switched the tenant's links off.
	linksOff bool
}

func (f *verifierFakeStore) GetCustomDomainClaim(userID, domain string) (*store.CustomDomainClaim, error) {
	if f.current != nil {
		return f.current, nil
	}
	for i := range f.pending {
		if f.pending[i].UserID == userID && f.pending[i].Domain == domain {
			c := f.pending[i]
			return &c, nil
		}
	}
	return nil, store.ErrNoClaim
}
func (f *verifierFakeStore) SetCustomDomainTXTToken(_ int, token string) error {
	f.tokens = append(f.tokens, token)
	return nil
}
func (f *verifierFakeStore) StartCustomDomainClaim(_, domain string, _ time.Time) (*store.CustomDomainClaim, error) {
	f.rearmed = append(f.rearmed, domain)
	return &store.CustomDomainClaim{Domain: domain, State: store.ClaimPending}, nil
}

func (f *verifierFakeStore) ListPendingClaims() ([]store.CustomDomainClaim, error) {
	return f.pending, nil
}

// No verified claims to re-check unless a test says so (recheckFakeStore).
func (f *verifierFakeStore) ListClaimsDueRecheck(time.Duration, int) ([]store.CustomDomainClaim, error) {
	return nil, nil
}
func (f *verifierFakeStore) GetUserBilling(userID string) (*store.UserBilling, error) {
	return &store.UserBilling{UserID: userID, Status: "active", NodeLinksOff: f.linksOff}, nil
}
func (f *verifierFakeStore) MarkCustomDomainVerified(id int) error {
	f.verified = append(f.verified, id)
	return nil
}
func (f *verifierFakeStore) FailCustomDomainClaim(id int) (string, error) {
	f.failed = append(f.failed, id)
	if f.nextFail == "" {
		return store.ClaimBlocked, nil
	}
	return f.nextFail, nil
}

type fakeRemover struct {
	removed []string
	err     error
}

func (r *fakeRemover) DeleteRoutesForDomain(_ context.Context, _, domain string) error {
	if r.err != nil {
		return r.err
	}
	r.removed = append(r.removed, domain)
	return nil
}

const claimToken = "dylaris-verify=0123456789abcdef0123456789abcdef"

func claim(id int, domain string, deadline time.Time) store.CustomDomainClaim {
	return store.CustomDomainClaim{ID: id, UserID: "u1", Domain: domain,
		State: store.ClaimPending, DeadlineAt: &deadline, TXTToken: claimToken}
}

// published is a resolver on which mc.example.com carries this claim's record.
func published() *fakeResolver {
	return &fakeResolver{txt: map[string][]string{TXTVerifyPrefix + "mc.example.com": {claimToken}}}
}

func TestVerifierProvesBeforeItPunishes(t *testing.T) {
	past := time.Now().Add(-time.Minute)

	// The case that matters: the record went live shortly before the deadline.
	// Failing it because this tick happened to run after the deadline would
	// punish a customer who did everything right, seconds late.
	t.Run("a proven claim past its deadline is verified, not blocked", func(t *testing.T) {
		fs := &verifierFakeStore{pending: []store.CustomDomainClaim{claim(1, "mc.example.com", past)}}
		rm := &fakeRemover{}
		NewCustomDomainVerifier(fs, published(), rm).RunOnce(context.Background())

		if len(fs.verified) != 1 || fs.verified[0] != 1 {
			t.Errorf("claim was not verified: %v", fs.verified)
		}
		if len(fs.failed) != 0 {
			t.Errorf("a proven claim was also failed: %v", fs.failed)
		}
		if len(rm.removed) != 0 {
			t.Errorf("routes were removed for a proven claim: %v", rm.removed)
		}
	})

	t.Run("an unproven claim past its deadline loses its routes and is blocked", func(t *testing.T) {
		fs := &verifierFakeStore{pending: []store.CustomDomainClaim{claim(2, "mc.example.com", past)}}
		rm := &fakeRemover{}
		NewCustomDomainVerifier(fs, &fakeResolver{}, rm).RunOnce(context.Background())

		if len(fs.failed) != 1 || fs.failed[0] != 2 {
			t.Errorf("claim was not failed: %v", fs.failed)
		}
		// The route must go BEFORE the block is recorded, or an unproven domain
		// keeps being served.
		if len(rm.removed) != 1 || rm.removed[0] != "mc.example.com" {
			t.Errorf("routes were not removed: %v", rm.removed)
		}
	})

	// Inside the grant nothing happens: no proof yet is the normal state for the
	// first few hours, not a failure.
	t.Run("an unproven claim inside its grant is left alone", func(t *testing.T) {
		future := time.Now().Add(2 * time.Hour)
		fs := &verifierFakeStore{pending: []store.CustomDomainClaim{claim(3, "mc.example.com", future)}}
		rm := &fakeRemover{}
		NewCustomDomainVerifier(fs, &fakeResolver{}, rm).RunOnce(context.Background())

		if len(fs.failed) != 0 || len(rm.removed) != 0 || len(fs.verified) != 0 {
			t.Errorf("a claim inside its grant was acted on: failed=%v removed=%v verified=%v",
				fs.failed, rm.removed, fs.verified)
		}
	})

	t.Run("a claim with no deadline is never failed", func(t *testing.T) {
		fs := &verifierFakeStore{pending: []store.CustomDomainClaim{
			{ID: 4, UserID: "u1", Domain: "mc.example.com", State: store.ClaimPending},
		}}
		NewCustomDomainVerifier(fs, &fakeResolver{}, &fakeRemover{}).RunOnce(context.Background())
		if len(fs.failed) != 0 {
			t.Errorf("a claim without a deadline was failed: %v", fs.failed)
		}
	})
}

// The proof is the claim's own token, and nothing else.
//
// It used to be "the domain resolves to us", which every tenant's claim on the
// same name passes alike: a CNAME a former customer left behind, or any name
// under a wildcard pointed at us, was proven for whoever entered it next.
func TestOnlyTheClaimsOwnTokenProvesIt(t *testing.T) {
	future := time.Now().Add(2 * time.Hour)
	cases := []struct {
		name string
		res  *fakeResolver
	}{
		{"a domain that merely points at us", &fakeResolver{
			cname: map[string]string{"mc.example.com": "route.eu.dylaris.com."},
			hosts: map[string][]string{"mc.example.com": {"203.0.113.10"}},
		}},
		{"another claim's token", &fakeResolver{
			txt: map[string][]string{TXTVerifyPrefix + "mc.example.com": {"dylaris-verify=ffffffffffffffffffffffffffffffff"}},
		}},
		{"the token on the wrong label", &fakeResolver{
			txt: map[string][]string{"mc.example.com": {claimToken}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &verifierFakeStore{pending: []store.CustomDomainClaim{claim(1, "mc.example.com", future)}}
			NewCustomDomainVerifier(fs, tc.res, &fakeRemover{}).RunOnce(context.Background())
			if len(fs.verified) != 0 {
				t.Fatalf("verified without this claim's TXT record: %v", fs.verified)
			}
		})
	}

	t.Run("a claim with no token is never proven", func(t *testing.T) {
		c := claim(1, "mc.example.com", future)
		c.TXTToken = ""
		fs := &verifierFakeStore{pending: []store.CustomDomainClaim{c}}
		res := &fakeResolver{txt: map[string][]string{TXTVerifyPrefix + "mc.example.com": {""}}}
		NewCustomDomainVerifier(fs, res, &fakeRemover{}).RunOnce(context.Background())
		if len(fs.verified) != 0 {
			t.Fatalf("an empty token proved a claim: %v", fs.verified)
		}
	})
}

// A claim failed while its route is still up is never looked at again, so the
// route would serve an unproven domain for good. It stays pending instead.
func TestAFailedRouteRemovalDoesNotFailTheClaim(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	fs := &verifierFakeStore{pending: []store.CustomDomainClaim{claim(1, "mc.example.com", past)}}
	rm := &fakeRemover{err: errors.New("redis down")}
	NewCustomDomainVerifier(fs, &fakeResolver{}, rm).RunOnce(context.Background())
	if len(fs.failed) != 0 {
		t.Fatalf("claim failed although its route could not be removed: %v", fs.failed)
	}
}

// slowResolver answers one host slowly, the way a tenant's own nameserver can.
type slowResolver struct {
	fakeResolver
	slowHost string
	delay    time.Duration
}

func (s *slowResolver) LookupTXT(ctx context.Context, host string) ([]string, error) {
	if host == s.slowHost {
		time.Sleep(s.delay)
	}
	return s.fakeResolver.LookupTXT(ctx, host)
}

// A claim is judged at the moment its own record was looked at. The deadline
// used to be read once, after the whole pass, so a slow claim later in the pass
// failed an earlier one that was checked well inside its grant.
func TestASlowPassDoesNotFailAClaimCheckedInTime(t *testing.T) {
	deadline := time.Now().Add(150 * time.Millisecond)
	early := claim(1, "early.example.com", deadline)
	slow := claim(2, "slow.example.com", time.Now().Add(time.Hour))
	fs := &verifierFakeStore{pending: []store.CustomDomainClaim{early, slow}}
	res := &slowResolver{slowHost: TXTVerifyPrefix + "slow.example.com", delay: 400 * time.Millisecond}
	NewCustomDomainVerifier(fs, res, &fakeRemover{}).RunOnce(context.Background())
	if len(fs.failed) != 0 {
		t.Fatalf("a claim checked before its deadline was failed by a slow pass: %v", fs.failed)
	}
}

// The grant must be comfortably longer than the poll interval, or a customer
// who configures DNS correctly could still miss the window between two ticks.
func TestPollIntervalFitsInsideTheGrant(t *testing.T) {
	if CustomDomainPollEvery >= CustomDomainGrant {
		t.Fatalf("poll interval %v does not fit inside the grant %v",
			CustomDomainPollEvery, CustomDomainGrant)
	}
	if CustomDomainGrant/CustomDomainPollEvery < 4 {
		t.Errorf("only %d polls fit inside the grant; a customer gets too few chances",
			CustomDomainGrant/CustomDomainPollEvery)
	}
}

// flippingElection is a leader flag that can change while the verifier holds
// it, which is the whole point: leadership is not a boot-time fact.
// (The package already has an immutable fakeElection.)
type flippingElection struct{ leader bool }

func (f *flippingElection) IsLeader() bool { return f.leader }

// A follower must not touch a claim.
//
// The gate was a one-shot check around Start in main.go, which is not what
// leadership is: the lease is acquired asynchronously, so on a cold start
// IsLeader() is still false and the verifier was never started at all - and
// after a failover the new leader had not started it at its own boot either.
// Both failures are silent. Claims simply stop being looked at.
func TestVerifierOnlyActsAsLeader(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	newVerifier := func(fs *verifierFakeStore, rm *fakeRemover) *CustomDomainVerifier {
		return NewCustomDomainVerifier(fs, published(), rm)
	}

	t.Run("a follower skips the pass entirely", func(t *testing.T) {
		fs := &verifierFakeStore{pending: []store.CustomDomainClaim{claim(1, "mc.example.com", past)}}
		rm := &fakeRemover{}
		v := newVerifier(fs, rm)
		v.SetLeader(&flippingElection{leader: false})
		if v.shouldRun() {
			t.Fatal("a follower would run the pass and race the leader on the same claims")
		}
	})

	t.Run("the leader runs it, and a follower promoted later does too", func(t *testing.T) {
		fs := &verifierFakeStore{pending: []store.CustomDomainClaim{claim(1, "mc.example.com", past)}}
		rm := &fakeRemover{}
		v := newVerifier(fs, rm)
		// The exact cold-start shape: not leader yet when Start would have been
		// called, leader by the time a tick fires.
		el := &flippingElection{leader: false}
		v.SetLeader(el)
		if v.shouldRun() {
			t.Fatal("ran before the lease was acquired")
		}
		el.leader = true
		if !v.shouldRun() {
			t.Fatal("did not resume once the lease was acquired; this is the failover hole")
		}
		v.RunOnce(context.Background())
		if len(fs.verified) != 1 || fs.verified[0] != 1 {
			t.Errorf("verified = %v, want [1]", fs.verified)
		}
	})

	t.Run("no election wired means ungated", func(t *testing.T) {
		v := newVerifier(&verifierFakeStore{}, &fakeRemover{})
		if !v.shouldRun() {
			t.Error("a verifier with no election must still run")
		}
	})
}

// The pass decides on a list read at its start. A customer who verified the
// claim ("check now") or re-armed it in the meantime must keep their routes.
func TestAClaimThatMovedOnDuringThePassKeepsItsRoutes(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	later := time.Now().Add(time.Hour)
	for name, cur := range map[string]store.CustomDomainClaim{
		"verified meanwhile": {ID: 1, UserID: "u1", Domain: "mc.example.com", State: store.ClaimVerified},
		"re-armed meanwhile": {ID: 1, UserID: "u1", Domain: "mc.example.com", State: store.ClaimPending, DeadlineAt: &later},
	} {
		t.Run(name, func(t *testing.T) {
			cur := cur
			fs := &verifierFakeStore{pending: []store.CustomDomainClaim{claim(1, "mc.example.com", past)}, current: &cur}
			rm := &fakeRemover{}
			NewCustomDomainVerifier(fs, &fakeResolver{}, rm).RunOnce(context.Background())
			if len(rm.removed) != 0 || len(fs.failed) != 0 {
				t.Fatalf("acted on a stale read: removed=%v failed=%v", rm.removed, fs.failed)
			}
		})
	}
}

// A claim from before the TXT proof has no token, and its customer was told to
// set a CNAME. It gets a token and a fresh grant, not a failure.
func TestAClaimWithoutATokenGetsOneAndAFreshGrant(t *testing.T) {
	c := claim(1, "mc.example.com", time.Now().Add(-time.Minute))
	c.TXTToken = ""
	fs := &verifierFakeStore{pending: []store.CustomDomainClaim{c}}
	rm := &fakeRemover{}
	NewCustomDomainVerifier(fs, &fakeResolver{}, rm).RunOnce(context.Background())
	if len(fs.failed) != 0 || len(rm.removed) != 0 {
		t.Fatalf("a tokenless claim was failed: failed=%v removed=%v", fs.failed, rm.removed)
	}
	if len(fs.tokens) != 1 || len(fs.rearmed) != 1 {
		t.Fatalf("tokens=%v rearmed=%v, want one of each", fs.tokens, fs.rearmed)
	}
}

// --- The daily Bedrock DNS re-check (display only). ---

// bedrockDNSStore is the verifier's store plus the Bedrock rows and
// address_dns_checks. Edition writes are counted so a test can say none
// happened; claim writes are recorded by recheckFakeStore.
type bedrockDNSStore struct {
	*recheckFakeStore
	hosted   []store.RouteEdition
	links    []store.LinkRouteEdition
	linksErr error
	dns      map[string]store.AddressDNSCheck
	recorded []string
	swept    [][]string
	writes   int
}

func newBedrockDNSStore() *bedrockDNSStore {
	return &bedrockDNSStore{recheckFakeStore: &recheckFakeStore{}, dns: map[string]store.AddressDNSCheck{}}
}

func (f *bedrockDNSStore) ListRouteEditions(int) ([]store.RouteEdition, error) { return f.hosted, nil }
func (f *bedrockDNSStore) ListLinkRouteEditions() ([]store.LinkRouteEdition, error) {
	return f.links, f.linksErr
}
func (f *bedrockDNSStore) SetRouteEdition(int, string, bool, bool) error { f.writes++; return nil }
func (f *bedrockDNSStore) DeleteRouteEdition(string) error               { f.writes++; return nil }
func (f *bedrockDNSStore) SetLinkRouteEdition(string, bool, bool, []byte) error {
	f.writes++
	return nil
}
func (f *bedrockDNSStore) GetAddressDNSChecks(names []string) (map[string]store.AddressDNSCheck, error) {
	out := map[string]store.AddressDNSCheck{}
	for _, n := range names {
		if c, ok := f.dns[n]; ok {
			out[n] = c
		}
	}
	return out, nil
}
func (f *bedrockDNSStore) RecordAddressDNSCheck(name, status, region string, ok bool) (store.AddressDNSCheck, error) {
	f.recorded = append(f.recorded, name)
	c := f.dns[name]
	c.Name, c.Status, c.Region, c.CheckedAt = name, status, region, time.Now()
	if ok {
		c.Misses, c.FailingSince = 0, nil
	} else {
		c.Misses++
		if c.FailingSince == nil {
			at := c.CheckedAt
			c.FailingSince = &at
		}
	}
	f.dns[name] = c
	return c, nil
}
func (f *bedrockDNSStore) DeleteAddressDNSChecksNotIn(names []string) (int64, error) {
	f.swept = append(f.swept, append([]string(nil), names...))
	keep := map[string]bool{}
	for _, n := range names {
		keep[n] = true
	}
	var n int64
	for k := range f.dns {
		if !keep[k] {
			delete(f.dns, k)
			n++
		}
	}
	return n, nil
}

func bedrockVerifier(fs *bedrockDNSStore, res PointingResolver, txt DomainResolver, rm RouteRemover) *CustomDomainVerifier {
	v := NewCustomDomainVerifier(fs, txt, rm)
	c, _ := newTestPointing(res, ptEdges())
	v.SetPointing(c)
	return v
}

func hostedBedrockRow(domain string) store.RouteEdition {
	return store.RouteEdition{Domain: domain, ServerID: 7, ServerUUID: "srv-7", Java: true, Bedrock: true}
}

// A miss is stored with its start; no edition row is written. The verifier
// holds no Redis client, so it cannot publish sys:route:editions at all.
func TestBedrockRecheckDisplayOnly(t *testing.T) {
	fs := newBedrockDNSStore()
	fs.hosted = []store.RouteEdition{hostedBedrockRow(ptName), hostedBedrockRow("s.eu.dylaris.com"),
		{Domain: "java.example.com", ServerID: 7, Java: true}}
	fs.links = []store.LinkRouteEdition{{Domain: "Link.Example.com", OwnerID: "u1", Java: true, Bedrock: true}}
	res := newFakePointingResolver()
	res.a[ptName] = []netip.Addr{ptForeign}
	res.a["link.example.com"] = []netip.Addr{ptEU1}
	bedrockVerifier(fs, res, &fakeResolver{}, &fakeRemover{}).RunOnce(context.Background())

	if row := fs.dns[ptName]; row.Status != string(PointingElsewhere) || row.FailingSince == nil || row.Misses != 1 {
		t.Fatalf("hosted row %+v", row)
	}
	if row := fs.dns["link.example.com"]; row.Status != string(PointingOK) || row.Region != "eu" || row.FailingSince != nil {
		t.Fatalf("route-only row %+v", row)
	}
	// The regional address and the Java-only one are never checked.
	if len(fs.dns) != 2 || res.names["s.eu.dylaris.com"] || res.names["java.example.com"] {
		t.Fatalf("rows %v, resolved %v", fs.dns, res.names)
	}
	if fs.writes != 0 {
		t.Fatalf("%d edition writes", fs.writes)
	}
}

// Review Focus 1: "could not check" writes nothing and keeps the last answer.
func TestBedrockRecheckLookupFailedWritesNothing(t *testing.T) {
	fs := newBedrockDNSStore()
	fs.hosted = []store.RouteEdition{hostedBedrockRow(ptName)}
	old := store.AddressDNSCheck{Name: ptName, Status: "ok", Region: "eu", CheckedAt: time.Now().Add(-25 * time.Hour)}
	fs.dns[ptName] = old
	res := newFakePointingResolver()
	res.a[ptName] = []netip.Addr{ptForeign}
	res.fail["A "+ptName] = errServFail
	bedrockVerifier(fs, res, &fakeResolver{}, &fakeRemover{}).RunOnce(context.Background())
	if res.total() == 0 {
		t.Fatal("the due name was not checked")
	}
	if len(fs.recorded) != 0 || fs.dns[ptName] != old || fs.writes != 0 {
		t.Fatalf("recorded %v row %+v writes %d", fs.recorded, fs.dns[ptName], fs.writes)
	}
}

// A pointing miss on a name that also has a verified claim does not count
// toward the claim's lapse, and removes no route.
func TestBedrockRecheckNeverTouchesAClaim(t *testing.T) {
	fs := newBedrockDNSStore()
	fs.due = []store.CustomDomainClaim{verifiedClaim(1)}
	fs.hosted = []store.RouteEdition{hostedBedrockRow("mc.example.com")}
	rm := &fakeRemover{}
	res := newFakePointingResolver()
	res.a["mc.example.com"] = []netip.Addr{ptForeign}
	bedrockVerifier(fs, res, published(), rm).RunOnce(context.Background())
	if fs.dns["mc.example.com"].Status != string(PointingElsewhere) {
		t.Fatalf("row %+v", fs.dns["mc.example.com"])
	}
	// verified [1] is the TXT re-check, which still passes.
	if len(fs.verified) != 1 || len(fs.misses) != 0 || len(fs.lapsed) != 0 || len(fs.failed) != 0 || len(rm.removed) != 0 {
		t.Fatalf("verified %v misses %v lapsed %v failed %v removed %v", fs.verified, fs.misses, fs.lapsed, fs.failed, rm.removed)
	}
}

// At most customDomainRecheckBatch names a pass, the rest the next pass; a
// row checked within the day is skipped, one older than a day is due.
func TestBedrockRecheckBatchAndDue(t *testing.T) {
	fs := newBedrockDNSStore()
	res := newFakePointingResolver()
	for i := 0; i < 150; i++ {
		d := fmt.Sprintf("s%03d.example.com", i)
		fs.hosted = append(fs.hosted, hostedBedrockRow(d))
		res.a[d] = []netip.Addr{ptEU1}
	}
	fs.hosted = append(fs.hosted, hostedBedrockRow("fresh.example.com"), hostedBedrockRow("stale.example.com"))
	fs.dns["fresh.example.com"] = store.AddressDNSCheck{Name: "fresh.example.com", Status: "ok", Region: "eu", CheckedAt: time.Now().Add(-time.Hour)}
	fs.dns["stale.example.com"] = store.AddressDNSCheck{Name: "stale.example.com", Status: "ok", Region: "eu", CheckedAt: time.Now().Add(-25 * time.Hour)}
	v := bedrockVerifier(fs, res, &fakeResolver{}, &fakeRemover{})

	v.RunOnce(context.Background())
	if len(fs.recorded) != customDomainRecheckBatch {
		t.Fatalf("pass 1 checked %d, want %d", len(fs.recorded), customDomainRecheckBatch)
	}
	for _, n := range fs.recorded {
		if n == "fresh.example.com" || n == "stale.example.com" {
			t.Fatalf("pass 1 checked %s before the never-checked names", n)
		}
	}
	fs.recorded = nil
	v.RunOnce(context.Background())
	got := map[string]bool{}
	for _, n := range fs.recorded {
		got[n] = true
	}
	if len(fs.recorded) != 51 || !got["stale.example.com"] || got["fresh.example.com"] {
		t.Fatalf("pass 2 checked %d (stale %v, fresh %v), want the 50 left plus stale", len(fs.recorded), got["stale.example.com"], got["fresh.example.com"])
	}
	fs.recorded = nil
	v.RunOnce(context.Background())
	if len(fs.recorded) != 0 {
		t.Fatalf("pass 3 checked %v, every row is fresh", fs.recorded)
	}
}

// A customer Core has no edges and checks nothing; neither does one whose
// mode is unknown.
func TestBedrockRecheckIdleInRemoteMode(t *testing.T) {
	for name, mode := range map[string]func() (bool, bool){
		"remote":  func() (bool, bool) { return true, true },
		"unknown": func() (bool, bool) { return false, false },
	} {
		fs := newBedrockDNSStore()
		fs.hosted = []store.RouteEdition{hostedBedrockRow(ptName)}
		res := newFakePointingResolver()
		v := bedrockVerifier(fs, res, &fakeResolver{}, &fakeRemover{})
		v.SetGatewayRemote(mode)
		v.RunOnce(context.Background())
		if res.total() != 0 || len(fs.recorded) != 0 || len(fs.swept) != 0 {
			t.Fatalf("%s: lookups %d recorded %v swept %v", name, res.total(), fs.recorded, fs.swept)
		}
	}
}

// A follower runs no pass, so it checks no Bedrock address either.
func TestBedrockRecheckFollowerIdle(t *testing.T) {
	fs := newBedrockDNSStore()
	fs.hosted = []store.RouteEdition{hostedBedrockRow(ptName)}
	res := newFakePointingResolver()
	v := bedrockVerifier(fs, res, &fakeResolver{}, &fakeRemover{})
	v.SetLeader(&flippingElection{leader: false})
	ctx, cancel := context.WithCancel(context.Background())
	v.Start(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()
	if res.total() != 0 || len(fs.recorded) != 0 || len(fs.swept) != 0 {
		t.Fatalf("a follower checked: lookups %d recorded %v swept %v", res.total(), fs.recorded, fs.swept)
	}
}

// Rows of names that no longer have Bedrock are swept once a pass, after the
// checks; a failed read sweeps nothing.
func TestBedrockRecheckSweepsUnusedRows(t *testing.T) {
	now := time.Now()
	fs := newBedrockDNSStore()
	fs.hosted = []store.RouteEdition{hostedBedrockRow(ptName)}
	fs.dns[ptName] = store.AddressDNSCheck{Name: ptName, Status: "ok", Region: "eu", CheckedAt: now}
	fs.dns["gone.example.com"] = store.AddressDNSCheck{Name: "gone.example.com", Status: "ok", CheckedAt: now}
	res := newFakePointingResolver()
	bedrockVerifier(fs, res, &fakeResolver{}, &fakeRemover{}).RunOnce(context.Background())
	if len(fs.swept) != 1 || len(fs.swept[0]) != 1 || fs.swept[0][0] != ptName {
		t.Fatalf("swept %v", fs.swept)
	}
	if _, ok := fs.dns["gone.example.com"]; ok || len(fs.dns) != 1 {
		t.Fatalf("rows %v", fs.dns)
	}

	broken := newBedrockDNSStore()
	broken.linksErr = errors.New("pq: connection reset by peer")
	broken.dns["kept.example.com"] = store.AddressDNSCheck{Name: "kept.example.com", Status: "ok", CheckedAt: now}
	bedrockVerifier(broken, res, &fakeResolver{}, &fakeRemover{}).RunOnce(context.Background())
	if len(broken.swept) != 0 || len(broken.dns) != 1 {
		t.Fatalf("a failed read swept %v, rows %v", broken.swept, broken.dns)
	}
}
