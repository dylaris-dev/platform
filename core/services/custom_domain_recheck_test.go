package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"dylaris-core/store"
)

// recheckFakeStore records what the re-check pass writes. failingSince is what
// the store answers for a miss: the start of the run of misses.
type recheckFakeStore struct {
	verifierFakeStore
	due          []store.CustomDomainClaim
	failingSince time.Time
	misses       []int
	lapsed       []int
	lapseErr     error
}

func (f *recheckFakeStore) ListClaimsDueRecheck(time.Duration, int) ([]store.CustomDomainClaim, error) {
	return f.due, nil
}
func (f *recheckFakeStore) RecheckFailedCustomDomainClaim(id int) (time.Time, error) {
	f.misses = append(f.misses, id)
	return f.failingSince, nil
}
func (f *recheckFakeStore) LapseCustomDomainClaim(id int, _ time.Duration) error {
	if f.lapseErr != nil {
		return f.lapseErr
	}
	f.lapsed = append(f.lapsed, id)
	return nil
}
func (f *recheckFakeStore) GetCustomDomainClaim(userID, domain string) (*store.CustomDomainClaim, error) {
	if f.current != nil {
		return f.current, nil
	}
	for i := range f.due {
		if f.due[i].UserID == userID && f.due[i].Domain == domain {
			c := f.due[i]
			c.FailingSince = &f.failingSince
			return &c, nil
		}
	}
	return nil, store.ErrNoClaim
}

func verifiedClaim(id int) store.CustomDomainClaim {
	return store.CustomDomainClaim{ID: id, UserID: "u1", Domain: "mc.example.com",
		State: store.ClaimVerified, TXTToken: claimToken}
}

// A proof used to be forever. A domain that expired or was sold kept reaching
// the old tenant's server, and the new owner could not route their own name.
func TestAVerifiedDomainIsReChecked(t *testing.T) {
	t.Run("the record is still there: it stays verified", func(t *testing.T) {
		fs := &recheckFakeStore{due: []store.CustomDomainClaim{verifiedClaim(1)}}
		rm := &fakeRemover{}
		NewCustomDomainVerifier(fs, published(), rm).RunOnce(context.Background())
		if len(fs.verified) != 1 || len(fs.misses) != 0 || len(rm.removed) != 0 {
			t.Fatalf("verified=%v misses=%v removed=%v", fs.verified, fs.misses, rm.removed)
		}
	})

	t.Run("the record is gone: the grace starts, nothing is removed yet", func(t *testing.T) {
		fs := &recheckFakeStore{due: []store.CustomDomainClaim{verifiedClaim(1)}, failingSince: time.Now()}
		rm := &fakeRemover{}
		NewCustomDomainVerifier(fs, &fakeResolver{}, rm).RunOnce(context.Background())
		if len(fs.misses) != 1 || len(rm.removed) != 0 || len(fs.lapsed) != 0 {
			t.Fatalf("misses=%v removed=%v lapsed=%v", fs.misses, rm.removed, fs.lapsed)
		}
	})

	t.Run("gone for the whole grace: routes first, then the claim lapses", func(t *testing.T) {
		long := time.Now().Add(-CustomDomainLapseGrace - time.Minute)
		fs := &recheckFakeStore{due: []store.CustomDomainClaim{verifiedClaim(1)}, failingSince: long}
		rm := &fakeRemover{}
		NewCustomDomainVerifier(fs, &fakeResolver{}, rm).RunOnce(context.Background())
		if len(rm.removed) != 1 || len(fs.lapsed) != 1 {
			t.Fatalf("removed=%v lapsed=%v, want both", rm.removed, fs.lapsed)
		}
		// No strike: a domain that changed hands is not a failed attempt.
		if len(fs.failed) != 0 {
			t.Fatalf("a lapse counted as a failed attempt: %v", fs.failed)
		}
	})

	t.Run("one minute short of the grace keeps its routes", func(t *testing.T) {
		short := time.Now().Add(-CustomDomainLapseGrace + time.Minute)
		fs := &recheckFakeStore{due: []store.CustomDomainClaim{verifiedClaim(1)}, failingSince: short}
		rm := &fakeRemover{}
		NewCustomDomainVerifier(fs, &fakeResolver{}, rm).RunOnce(context.Background())
		if len(rm.removed) != 0 || len(fs.lapsed) != 0 {
			t.Fatalf("removed=%v lapsed=%v inside the grace", rm.removed, fs.lapsed)
		}
	})
}

