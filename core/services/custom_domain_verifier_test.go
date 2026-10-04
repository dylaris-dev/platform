package services

import (
	"context"
	"errors"
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
