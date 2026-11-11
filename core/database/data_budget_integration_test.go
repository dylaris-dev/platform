package database

import "testing"

// The owner's data budget is a users column (phase 60) holding the
// ParseLimitSetting sentinel. Pinned against a real Postgres: a fresh row
// reads "" (never set), every value round-trips, and an unknown user is an
// error rather than a silent no-op. Skipped without DYLARIS_TEST_DB_HOST.
func TestIntegrationDataBudgetRoundTrips(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	if raw, err := st.GetUserDataBudget(f.user.ID); err != nil || raw != "" {
		t.Fatalf("fresh user: %q %v, want \"\"", raw, err)
	}
	for _, v := range []string{"unlimited", "0", "1000", "1000000", ""} {
		if err := st.SetUserDataBudget(f.user.ID, v); err != nil {
			t.Fatalf("set %q: %v", v, err)
		}
		if raw, err := st.GetUserDataBudget(f.user.ID); err != nil || raw != v {
			t.Fatalf("read back %q: %q %v", v, raw, err)
		}
	}
	if err := st.SetUserDataBudget("00000000-0000-0000-0000-000000000000", "5"); err == nil {
		t.Fatal("unknown user: want an error")
	}
}
