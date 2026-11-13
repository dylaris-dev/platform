package database

import "testing"

// custom_domain_cap_since: set only by a clear that removed a row, kept on a
// second clear, cleared by a set, read back through GetUserBilling, and a
// second boot of the migration is a no-op. Skipped without DYLARIS_TEST_DB_HOST.
func TestIntegrationCustomDomainCapSince(t *testing.T) {
	db, st := integrationDB(t)
	if err := applyCustomDomainCapSince(db); err != nil {
		t.Fatalf("second run: %v", err)
	}
	f := newFixture(t, st)
	user := f.server.OwnerID
	t.Cleanup(func() { _ = st.DeleteGatewayCustomDomainLimit("user:" + user) })
	if err := st.SetUserBillingStatus(user, "active", nil, nil); err != nil {
		t.Fatal(err)
	}
	marker := func(stage string, want bool) {
		t.Helper()
		b, err := st.GetUserBilling(user)
		if err != nil || b == nil || (b.CustomDomainCapSince != nil) != want {
			t.Fatalf("%s: billing = %+v, %v; want marker %v", stage, b, err, want)
		}
	}

	// Never managed: a clear finds no row and marks nothing.
	if err := st.ClearUserCustomDomainLimit(user); err != nil {
		t.Fatal(err)
	}
	marker("clear without a row", false)

	// Managed: the unlimited row, no marker.
	if err := st.SetUserCustomDomainLimit(user, nil); err != nil {
		t.Fatal(err)
	}
	if l, err := st.GetGatewayCustomDomainLimit("user:" + user); err != nil || l.MaxCustomDomains != nil {
		t.Fatalf("row = %+v, %v; want a NULL row", l, err)
	}
	marker("managed", false)

	// The plan ends: row gone, marker set; a second clear keeps the first stamp.
	if err := st.ClearUserCustomDomainLimit(user); err != nil {
		t.Fatal(err)
	}
	marker("ended", true)
	b, _ := st.GetUserBilling(user)
	first := *b.CustomDomainCapSince
	if err := st.ClearUserCustomDomainLimit(user); err != nil {
		t.Fatal(err)
	}
	if b, _ = st.GetUserBilling(user); b.CustomDomainCapSince == nil || !b.CustomDomainCapSince.Equal(first) {
		t.Fatalf("second clear moved the marker: %v -> %v", first, b.CustomDomainCapSince)
	}
	if _, err := st.GetGatewayCustomDomainLimit("user:" + user); err == nil {
		t.Fatal("the row survived the clear")
	}

	// Managed again: marker cleared. The status write in between keeps it.
	if err := st.SetUserBillingStatus(user, "active", nil, nil); err != nil {
		t.Fatal(err)
	}
	marker("after a status write", true)
	five := int64(5)
	if err := st.SetUserCustomDomainLimit(user, &five); err != nil {
		t.Fatal(err)
	}
	marker("managed again", false)
}
