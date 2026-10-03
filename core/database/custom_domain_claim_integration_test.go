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
