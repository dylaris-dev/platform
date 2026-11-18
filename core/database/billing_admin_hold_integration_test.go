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

// A payment suspension that arrives DURING an operator's hold carries its own
// date out of the hold. It used to keep the hold's suspended_at, so lifting a
// hold placed days earlier cut the tenant off at once and deleted their
// backups on the next pass, for a suspension one day old. A hold placed over
// an existing suspension keeps that suspension's date.
func TestIntegrationASuspensionUnderTheHoldKeepsItsOwnDate(t *testing.T) {
	_, st := integrationDB(t)
	day0 := time.Now().Add(-10 * 24 * time.Hour).UTC().Truncate(time.Second)
	day9 := day0.Add(9 * 24 * time.Hour)
	cases := []struct {
		name       string
		before     string     // payment status before the hold
		beforeAt   *time.Time // its suspended_at
		wantAtLift time.Time
	}{
		{"held while active, suspended during the hold", "active", nil, day9},
		{"held while already suspended", "suspended", &day0, day0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, st)
			user := f.server.OwnerID
			if err := st.SetUserBillingStatus(user, tc.before, nil, tc.beforeAt); err != nil {
				t.Fatalf("status: %v", err)
			}
			if err := st.PlaceAdminHold(user, day0); err != nil {
				t.Fatalf("hold: %v", err)
			}
			if _, err := st.SetUserBillingStatusIf(user, "suspended", nil, &day9, nil); err != nil {
				t.Fatalf("suspend under hold: %v", err)
			}
			prior, err := st.LiftAdminHold(user)
			if err != nil || prior != "suspended" {
				t.Fatalf("lift: prior %q, %v", prior, err)
			}
			b, err := st.GetUserBilling(user)
			if err != nil || b.SuspendedAt == nil {
				t.Fatalf("after lift: %+v, %v", b, err)
			}
			if !b.SuspendedAt.Equal(tc.wantAtLift) {
				t.Errorf("suspended_at after lift = %s, want %s", b.SuspendedAt.UTC(), tc.wantAtLift)
			}
		})
	}
}
