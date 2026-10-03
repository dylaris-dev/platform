package database

import (
	"testing"
	"time"
)

// The hold is a column; a migration that never added it, or a read that never
// scans it, would leave every operator suspension liftable by the store again.
// Skipped without DYLARIS_TEST_DB_HOST, like its neighbours.
func TestIntegrationTheAdminHoldRoundTrips(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	user := f.server.OwnerID

	if err := st.SetUserBillingStatus(user, "suspended", nil, nil); err != nil {
		t.Fatalf("status: %v", err)
	}
	if err := st.SetUserBillingAdminHold(user, true); err != nil {
		t.Fatalf("hold: %v", err)
	}
	b, err := st.GetUserBilling(user)
	if err != nil || !b.AdminHold || b.Status != "suspended" {
		t.Fatalf("after hold: %+v, %v", b, err)
	}
	// A status write must not drop it.
	now := time.Now()
	if err := st.SetUserBillingStatus(user, "suspended", nil, &now); err != nil {
		t.Fatalf("status: %v", err)
	}
	if b, _ = st.GetUserBilling(user); !b.AdminHold {
		t.Fatal("a status write cleared the hold")
	}
	// Nothing moves a held row out of suspended, however late it read the hold.
	for _, status := range []string{"active", "past_due"} {
		if err := st.SetUserBillingStatus(user, status, nil, nil); err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		if b, _ = st.GetUserBilling(user); b.Status != "suspended" {
			t.Fatalf("a held row was moved to %q", b.Status)
		}
	}
	if err := st.SetUserBillingAdminHold(user, false); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if b, _ = st.GetUserBilling(user); b.AdminHold {
		t.Fatal("the hold did not clear")
	}
	if err := st.SetUserBillingStatus(user, "active", nil, nil); err != nil {
		t.Fatalf("active: %v", err)
	}
	if b, _ = st.GetUserBilling(user); b.Status != "active" {
		t.Fatalf("with the hold cleared the operator could not reactivate: %q", b.Status)
	}
}
