package database

import (
	"errors"
	"testing"
	"time"

	"dylaris-core/store"
)

// The verifier fails a claim on a list read at the start of its pass. In
// between, the customer may verify it or re-arm it, and the failure must then
// land on nothing: it used to overwrite a verified claim with a block.
//
// Against a real Postgres, because the guard IS the query.
// Skipped without DYLARIS_TEST_DB_HOST, like its neighbours.
func TestIntegrationOnlyAPendingOverdueClaimCanFail(t *testing.T) {
	_, st := integrationDB(t)
	user := "claim-test-" + time.Now().Format("150405.000000000")

	t.Run("pending and overdue fails", func(t *testing.T) {
		c, err := st.StartCustomDomainClaim(user, "overdue.example.test", time.Now().Add(-time.Minute))
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		state, err := st.FailCustomDomainClaim(c.ID)
		if err != nil || state != store.ClaimBlocked {
			t.Fatalf("state %q, err %v; want blocked", state, err)
		}
	})

	t.Run("verified meanwhile is left alone", func(t *testing.T) {
		c, err := st.StartCustomDomainClaim(user, "verified.example.test", time.Now().Add(-time.Minute))
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		if err := st.MarkCustomDomainVerified(c.ID); err != nil {
			t.Fatalf("verify: %v", err)
		}
		if _, err := st.FailCustomDomainClaim(c.ID); !errors.Is(err, store.ErrNoClaim) {
			t.Fatalf("err %v, want ErrNoClaim", err)
		}
		got, _ := st.GetCustomDomainClaim(user, "verified.example.test")
		if got == nil || got.State != store.ClaimVerified {
			t.Fatalf("claim is %+v, want still verified", got)
		}
	})

	t.Run("re-armed meanwhile is left alone", func(t *testing.T) {
		c, err := st.StartCustomDomainClaim(user, "rearmed.example.test", time.Now().Add(-time.Minute))
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		if _, err := st.StartCustomDomainClaim(user, "rearmed.example.test", time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("re-arm: %v", err)
		}
		if _, err := st.FailCustomDomainClaim(c.ID); !errors.Is(err, store.ErrNoClaim) {
			t.Fatalf("err %v, want ErrNoClaim", err)
		}
	})
}

