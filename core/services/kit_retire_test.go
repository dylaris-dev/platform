package services

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"dylaris-core/store"
)

// retireFakeStore keeps kits and retirement records the way the SQL does:
// one owner's view, retiring kits out of the live count, records kept after
// the kit is revoked.
type retireFakeStore struct {
	store.Store
	maxLinks *int64
	kits     []*store.WarpAPIKey
	records  []retireRecord
	seq      int
	purges   int
}

type retireRecord struct {
	owner, link string
	at          time.Time
}

func (f *retireFakeStore) GetUserBilling(userID string) (*store.UserBilling, error) {
	return &store.UserBilling{UserID: userID, Status: "active", MaxLinks: f.maxLinks}, nil
}

func (f *retireFakeStore) live(owner string) (live, retiring int) {
	for _, k := range f.kits {
		if k.OwnerID != owner || k.RevokedAt != nil {
			continue
		}
		if k.RetireAt != nil {
			retiring++
		} else {
			live++
		}
	}
	return live, retiring
}

// mint is CreateLinkKitUnderCap.
func (f *retireFakeStore) mint(owner string) bool {
	if live, _ := f.live(owner); f.maxLinks != nil && int64(live) >= *f.maxLinks {
		return false
	}
	f.seq++
	f.kits = append(f.kits, &store.WarpAPIKey{ID: f.seq, NodeID: fmt.Sprintf("link-%d", f.seq), OwnerID: owner})
	return true
}

func (f *retireFakeStore) RetireLinkKit(owner, link string, now, retireAt, monthStart time.Time, allow func(int64, int64) error) (time.Time, error) {
	var kit *store.WarpAPIKey
	for _, k := range f.kits {
		if k.NodeID == link && k.OwnerID == owner && k.RevokedAt == nil {
			kit = k
		}
	}
	if kit == nil {
		return time.Time{}, sql.ErrNoRows
	}
	if kit.RetireAt != nil {
		return *kit.RetireAt, store.ErrLinkKitRetiring
	}
	_, retiring := f.live(owner)
	month := 0
	for _, r := range f.records {
		if r.owner == owner && !r.at.Before(monthStart) {
			month++
		}
	}
	if err := allow(int64(retiring), int64(month)); err != nil {
		return time.Time{}, err
	}
	at := retireAt
	kit.RetireAt = &at
	f.records = append(f.records, retireRecord{owner, link, now})
	return retireAt, nil
}

func (f *retireFakeStore) ListDueRetiringLinkKits(now time.Time) ([]store.WarpAPIKey, error) {
	var out []store.WarpAPIKey
	for _, k := range f.kits {
		if k.RetireAt != nil && !k.RetireAt.After(now) && k.RevokedAt == nil {
			out = append(out, store.WarpAPIKey{NodeID: k.NodeID, OwnerID: k.OwnerID})
		}
	}
	return out, nil
}

func (f *retireFakeStore) PurgeKitRetirements(before time.Time) error {
	f.purges++
	kept := f.records[:0]
	for _, r := range f.records {
		if !r.at.Before(before) {
			kept = append(kept, r)
		}
	}
	f.records = kept
	return nil
}

// revoke is the owner's revoke as the tick sees it.
func (f *retireFakeStore) revoke(_ context.Context, owner, link string) error {
	for _, k := range f.kits {
		if k.NodeID == link && k.OwnerID == owner && k.RevokedAt == nil {
			now := time.Now()
			k.RevokedAt = &now
		}
	}
	return nil
}

const retireOwner = "owner-1"

func i64p(n int64) *int64 { return &n }

