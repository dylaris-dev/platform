package services

import (
	"context"
	"errors"
	"log"
	"time"

	"dylaris-core/pkg/leader"
	"dylaris-core/store"
)

// Link kit retirement: how a customer at their host limit replaces a node
// without dropping its players. The old kit is retired instead of revoked: it
// stops counting toward MaxLinks at once (so the new node's kit fits under the
// cap), keeps working for KitRetireWindow while players drain off it, and is
// then revoked by the minute tick below through the same teardown as the
// owner's own revoke.
//
// Two guards keep it from being a way to hold more links than were bought:
// at most MaxLinks kits retiring at one time (so at any instant an owner has
// at most 2 x MaxLinks kits alive), and at most KitRetireBudgetPerHost x
// MaxLinks retirements per UTC calendar month.

// KitRetireWindow is how long a retired kit keeps working: the managed link's
// 6 h drain plus margin.
const KitRetireWindow = 8 * time.Hour

// KitRetireBudgetPerHost is the monthly retirements allowed per host of the plan.
const KitRetireBudgetPerHost = 4

// kitRetireRelog is how often a kit whose revoke keeps failing is logged again.
// kitRetirePurgeEvery spaces the record purge: records age in days, not minutes.
const (
	kitRetireRelog      = time.Hour
	kitRetirePurgeEvery = time.Hour
)

// kitRetirementKeep is how long a retirement record outlives its month. The
// budget only ever looks at the running month; 90 days leaves room for support.
const kitRetirementKeep = 90 * 24 * time.Hour

var (
	// ErrRetireNotNeeded: the owner has no host limit to make room under (no
	// cap at all, or a cap of zero kits).
	ErrRetireNotNeeded = errors.New("retire not needed")
	// ErrRetireLimit: as many kits are retiring as the plan has hosts.
	ErrRetireLimit = errors.New("retire limit reached")
	// ErrRetireBudget: the month's retirements are used up.
	ErrRetireBudget = errors.New("retire budget used up")
)

// RetireLinkKit retires one of the owner's link kits at now and returns its
// revoke time. Besides the errors above it passes on the store's: sql.ErrNoRows
// for a kit the owner does not hold, store.ErrLinkKitRetiring (with the
// existing deadline) for one already retiring.
func RetireLinkKit(st store.Store, ownerID, linkID string, now time.Time) (time.Time, error) {
	lim, err := EffectiveLimits(st, ownerID)
	if err != nil {
		return time.Time{}, err
	}
	hosts := lim.MaxLinks
	if hosts == nil || *hosts == 0 {
		return time.Time{}, ErrRetireNotNeeded
	}
	budget := *hosts * KitRetireBudgetPerHost
	now = now.UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return st.RetireLinkKit(ownerID, linkID, now, now.Add(KitRetireWindow), monthStart, func(retiring, thisMonth int64) error {
		if AtOrOver(hosts, retiring) {
			return ErrRetireLimit
		}
		if AtOrOver(&budget, thisMonth) {
			return ErrRetireBudget
		}
		return nil
	})
}

// KitRevokeFunc revokes one link kit end to end: the owner's own kit revoke.
type KitRevokeFunc func(ctx context.Context, ownerID, linkID string) error

// kitRetirement is the revoke-at-deadline tick. Its state is all in the
// database, so a Core that becomes leader mid-window, or one that restarted,
// picks up exactly where the last leader stopped.
type kitRetirement struct {
	st     store.Store
	revoke KitRevokeFunc
	now    func() time.Time
	// failing maps each kit whose revoke failed to when that was last logged,
	// so a kit that keeps failing is logged once an hour rather than every
	// minute. Rebuilt from every tick's due list: a kit no longer due drops out.
	failing map[string]time.Time
	// nextPurge: the record purge runs at most once per kitRetirePurgeEvery.
	nextPurge time.Time
}

func newKitRetirement(st store.Store, revoke KitRevokeFunc) *kitRetirement {
	return &kitRetirement{st: st, revoke: revoke, now: time.Now, failing: map[string]time.Time{}}
}

// StartKitRetirement runs the tick every minute on the leader, and once at
// start so a deadline that passed while no Core was leader is not a minute late.
func StartKitRetirement(ctx context.Context, st store.Store, revoke KitRevokeFunc, el leader.Election) {
	k := newKitRetirement(st, revoke)
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			k.step(ctx, el)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

func (k *kitRetirement) step(ctx context.Context, el leader.Election) {
	if el != nil && !el.IsLeader() {
		return
	}
	k.tick(ctx)
}

// tick revokes every kit whose deadline has passed and purges old records.
// Idempotent: a revoked kit leaves the due list, and one whose revoke failed
// stays on it and is retried next minute.
func (k *kitRetirement) tick(ctx context.Context) {
	now := k.now()
	due, err := k.st.ListDueRetiringLinkKits(now)
	if err != nil {
		log.Printf("kit retirement: list due kits: %v", err)
		return
	}
	failing := make(map[string]time.Time, len(k.failing))
	for _, kit := range due {
		if ctx.Err() != nil {
			return // shutting down: the old set is as good as a half-built one
		}
		if err := k.revoke(ctx, kit.OwnerID, kit.NodeID); err != nil {
			at, logged := k.failing[kit.NodeID]
			if !logged || now.Sub(at) >= kitRetireRelog {
				at = now
				log.Printf("kit retirement: revoke %s failed, retrying every minute: %v", kit.NodeID, err)
			}
			failing[kit.NodeID] = at
			continue
		}
		log.Printf("kit retirement: revoked retired link kit %s (owner %s)", kit.NodeID, kit.OwnerID)
	}
	k.failing = failing
	if now.Before(k.nextPurge) {
		return
	}
	k.nextPurge = now.Add(kitRetirePurgeEvery)
	if err := k.st.PurgeKitRetirements(now.Add(-kitRetirementKeep)); err != nil {
		log.Printf("kit retirement: purge old records: %v", err)
	}
}
