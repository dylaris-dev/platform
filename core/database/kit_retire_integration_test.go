package database

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"dylaris-core/store"
)

// Link kit retirement against real Postgres. Skipped without
// DYLARIS_TEST_DB_HOST, like its neighbours.

func TestIntegrationKitRetireSchema(t *testing.T) {
	db, _ := integrationDB(t)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'warp_api_keys' AND column_name = 'retire_at' AND is_nullable = 'YES'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("warp_api_keys.retire_at: count %d, err %v", n, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name = 'warp_kit_retirements'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("warp_kit_retirements: count %d, err %v", n, err)
	}
	for _, idx := range []string{"idx_warp_api_keys_retire_at", "idx_warp_kit_retirements_owner"} {
		if err := db.QueryRow(`SELECT COUNT(*) FROM pg_indexes WHERE schemaname = current_schema() AND indexname = $1`, idx).Scan(&n); err != nil || n != 1 {
			t.Fatalf("index %s: count %d, err %v", idx, n, err)
		}
	}
	if err := applyKitRetire(db); err != nil {
		t.Fatalf("second run: %v", err)
	}
}

func retireKit(t *testing.T, st *store.PostgresStore, owner string) string {
	t.Helper()
	id := uniqueName("link-")
	if _, err := st.CreateWarpAPIKey(store.WarpAPIKey{Name: "kit", KeyHash: uniqueName("h_"), Policy: "general", MaxConns: 1,
		OnNewConn: "kill_old", NodeID: id, OwnerID: owner}); err != nil {
		t.Fatalf("CreateWarpAPIKey: %v", err)
	}
	return id
}

func allowAll(int64, int64) error { return nil }