// A route that could not be removed keeps the claim verified and failing, so
// the next pass tries again; lapsing it would stop the re-checks for good.
func TestALapseWaitsForTheRoutesToGo(t *testing.T) {
	long := time.Now().Add(-CustomDomainLapseGrace - time.Minute)
	fs := &recheckFakeStore{due: []store.CustomDomainClaim{verifiedClaim(1)}, failingSince: long}
	rm := &fakeRemover{err: errors.New("redis down")}
	NewCustomDomainVerifier(fs, &fakeResolver{}, rm).RunOnce(context.Background())
	if len(fs.lapsed) != 0 {
		t.Fatalf("lapsed although the routes are still up: %v", fs.lapsed)
	}
}

// "Check now" between the list and the removal ends the run of misses; the
// routes stay.
func TestARecordThatCameBackKeepsItsRoutes(t *testing.T) {
	long := time.Now().Add(-CustomDomainLapseGrace - time.Minute)
	cur := verifiedClaim(1) // FailingSince nil: the run of misses is over
	fs := &recheckFakeStore{due: []store.CustomDomainClaim{verifiedClaim(1)}, failingSince: long}
	fs.current = &cur
	rm := &fakeRemover{}
	NewCustomDomainVerifier(fs, &fakeResolver{}, rm).RunOnce(context.Background())
	if len(rm.removed) != 0 || len(fs.lapsed) != 0 {
		t.Fatalf("acted on a stale read: removed=%v lapsed=%v", rm.removed, fs.lapsed)
	}
}

// Proven before the token was kept: nothing to check against. It gets a token
// and the grace starts, rather than losing its routes or staying proven.
func TestAVerifiedClaimWithoutATokenGetsOneAndTheGrace(t *testing.T) {
	c := verifiedClaim(1)
	c.TXTToken = ""
	fs := &recheckFakeStore{due: []store.CustomDomainClaim{c}, failingSince: time.Now()}
	rm := &fakeRemover{}
	NewCustomDomainVerifier(fs, published(), rm).RunOnce(context.Background())
	if len(fs.tokens) != 1 || len(fs.misses) != 1 {
		t.Fatalf("tokens=%v misses=%v, want one of each", fs.tokens, fs.misses)
	}
	if len(fs.verified) != 0 || len(rm.removed) != 0 {
		t.Fatalf("verified=%v removed=%v", fs.verified, rm.removed)
	}
}

// A tenant whose node links the billing cutoff switched off has managed routes
// the remover cannot see: the Hub keeps their rows but drops them from Redis.
// Settling the claim then removed nothing, and the routes came back with the
// links under a claim nothing re-checks. Neither path settles until they are back.
func TestNothingIsSettledWhileTheRoutesAreHidden(t *testing.T) {
	long := time.Now().Add(-CustomDomainLapseGrace - time.Minute)
	t.Run("lapse", func(t *testing.T) {
		fs := &recheckFakeStore{due: []store.CustomDomainClaim{verifiedClaim(1)}, failingSince: long}
		fs.linksOff = true
		rm := &fakeRemover{}
		NewCustomDomainVerifier(fs, &fakeResolver{}, rm).RunOnce(context.Background())
		if len(rm.removed) != 0 || len(fs.lapsed) != 0 {
			t.Fatalf("settled while hidden: removed=%v lapsed=%v", rm.removed, fs.lapsed)
		}
	})
	t.Run("pending", func(t *testing.T) {
		fs := &verifierFakeStore{pending: []store.CustomDomainClaim{claim(1, "mc.example.com", time.Now().Add(-time.Minute))}, linksOff: true}
		rm := &fakeRemover{}
		NewCustomDomainVerifier(fs, &fakeResolver{}, rm).RunOnce(context.Background())
		if len(rm.removed) != 0 || len(fs.failed) != 0 {
			t.Fatalf("settled while hidden: removed=%v failed=%v", rm.removed, fs.failed)
		}
	})
}

// The grace has to give a failing claim many looks, and a daily re-check has to
// fit in it several times over, or one bad day of DNS takes the routes.
func TestTheLapseGraceOutlastsTheRecheck(t *testing.T) {
	if CustomDomainLapseGrace < 3*CustomDomainRecheckEvery {
		t.Fatalf("grace %v is under three re-check intervals of %v", CustomDomainLapseGrace, CustomDomainRecheckEvery)
	}
}