// A verified claim is re-checked, and the guards that decide when it loses its
// routes are SQL. Against a real Postgres for the same reason as above.
func TestIntegrationAVerifiedClaimIsRecheckedAndLapses(t *testing.T) {
	db, st := integrationDB(t)
	user := "recheck-test-" + time.Now().Format("150405.000000000")
	verified := func(domain string) *store.CustomDomainClaim {
		t.Helper()
		c, err := st.StartCustomDomainClaim(user, domain, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		if err := st.SetCustomDomainTXTToken(c.ID, "dylaris-verify=abc"); err != nil {
			t.Fatalf("token: %v", err)
		}
		if err := st.MarkCustomDomainVerified(c.ID); err != nil {
			t.Fatalf("verify: %v", err)
		}
		got, _ := st.GetCustomDomainClaim(user, domain)
		return got
	}
	due := func() map[int]bool {
		t.Helper()
		list, err := st.ListClaimsDueRecheck(24*time.Hour, 1000)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		out := map[int]bool{}
		for _, c := range list {
			out[c.ID] = true
		}
		return out
	}

	t.Run("verification keeps the token the re-check needs", func(t *testing.T) {
		c := verified("keep.example.test")
		if c.TXTToken != "dylaris-verify=abc" || c.CheckedAt == nil || c.FailingSince != nil {
			t.Fatalf("claim %+v", c)
		}
	})

	t.Run("due once a day, every pass while failing", func(t *testing.T) {
		c := verified("due.example.test")
		if due()[c.ID] {
			t.Fatal("a claim checked just now is due again")
		}
		if _, err := db.Exec(`UPDATE custom_domain_claims SET checked_at = NOW() - interval '25 hours' WHERE id = $1`, c.ID); err != nil {
			t.Fatal(err)
		}
		if !due()[c.ID] {
			t.Fatal("a claim checked 25h ago is not due")
		}
		if _, err := st.RecheckFailedCustomDomainClaim(c.ID); err != nil {
			t.Fatal(err)
		}
		if !due()[c.ID] {
			t.Fatal("a failing claim is not looked at again on the next pass")
		}
	})

	t.Run("the run of misses keeps its first date", func(t *testing.T) {
		c := verified("since.example.test")
		first, err := st.RecheckFailedCustomDomainClaim(c.ID)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		second, err := st.RecheckFailedCustomDomainClaim(c.ID)
		if err != nil || !second.Equal(first) {
			t.Fatalf("second miss moved the start: %v -> %v (%v)", first, second, err)
		}
		// A hit ends the run.
		if err := st.MarkCustomDomainVerified(c.ID); err != nil {
			t.Fatal(err)
		}
		got, _ := st.GetCustomDomainClaim(user, "since.example.test")
		if got.FailingSince != nil {
			t.Fatalf("a hit left the run of misses: %v", got.FailingSince)
		}
	})

	t.Run("lapses only after the grace, without a strike", func(t *testing.T) {
		c := verified("lapse.example.test")
		if _, err := st.RecheckFailedCustomDomainClaim(c.ID); err != nil {
			t.Fatal(err)
		}
		if err := st.LapseCustomDomainClaim(c.ID, time.Hour); !errors.Is(err, store.ErrNoClaim) {
			t.Fatalf("lapsed inside the grace: %v", err)
		}
		if _, err := db.Exec(`UPDATE custom_domain_claims SET failing_since = NOW() - interval '2 hours' WHERE id = $1`, c.ID); err != nil {
			t.Fatal(err)
		}
		if err := st.LapseCustomDomainClaim(c.ID, time.Hour); err != nil {
			t.Fatalf("lapse: %v", err)
		}
		got, _ := st.GetCustomDomainClaim(user, "lapse.example.test")
		if got.State != store.ClaimBlocked || got.Attempts != 0 {
			t.Fatalf("claim %+v, want blocked with no strike", got)
		}
		// A miss on a claim that is no longer verified records nothing.
		if _, err := st.RecheckFailedCustomDomainClaim(c.ID); !errors.Is(err, store.ErrNoClaim) {
			t.Fatalf("err %v, want ErrNoClaim", err)
		}
	})

	// Only verified claims are re-checked or lapsed: a pending one has its own
	// deadline, and lapsing it would replace a strike with a free block.
	t.Run("a claim that is not verified is neither due nor lapsed", func(t *testing.T) {
		c, err := st.StartCustomDomainClaim(user, "pending.example.test", time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE custom_domain_claims SET failing_since = NOW() - interval '2 hours' WHERE id = $1`, c.ID); err != nil {
			t.Fatal(err)
		}
		if due()[c.ID] {
			t.Fatal("a pending claim is listed for re-check")
		}
		if err := st.LapseCustomDomainClaim(c.ID, time.Hour); !errors.Is(err, store.ErrNoClaim) {
			t.Fatalf("a pending claim lapsed: %v", err)
		}
	})

	// A failing claim is looked at before the daily ones, or a full batch of
	// those would keep a record that came back from ending its run of misses.
	t.Run("failing claims come first", func(t *testing.T) {
		c := verified("first.example.test")
		if _, err := st.RecheckFailedCustomDomainClaim(c.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE custom_domain_claims SET checked_at = NULL, failing_since = NULL WHERE id <> $1 AND state = 'verified'`, c.ID); err != nil {
			t.Fatal(err)
		}
		list, err := st.ListClaimsDueRecheck(24*time.Hour, 1)
		if err != nil || len(list) != 1 || list[0].ID != c.ID {
			t.Fatalf("first due = %+v (%v), want the failing claim %d", list, err, c.ID)
		}
	})

	t.Run("a healthy claim never lapses", func(t *testing.T) {
		c := verified("healthy.example.test")
		if err := st.LapseCustomDomainClaim(c.ID, 0); !errors.Is(err, store.ErrNoClaim) {
			t.Fatalf("a claim with no misses lapsed: %v", err)
		}
	})
}