func TestRetireLimitAndBudget(t *testing.T) {
	nov := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		maxLinks *int64
		retiring int // kits already retiring
		month    int // retirements already this month (beyond the retiring ones)
		lastMon  int // retirements last month
		at       time.Time
		want     error
	}{
		{"no host limit", nil, 0, 0, 0, nov, ErrRetireNotNeeded},
		{"a cap of no kits", i64p(0), 0, 0, 0, nov, ErrRetireNotNeeded},
		{"first retirement", i64p(1), 0, 0, 0, nov, nil},
		{"one host, one already retiring", i64p(1), 1, 0, 0, nov, ErrRetireLimit},
		{"three hosts, two retiring", i64p(3), 2, 0, 0, nov, nil},
		{"three hosts, three retiring", i64p(3), 3, 0, 0, nov, ErrRetireLimit},
		{"one host, fourth of the month", i64p(1), 0, 3, 0, nov, nil},
		{"one host, fifth of the month", i64p(1), 0, 4, 0, nov, ErrRetireBudget},
		{"three hosts, twelfth of the month", i64p(3), 0, 11, 0, nov, nil},
		{"three hosts, thirteenth of the month", i64p(3), 0, 12, 0, nov, ErrRetireBudget},
		{"a new month resets the budget", i64p(1), 0, 0, 4, nov, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &retireFakeStore{maxLinks: c.maxLinks}
			for i := 0; i < c.retiring; i++ {
				f.seq++
				at := c.at.Add(time.Hour)
				f.kits = append(f.kits, &store.WarpAPIKey{NodeID: fmt.Sprintf("link-r%d", i), OwnerID: retireOwner, RetireAt: &at})
				f.records = append(f.records, retireRecord{retireOwner, fmt.Sprintf("link-r%d", i), c.at})
			}
			for i := 0; i < c.month; i++ {
				f.records = append(f.records, retireRecord{retireOwner, fmt.Sprintf("link-m%d", i), c.at.Add(-time.Hour)})
			}
			for i := 0; i < c.lastMon; i++ {
				f.records = append(f.records, retireRecord{retireOwner, fmt.Sprintf("link-l%d", i), time.Date(2026, 10, 31, 23, 59, 0, 0, time.UTC)})
			}
			// Another tenant's retirements count for nobody else.
			f.records = append(f.records, retireRecord{"owner-2", "link-x", c.at}, retireRecord{"owner-2", "link-y", c.at},
				retireRecord{"owner-2", "link-z", c.at}, retireRecord{"owner-2", "link-w", c.at})
			f.kits = append(f.kits, &store.WarpAPIKey{NodeID: "link-target", OwnerID: retireOwner})

			got, err := RetireLinkKit(f, retireOwner, "link-target", c.at)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if c.want == nil && !got.Equal(c.at.Add(KitRetireWindow)) {
				t.Fatalf("retire_at = %v, want %v", got, c.at.Add(KitRetireWindow))
			}
		})
	}
}

