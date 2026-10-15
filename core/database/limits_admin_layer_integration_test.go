package database

import "testing"

// An install with no store moves every per-user limit into the operator's
// layer, where the panel shows and clears it; a value already there wins.
// Skipped without DYLARIS_TEST_DB_HOST, like its neighbours.
func TestIntegrationOperatorLimitsMoveToTheAdminLayer(t *testing.T) {
	db, st := integrationDB(t)
	a := newFixture(t, st).server.OwnerID
	b := newFixture(t, st).server.OwnerID
	three, two, one := int64(3), int64(2), int64(1)
	if err := st.SetUserPurchasedEntitlement(a, &three, true, &two, true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUserLimitOverrides(b, "7", "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUserPurchasedEntitlement(b, &one, true, nil, false); err != nil {
		t.Fatal(err)
	}
	four := 4
	if err := st.SetGatewayRouteLimit("user:"+a, &four); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM gateway_route_limits WHERE scope IN ($1, $2)`, "user:"+a, "admin:"+a)
	})

	for i := 0; i < 2; i++ { // idempotent
		if err := st.MoveOperatorLimitsToAdminLayer(); err != nil {
			t.Fatal(err)
		}
	}

	ba, _ := st.GetUserBilling(a)
	if ba.MaxNodes != nil || ba.MaxLinks != nil || ba.AdminMaxNodes != "3" || ba.AdminMaxLinks != "2" {
		t.Fatalf("a: bought %v/%v, operator %q/%q; want nothing bought and 3/2 in the operator layer", ba.MaxNodes, ba.MaxLinks, ba.AdminMaxNodes, ba.AdminMaxLinks)
	}
	bb, _ := st.GetUserBilling(b)
	if bb.AdminMaxNodes != "7" || bb.MaxNodes != nil {
		t.Fatalf("b: operator %q, bought %v; want the operator's 7 kept", bb.AdminMaxNodes, bb.MaxNodes)
	}
	if l, _ := st.GetGatewayRouteLimit("user:" + a); l != nil {
		t.Fatal("the user scope row is still there")
	}
	if l, _ := st.GetGatewayRouteLimit("admin:" + a); l == nil || l.MaxRoutes == nil || *l.MaxRoutes != 4 {
		t.Fatalf("admin scope = %+v, want 4", l)
	}

	// The sentinel round-trips, and reads back as the operator's ceiling.
	if err := st.SetUserLimitOverrides(a, "unlimited", "0", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	ba, _ = st.GetUserBilling(a)
	if ba.AdminMaxNodes != "unlimited" || ba.AdminMaxLinks != "0" {
		t.Fatalf("operator layer = %q/%q", ba.AdminMaxNodes, ba.AdminMaxLinks)
	}
}
