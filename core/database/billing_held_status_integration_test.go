package database

import (
	"testing"
	"time"
)

// Lifting an operator's hold returns the account to what its payments say, and
// payments that arrive during the hold move that on. Lifting used to mean
// "active", so a tenant who stopped paying while held came back with
// everything.
func TestIntegrationLiftingAHoldReturnsToThePaymentStatus(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	user := f.server.OwnerID
	now := time.Now()

	if err := st.SetUserBillingStatus(user, "active", nil, nil); err != nil {
		t.Fatalf("active: %v", err)
	}
	if err := st.PlaceAdminHold(user, now); err != nil {
		t.Fatalf("hold: %v", err)
	}
	b, err := st.GetUserBilling(user)
	if err != nil || !b.AdminHold || b.Status != "suspended" || b.HeldStatus != "active" {
		t.Fatalf("after the hold: %+v, %v", b, err)
	}

	// A second hold does not overwrite what the payments said: underneath it
	// is still "active", not the "suspended" the account reads.
	if err := st.PlaceAdminHold(user, now); err != nil {
		t.Fatalf("second hold: %v", err)
	}
	if b, _ = st.GetUserBilling(user); b.HeldStatus != "active" {
		t.Fatalf("a hold over a hold forgot the payment status: %q", b.HeldStatus)
	}

	// The store cancels during the hold: the account still reads suspended,
	// and the cancellation is remembered underneath.
	if err := st.SetUserBillingStatus(user, "suspended", nil, &now); err != nil {
		t.Fatalf("store suspend: %v", err)
	}
	if b, _ = st.GetUserBilling(user); b.Status != "suspended" || b.HeldStatus != "suspended" {
		t.Fatalf("a cancellation under the hold: status %q held %q", b.Status, b.HeldStatus)
	}

	prior, err := st.LiftAdminHold(user)
	if err != nil || prior != "suspended" {
		t.Fatalf("lift: %q, %v - want the cancellation back", prior, err)
	}
	if b, _ = st.GetUserBilling(user); b.AdminHold || b.HeldStatus != "" || b.Status != "suspended" {
		t.Fatalf("after the lift: %+v", b)
	}
	if prior, err = st.LiftAdminHold(user); err != nil || prior != "" {
		t.Fatalf("lifting without a hold: %q, %v", prior, err)
	}

	// A payment during the hold is what lifting finds.
	if err := st.PlaceAdminHold(user, now); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := st.SetUserBillingStatus(user, "active", nil, nil); err != nil {
		t.Fatalf("store activate: %v", err)
	}
	if b, _ = st.GetUserBilling(user); b.Status != "suspended" {
		t.Fatalf("a payment moved a held row to %q", b.Status)
	}
	if prior, _ = st.LiftAdminHold(user); prior != "active" {
		t.Fatalf("lift after a payment: %q, want active", prior)
	}
	// The lift itself puts the account there, in the same statement.
	if b, _ = st.GetUserBilling(user); b.Status != "active" {
		t.Fatalf("after lifting to a paid-up account: status %q", b.Status)
	}
}

// A hold does not reset the clocks underneath it: a tenant already suspended
// for non-payment keeps the date their cutoff runs from, and a past_due one
// their grace deadline. Resetting them gave the one another 48 hours and the
// other a fresh grace window once the hold was lifted.
func TestIntegrationAHoldKeepsTheClocksUnderneathIt(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	user := f.server.OwnerID

	long := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
	if err := st.SetUserBillingStatus(user, "suspended", nil, &long); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if err := st.PlaceAdminHold(user, time.Now()); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if b, _ := st.GetUserBilling(user); b.SuspendedAt == nil || !b.SuspendedAt.Equal(long) {
		t.Fatalf("the hold moved the suspension date: %v, want %v", b.SuspendedAt, long)
	}
	if _, err := st.LiftAdminHold(user); err != nil {
		t.Fatalf("lift: %v", err)
	}

	grace := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	if err := st.SetUserBillingStatus(user, "past_due", &grace, nil); err != nil {
		t.Fatalf("past_due: %v", err)
	}
	if err := st.PlaceAdminHold(user, time.Now()); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if _, err := st.LiftAdminHold(user); err != nil {
		t.Fatalf("lift: %v", err)
	}
	if b, _ := st.GetUserBilling(user); b.Status != "past_due" || b.GraceUntil == nil || !b.GraceUntil.Equal(grace) {
		t.Fatalf("after the lift: status %q grace %v, want past_due until %v", b.Status, b.GraceUntil, grace)
	}
}

// A transition only right FROM certain states is decided in the write, so a
// writer that read the state earlier cannot undo what landed since.
func TestIntegrationAConditionalStatusWriteLosesToWhatLandedFirst(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	user := f.server.OwnerID
	now := time.Now()

	// The lifecycle read past_due; the payment landed first.
	if err := st.SetUserBillingStatus(user, "active", nil, nil); err != nil {
		t.Fatalf("active: %v", err)
	}
	wrote, err := st.SetUserBillingStatusIf(user, "suspended", nil, &now, []string{"past_due"})
	if err != nil || wrote {
		t.Fatalf("suspend from past_due over active: wrote=%v, %v", wrote, err)
	}
	if b, _ := st.GetUserBilling(user); b.Status != "active" {
		t.Fatalf("a paying tenant was suspended: %q", b.Status)
	}

	// Dunning read active; the suspension landed first.
	if err := st.SetUserBillingStatus(user, "suspended", nil, &now); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	grace := now.Add(time.Hour)
	if wrote, err = st.SetUserBillingStatusIf(user, "past_due", &grace, nil, []string{"active", "past_due"}); err != nil || wrote {
		t.Fatalf("dunning over a suspension: wrote=%v, %v", wrote, err)
	}
	if b, _ := st.GetUserBilling(user); b.Status != "suspended" {
		t.Fatalf("dunning lifted a suspension to %q", b.Status)
	}

	// Under a hold, from is asked of the payment status underneath.
	if err := st.SetUserBillingStatus(user, "past_due", &grace, nil); err != nil {
		t.Fatalf("past_due: %v", err)
	}
	if err := st.PlaceAdminHold(user, now); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if wrote, err = st.SetUserBillingStatusIf(user, "suspended", nil, &now, []string{"past_due"}); err != nil || !wrote {
		t.Fatalf("the grace running out under a hold: wrote=%v, %v", wrote, err)
	}
	if b, _ := st.GetUserBilling(user); b.Status != "suspended" || b.HeldStatus != "suspended" {
		t.Fatalf("under the hold: status %q held %q", b.Status, b.HeldStatus)
	}
}

// The mark that the cutoff switched a tenant's node links off survives a read
// and clears, and a second mark reports it was already there.
func TestIntegrationTheNodeLinksMarkRoundTrips(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	user := f.server.OwnerID

	if err := st.SetUserBillingStatus(user, "suspended", nil, nil); err != nil {
		t.Fatalf("status: %v", err)
	}
	if first, err := st.MarkNodeLinksOff(user); err != nil || !first {
		t.Fatalf("mark: %v, %v", first, err)
	}
	if again, err := st.MarkNodeLinksOff(user); err != nil || again {
		t.Fatalf("second mark: %v, %v", again, err)
	}
	if b, _ := st.GetUserBilling(user); !b.NodeLinksOff {
		t.Fatal("the mark does not read back")
	}
	if err := st.ClearNodeLinksOff(user); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if b, _ := st.GetUserBilling(user); b.NodeLinksOff {
		t.Fatal("the mark did not clear")
	}
}