// Review focus 2: minting and retiring in a loop as fast as the guards allow.
// At no instant may the owner hold more than 2 x maxLinks working kits, and
// the budget must stop the loop within the month.
func TestRetireMintLoopCannotExceedPlanForLong(t *testing.T) {
	for _, tc := range []struct {
		hosts int64
		// manualRevoke: the owner revokes every retiring kit at once and
		// retires again, so the retiring slots never stay full; only the
		// monthly budget can stop that loop.
		manualRevoke bool
	}{{1, false}, {3, false}, {1, true}, {3, true}} {
		hosts := tc.hosts
		t.Run(fmt.Sprintf("%d hosts, manual revoke %v", hosts, tc.manualRevoke), func(t *testing.T) {
			f := &retireFakeStore{maxLinks: i64p(hosts)}
			clock := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
			tick := newKitRetirement(f, f.revoke)
			tick.now = func() time.Time { return clock }
			retired, peak := 0, 0
			retireAll := func() {
				for _, k := range append([]*store.WarpAPIKey(nil), f.kits...) {
					if k.RevokedAt != nil || k.RetireAt != nil {
						continue
					}
					if _, err := RetireLinkKit(f, retireOwner, k.NodeID, clock); err == nil {
						retired++
					} else if !errors.Is(err, ErrRetireLimit) && !errors.Is(err, ErrRetireBudget) {
						t.Fatalf("retire: %v", err)
					}
				}
			}
			for step := 0; clock.Month() == time.November; step++ {
				for f.mint(retireOwner) {
				}
				// Retire every live kit the guards let through, then mint again.
				retireAll()
				for f.mint(retireOwner) {
				}
				if tc.manualRevoke {
					// The owner revokes the retiring kits at once, freeing the
					// retiring slots, and retires again.
					for _, k := range f.kits {
						if k.RevokedAt == nil && k.RetireAt != nil {
							f.revoke(context.Background(), retireOwner, k.NodeID)
						}
					}
					retireAll()
					for f.mint(retireOwner) {
					}
				}
				live, retiring := f.live(retireOwner)
				if live+retiring > peak {
					peak = live + retiring
				}
				if int64(live) > hosts || int64(retiring) > hosts {
					t.Fatalf("%v: %d live and %d retiring kits on a plan of %d", clock, live, retiring, hosts)
				}
				clock = clock.Add(10 * time.Minute)
				tick.tick(context.Background())
			}
			if int64(peak) != 2*hosts {
				t.Fatalf("peak = %d, want exactly 2 x %d (the loop did not run as hard as it could)", peak, hosts)
			}
			if want := int(KitRetireBudgetPerHost * hosts); retired != want {
				t.Fatalf("%d retirements in November, want the budget of %d", retired, want)
			}
			// Once the budget is spent and the last retirement has run out, the
			// owner is back to the plan.
			if live, retiring := f.live(retireOwner); int64(live) > hosts || retiring != 0 {
				t.Fatalf("at the end of the month: %d live, %d retiring", live, retiring)
			}
		})
	}
}