// The slot is freed in the same statement set the kit is marked in, the record
// is written beside it, and everything the kit must no longer do is refused.
func TestIntegrationKitRetireFreesTheSlot(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	other := newFixture(t, st)
	kit := retireKit(t, st, f.user.ID)
	now := time.Now().UTC().Truncate(time.Second)
	deadline := now.Add(8 * time.Hour)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	if ok, err := st.CreateLinkKitUnderCap(store.WarpAPIKey{Name: "x", KeyHash: uniqueName("h_"), Policy: "general", MaxConns: 1,
		OnNewConn: "kill_old", NodeID: uniqueName("link-"), OwnerID: f.user.ID}, 1); err != nil || ok {
		t.Fatalf("premise: mint at the cap = (%v, %v), want refused", ok, err)
	}
	if _, err := st.RetireLinkKit(other.user.ID, kit, now, deadline, month, allowAll); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("another owner's retire = %v, want sql.ErrNoRows", err)
	}
	got, err := st.RetireLinkKit(f.user.ID, kit, now, deadline, month, allowAll)
	if err != nil || !got.Equal(deadline) {
		t.Fatalf("RetireLinkKit = (%v, %v), want %v", got, err, deadline)
	}
	if n, err := st.CountLinkKitsByOwner(f.user.ID); err != nil || n != 0 {
		t.Fatalf("live kits after retire = (%d, %v), want 0", n, err)
	}
	if ok, err := st.CreateLinkKitUnderCap(store.WarpAPIKey{Name: "new", KeyHash: uniqueName("h_"), Policy: "general", MaxConns: 1,
		OnNewConn: "kill_old", NodeID: uniqueName("link-"), OwnerID: f.user.ID}, 1); err != nil || !ok {
		t.Fatalf("mint after retire = (%v, %v), want created", ok, err)
	}
	again, err := st.RetireLinkKit(f.user.ID, kit, now.Add(time.Minute), deadline.Add(time.Minute), month, allowAll)
	if !errors.Is(err, store.ErrLinkKitRetiring) || !again.Equal(deadline) {
		t.Fatalf("second retire = (%v, %v), want ErrLinkKitRetiring with the first deadline", again, err)
	}
	if err := st.RollWarpAPIKeyHash(kit, uniqueName("h_")); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("roll of a retiring kit = %v, want sql.ErrNoRows", err)
	}
	k, err := st.GetWarpAPIKeyByNodeID(kit)
	if err != nil || k.RetireAt == nil || !k.RetireAt.Equal(deadline) {
		t.Fatalf("kit row = %+v, %v; want retire_at %v", k, err, deadline)
	}

	due := func(at time.Time) bool {
		t.Helper()
		kits, err := st.ListDueRetiringLinkKits(at)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range kits {
			if d.NodeID == kit {
				if d.OwnerID != f.user.ID {
					t.Fatalf("due kit owner = %q, want %q", d.OwnerID, f.user.ID)
				}
				return true
			}
		}
		return false
	}
	if due(deadline.Add(-time.Second)) {
		t.Fatal("kit is due before its deadline")
	}
	if !due(deadline) {
		t.Fatal("kit is not due at its deadline")
	}
	if err := st.RevokeWarpAPIKeyByNodeID(kit); err != nil {
		t.Fatal(err)
	}
	if due(deadline) {
		t.Fatal("a revoked kit is still due")
	}

	// The record outlives the kit: the budget counts it.
	var thisMonth int64
	if _, err := st.RetireLinkKit(f.user.ID, retireKit(t, st, f.user.ID), now, deadline, month, func(_, m int64) error {
		thisMonth = m
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if thisMonth != 1 {
		t.Fatalf("retirements this month seen by the second retire = %d, want 1", thisMonth)
	}
}

// Every retire of one kit at once: exactly one wins, and the kit and the
// record agree.
func TestIntegrationKitRetireRace(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)
	kit := retireKit(t, st, f.user.ID)
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	const racers = 8
	var wg sync.WaitGroup
	won := make(chan bool, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.RetireLinkKit(f.user.ID, kit, now, now.Add(8*time.Hour), month, allowAll)
			if err != nil && !errors.Is(err, store.ErrLinkKitRetiring) && !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("RetireLinkKit: %v", err)
			}
			won <- err == nil
		}()
	}
	// A revoke racing them: whichever order, the kit ends revoked.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := st.RevokeWarpAPIKeyByNodeID(kit); err != nil {
			t.Errorf("revoke: %v", err)
		}
	}()
	wg.Wait()
	close(won)
	wins := 0
	for w := range won {
		if w {
			wins++
		}
	}
	var marked, records int
	if err := db.QueryRow(`SELECT COUNT(*) FROM warp_api_keys WHERE node_id = $1 AND retire_at IS NOT NULL`, kit).Scan(&marked); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM warp_kit_retirements WHERE link_id = $1`, kit).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if wins > 1 || marked != wins || records != wins {
		t.Fatalf("wins %d, kit marked %d, records %d: want at most one, and all three equal", wins, marked, records)
	}
	if k, err := st.GetWarpAPIKeyByNodeID(kit); err != nil || k.RevokedAt == nil {
		t.Fatalf("kit = %+v, %v; want revoked", k, err)
	}
}

// A retire and a mint of the same owner, racing for the last slot under a cap
// of one: the retire frees it, so exactly one mint fits after it however they
// interleave, and the owner never holds more than one live kit.
func TestIntegrationKitRetireAndMintSerialise(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	kit := retireKit(t, st, f.user.ID)
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	minted := make(chan bool, 4)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := st.RetireLinkKit(f.user.ID, kit, now, now.Add(8*time.Hour), month, allowAll); err != nil {
			t.Errorf("retire: %v", err)
		}
	}()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := st.CreateLinkKitUnderCap(store.WarpAPIKey{Name: "m", KeyHash: uniqueName("h_"), Policy: "general", MaxConns: 1,
				OnNewConn: "kill_old", NodeID: uniqueName("link-"), OwnerID: f.user.ID}, 1)
			if err != nil {
				t.Errorf("mint: %v", err)
			}
			minted <- ok
		}()
	}
	wg.Wait()
	close(minted)
	n := 0
	for ok := range minted {
		if ok {
			n++
		}
	}
	if n > 1 {
		t.Fatalf("%d mints succeeded under a cap of 1", n)
	}
	if live, err := st.CountLinkKitsByOwner(f.user.ID); err != nil || live != n {
		t.Fatalf("live kits = (%d, %v), want %d", live, err, n)
	}
}

func TestIntegrationKitRetirementsPurge(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)
	kit := retireKit(t, st, f.user.ID)
	old := time.Now().UTC().Add(-100 * 24 * time.Hour)
	if _, err := st.RetireLinkKit(f.user.ID, kit, old, old.Add(8*time.Hour), old, allowAll); err != nil {
		t.Fatal(err)
	}
	if err := st.PurgeKitRetirements(time.Now().UTC().Add(-90 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM warp_kit_retirements WHERE link_id = $1`, kit).Scan(&n); err != nil || n != 0 {
		t.Fatalf("records left = %d (%v), want 0", n, err)
	}
}
