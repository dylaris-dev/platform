package services

import (
	"context"
	"errors"
	"log"
	"time"

	"dylaris-core/pkg/leader"
	"dylaris-core/store"
)

// Verification cadence, from the pinned spec: a tenant gets four hours to
// publish their TXT record, and we look every thirty minutes.
//
// The poll interval is well inside the grant, so a customer who sets the record
// correctly is verified long before the deadline and never sees it. The deadline
// is what stops an unproven claim from sitting on a domain forever.
const (
	CustomDomainGrant       = 4 * time.Hour
	CustomDomainPollEvery   = 30 * time.Minute
	customDomainProofTimout = 10 * time.Second
)

// A verified domain is re-checked once a day, and loses its routes once its
// record has been missing for a week (owner decision, 2026-10-04). The week is
// for a customer who removed the record by mistake and only sees it in the
// panel; it is also how long a domain that changed hands still reaches the old
// tenant.
const (
	CustomDomainRecheckEvery = 24 * time.Hour
	CustomDomainLapseGrace   = 7 * 24 * time.Hour
	// ponytail: fixed batch per pass; at 10s a lookup worst case, 100 claims
	// fit well inside a 30-minute pass. Raise it with the claim count.
	customDomainRecheckBatch = 100
)

// RouteRemover deletes the routes a user holds on a domain they failed to prove.
// Implemented by the gateway route service; an interface so the verifier does
// not drag the whole handler package in.
type RouteRemover interface {
	DeleteRoutesForDomain(ctx context.Context, userID, domain string) error
}

// CustomDomainVerifier proves (or fails) pending custom-domain claims.
type CustomDomainVerifier struct {
	store    store.Store
	resolver DomainResolver
	routes   RouteRemover

	// leader gates each pass, like every other Core singleton. Nil means
	// ungated, which is what the tests want.
	leader leader.Election
}

// SetLeader gates the poll loop so only one Core acts on a claim.
//
// The gate used to be a one-shot `if coreLeader.IsLeader()` around Start in
// main.go, and leadership is not a boot-time fact. Two ways that lost the
// verifier entirely: the lease is acquired by a goroutine started ~200 lines
// earlier, so a follower - or, if Redis was slow, BOTH replicas - simply never
// started it; and after any failover the new leader had not started it at ITS
// boot either. Nothing logged, nothing errored. Claims just stopped being
// looked at, sitting pending until someone noticed their domain never verified.
func (v *CustomDomainVerifier) SetLeader(l leader.Election) { v.leader = l }

func (v *CustomDomainVerifier) shouldRun() bool {
	return v.leader == nil || v.leader.IsLeader()
}

// NewCustomDomainVerifier wires the verifier.
func NewCustomDomainVerifier(st store.Store, res DomainResolver, routes RouteRemover) *CustomDomainVerifier {
	return &CustomDomainVerifier{store: st, resolver: res, routes: routes}
}

// Start runs the poll loop until ctx is cancelled.
func (v *CustomDomainVerifier) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(CustomDomainPollEvery)
		defer t.Stop()
		// One pass immediately: after a Core restart a claim could otherwise sit
		// unproven for a whole interval even though its DNS has been right for
		// hours. Skipped on a follower, and on a leader that has not acquired
		// its lease yet - the next tick picks it up either way.
		if v.shouldRun() {
			v.RunOnce(ctx)
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if !v.shouldRun() {
					continue
				}
				v.RunOnce(ctx)
			}
		}
	}()
}

// RunOnce proves what can be proven, fails what has run out of time, then
// re-checks the claims that are already proven.
func (v *CustomDomainVerifier) RunOnce(ctx context.Context) {
	v.checkPending(ctx)
	v.recheckVerified(ctx)
}