// Review focus 1: revoked at the deadline, not before, once.
func TestRetireTickRevokesAtDeadline(t *testing.T) {
	f := &retireFakeStore{maxLinks: i64p(1)}
	f.mint(retireOwner)
	t0 := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	deadline, err := RetireLinkKit(f, retireOwner, "link-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	failNext := 0
	revoke := func(ctx context.Context, owner, link string) error {
		calls = append(calls, owner+"/"+link)
		if failNext > 0 {
			failNext--
			return errors.New("redis down")
		}
		return f.revoke(ctx, owner, link)
	}
	clock := deadline.Add(-time.Second)
	k := newKitRetirement(f, revoke)
	k.now = func() time.Time { return clock }

	k.tick(context.Background())
	if len(calls) != 0 {
		t.Fatalf("revoked before the deadline: %v", calls)
	}
	clock = deadline
	failNext = 2
	k.tick(context.Background())
	k.tick(context.Background())
	if _, failing := k.failing["link-1"]; len(calls) != 2 || !failing {
		t.Fatalf("calls %v, failing %v: a failed revoke must be retried every tick and remembered", calls, k.failing)
	}
	k.tick(context.Background())
	if _, failing := k.failing["link-1"]; len(calls) != 3 || calls[2] != retireOwner+"/link-1" || failing {
		t.Fatalf("calls %v, failing %v: want the third tick to revoke and clear the failure", calls, k.failing)
	}
	if f.kits[0].RevokedAt == nil {
		t.Fatal("kit not revoked")
	}
	k.tick(context.Background())
	if len(calls) != 3 {
		t.Fatalf("revoked again: %v", calls)
	}
}

type flipLeader struct{ leader bool }

func (l *flipLeader) IsLeader() bool { return l.leader }

// Review focus 1: the deadline holds across a leader change and a restart.
// Core A retires the kit and loses the lease before the deadline; Core B, a
// fresh process with nothing in memory, becomes leader after it and revokes.
func TestRetireTickSurvivesLeaderChange(t *testing.T) {
	f := &retireFakeStore{maxLinks: i64p(2)}
	f.mint(retireOwner)
	t0 := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	deadline, err := RetireLinkKit(f, retireOwner, "link-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	revoked := 0
	revoke := func(ctx context.Context, owner, link string) error {
		revoked++
		return f.revoke(ctx, owner, link)
	}
	clock := t0.Add(time.Hour)
	now := func() time.Time { return clock }

	a, leaderA := newKitRetirement(f, revoke), &flipLeader{leader: true}
	a.now = now
	a.step(context.Background(), leaderA)
	leaderA.leader = false

	clock = deadline.Add(30 * time.Minute)
	a.step(context.Background(), leaderA) // a follower does nothing
	b, leaderB := newKitRetirement(f, revoke), &flipLeader{}
	b.now = now
	b.step(context.Background(), leaderB)
	if revoked != 0 {
		t.Fatalf("revoked %d times with no leader", revoked)
	}
	leaderB.leader = true
	b.step(context.Background(), leaderB)
	if revoked != 1 || f.kits[0].RevokedAt == nil {
		t.Fatalf("revoked %d times after B became leader, want 1", revoked)
	}
}

func TestRetirePurgesOldRecords(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	f := &retireFakeStore{records: []retireRecord{
		{retireOwner, "link-old", now.Add(-91 * 24 * time.Hour)},
		{retireOwner, "link-new", now.Add(-89 * 24 * time.Hour)},
	}}
	k := newKitRetirement(f, f.revoke)
	k.now = func() time.Time { return now }
	k.tick(context.Background())
	if len(f.records) != 1 || f.records[0].link != "link-new" {
		t.Fatalf("records = %+v, want only the one inside 90 days", f.records)
	}
}

// A kit that keeps failing is logged once an hour, and the set of failing kits
// is rebuilt from each tick's due list, so a kit that left it (revoked by its
// owner meanwhile) is forgotten.
func TestRetireTickFailingKitsLogHourly(t *testing.T) {
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	f := &retireFakeStore{maxLinks: i64p(2)}
	f.mint(retireOwner)
	f.mint(retireOwner)
	t0 := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for _, link := range []string{"link-1", "link-2"} {
		if _, err := RetireLinkKit(f, retireOwner, link, t0); err != nil {
			t.Fatal(err)
		}
	}
	clock := t0.Add(KitRetireWindow)
	k := newKitRetirement(f, func(context.Context, string, string) error { return errors.New("redis down") })
	k.now = func() time.Time { return clock }
	failLogs := func() int { return strings.Count(logged.String(), "revoke link-1 failed") }

	k.tick(context.Background())
	if failLogs() != 1 || len(k.failing) != 2 {
		t.Fatalf("first failure: %d log lines, failing %v", failLogs(), k.failing)
	}
	for i := 0; i < 59; i++ {
		clock = clock.Add(time.Minute)
		k.tick(context.Background())
	}
	if failLogs() != 1 {
		t.Fatalf("logged %d times within the hour, want 1", failLogs())
	}
	clock = clock.Add(time.Minute)
	k.tick(context.Background())
	if failLogs() != 2 {
		t.Fatalf("logged %d times after an hour, want 2", failLogs())
	}

	// The owner revokes link-2 by hand: it leaves the due list and the set.
	f.revoke(context.Background(), retireOwner, "link-2")
	k.tick(context.Background())
	if _, still := k.failing["link-2"]; still || len(k.failing) != 1 {
		t.Fatalf("failing = %v, want only link-1", k.failing)
	}
}

// The record purge runs on the first tick and then at most once an hour.
func TestRetirePurgeRunsHourly(t *testing.T) {
	f := &retireFakeStore{}
	clock := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	k := newKitRetirement(f, f.revoke)
	k.now = func() time.Time { return clock }
	for i := 0; i < 60; i++ {
		k.tick(context.Background())
		clock = clock.Add(time.Minute)
	}
	if f.purges != 1 {
		t.Fatalf("purges in the first hour = %d, want 1", f.purges)
	}
	k.tick(context.Background())
	if f.purges != 2 {
		t.Fatalf("purges after an hour = %d, want 2", f.purges)
	}
}