// checkPending proves what can be proven, then fails what has run out of time.
//
// Order matters: a claim whose DNS went live minutes before its deadline must be
// verified rather than punished for the timing of this tick.
func (v *CustomDomainVerifier) checkPending(ctx context.Context) {
	pending, err := v.store.ListPendingClaims()
	if err != nil {
		log.Printf("custom-domain verifier: list pending: %v", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	// Each claim is judged against the moment ITS record was looked at, not the
	// end of the pass. A pass is sequential at up to customDomainProofTimout a
	// claim, and slow nameservers - a tenant can run their own - stretched it
	// for hours: a claim checked early, before its record had propagated, was
	// then failed against a clock read long after its deadline.
	type checked struct {
		claim store.CustomDomainClaim
		at    time.Time
	}
	stillPending := make([]checked, 0, len(pending))
	for _, c := range pending {
		if c.TXTToken == "" {
			v.issueToken(c)
			continue
		}
		at := time.Now()
		pctx, cancel := context.WithTimeout(ctx, customDomainProofTimout)
		ok := CheckTXTToken(pctx, v.resolver, c.Domain, c.TXTToken)
		cancel()
		if ok {
			if err := v.store.MarkCustomDomainVerified(c.ID); err != nil {
				log.Printf("custom-domain verifier: mark %s verified: %v", c.Domain, err)
				continue
			}
			log.Printf("custom-domain verifier: %s proven for user %s", c.Domain, c.UserID)
			continue
		}
		stillPending = append(stillPending, checked{claim: c, at: at})
	}

	for _, p := range stillPending {
		c := p.claim
		if c.DeadlineAt == nil || p.at.Before(*c.DeadlineAt) {
			continue // still inside the grant
		}
		// Read again before anything is removed: the list is from the start of
		// the pass, and the customer may have verified the claim or re-armed it
		// since. Removing routes on that stale read deleted a verified domain's.
		cur, gerr := v.store.GetCustomDomainClaim(c.UserID, c.Domain)
		if gerr != nil || cur.State != store.ClaimPending || cur.DeadlineAt == nil || p.at.Before(*cur.DeadlineAt) {
			continue
		}
		if v.routesHidden(c.UserID) {
			continue
		}
		// The route goes first. Leaving it up while the claim is blocked would
		// keep serving a domain nobody has shown they own - and a failed claim
		// is never looked at again, so a removal that did not happen leaves the
		// claim pending for the next pass instead.
		if v.routes != nil {
			if derr := v.routes.DeleteRoutesForDomain(ctx, c.UserID, c.Domain); derr != nil {
				log.Printf("custom-domain verifier: remove routes for %s: %v (retrying next pass)", c.Domain, derr)
				continue
			}
		}
		state, ferr := v.store.FailCustomDomainClaim(c.ID)
		if errors.Is(ferr, store.ErrNoClaim) {
			continue // verified or re-armed in the meantime
		}
		if ferr != nil {
			logErrf("custom-domain-verifier", "fail %s: %v", c.Domain, ferr)
			continue
		}
		log.Printf("custom-domain verifier: %s not proven in time for user %s -> %s",
			c.Domain, c.UserID, state)
	}
}

// recheckVerified looks again at proven claims. A proof used to be forever:
// the token was cleared on verification and nothing read a verified claim
// again, so after a domain expired or was sold the old tenant's route kept the
// new owner's name - their players landed on the old tenant's server, and the
// new owner could not route it at all, because the name was taken.
//
// A miss does not take anything at once. It starts a run of misses the panel
// shows; once that run is CustomDomainLapseGrace old the routes go, and the
// claim is blocked without a strike.
func (v *CustomDomainVerifier) recheckVerified(ctx context.Context) {
	due, err := v.store.ListClaimsDueRecheck(CustomDomainRecheckEvery, customDomainRecheckBatch)
	if err != nil {
		log.Printf("custom-domain verifier: list claims to re-check: %v", err)
		return
	}
	for _, c := range due {
		if ctx.Err() != nil {
			return
		}
		at := time.Now()
		if c.TXTToken == "" {
			// Proven before the token was kept. There is nothing to check it
			// against, so it gets a token and the grace starts: the panel shows
			// the record to publish, and the week is the time to do it.
			token, terr := NewTXTToken()
			if terr == nil {
				terr = v.store.SetCustomDomainTXTToken(c.ID, token)
			}
			if terr != nil {
				log.Printf("custom-domain verifier: issue a token for verified %s: %v", c.Domain, terr)
				continue
			}
		} else {
			pctx, cancel := context.WithTimeout(ctx, customDomainProofTimout)
			ok := CheckTXTToken(pctx, v.resolver, c.Domain, c.TXTToken)
			cancel()
			if ok {
				if err := v.store.MarkCustomDomainVerified(c.ID); err != nil {
					log.Printf("custom-domain verifier: re-check of %s: %v", c.Domain, err)
				}
				continue
			}
		}
		since, ferr := v.store.RecheckFailedCustomDomainClaim(c.ID)
		if errors.Is(ferr, store.ErrNoClaim) {
			continue
		}
		if ferr != nil {
			logErrf("custom-domain-verifier", "record re-check miss for %s: %v", c.Domain, ferr)
			continue
		}
		if c.FailingSince == nil {
			log.Printf("custom-domain verifier: %s lost its TXT record for user %s; routes go in %v unless it returns",
				c.Domain, c.UserID, CustomDomainLapseGrace)
		}
		if at.Sub(since) < CustomDomainLapseGrace {
			continue
		}
		// Read again before anything is removed, like the pending path: a
		// "check now" may have ended the run of misses since the list was read.
		cur, gerr := v.store.GetCustomDomainClaim(c.UserID, c.Domain)
		if gerr != nil || cur.State != store.ClaimVerified || cur.FailingSince == nil ||
			at.Sub(*cur.FailingSince) < CustomDomainLapseGrace {
			continue
		}
		if v.routesHidden(c.UserID) {
			continue
		}
		if v.routes != nil {
			if derr := v.routes.DeleteRoutesForDomain(ctx, c.UserID, c.Domain); derr != nil {
				log.Printf("custom-domain verifier: remove routes for lapsed %s: %v (retrying next pass)", c.Domain, derr)
				continue
			}
		}
		if lerr := v.store.LapseCustomDomainClaim(c.ID, CustomDomainLapseGrace); lerr != nil {
			if !errors.Is(lerr, store.ErrNoClaim) {
				logErrf("custom-domain-verifier", "lapse %s: %v", c.Domain, lerr)
			}
			continue
		}
		log.Printf("custom-domain verifier: %s has had no TXT record for %v; routes removed for user %s",
			c.Domain, CustomDomainLapseGrace, c.UserID)
	}
}

// routesHidden reports whether the tenant's managed routes are out of sight of
// the remover right now. The billing cutoff switches their node links off in
// the Hub, and the Hub keeps the route rows of a switched-off link but drops
// them from Redis - the only place the remover looks. Settling a claim then
// removed nothing and recorded it as done; the routes came back with the links,
// under a claim nothing looks at again. So the claim waits, past its deadline,
// until the links are back and the routes can be removed.
//
// A link an operator switched off in the Hub hides its routes the same way and
// is not covered: Core does not know about it.
func (v *CustomDomainVerifier) routesHidden(userID string) bool {
	b, err := v.store.GetUserBilling(userID)
	if err != nil {
		log.Printf("custom-domain verifier: billing of %s: %v (deciding next pass)", userID, err)
		return true
	}
	return b.NodeLinksOff
}

// issueToken gives a claim from before the TXT proof its token and a fresh
// grant. Its customer was told to set a CNAME, which no longer proves anything,
// and failing them on the old deadline would take their route for following the
// instructions they had.
func (v *CustomDomainVerifier) issueToken(c store.CustomDomainClaim) {
	token, err := NewTXTToken()
	if err == nil {
		err = v.store.SetCustomDomainTXTToken(c.ID, token)
	}
	if err == nil {
		_, err = v.store.StartCustomDomainClaim(c.UserID, c.Domain, time.Now().Add(CustomDomainGrant))
	}
	if err != nil {
		log.Printf("custom-domain verifier: issue a token for %s: %v", c.Domain, err)
		return
	}
	log.Printf("custom-domain verifier: %s had no token; issued one with a fresh grant", c.Domain)
}
