package database

import (
	"database/sql"
	"errors"
	"testing"
)

// A missing row says nothing, a NULL row is no cap, 0 is none, and a negative
// is read as no cap. Skipped without DYLARIS_TEST_DB_HOST.
func TestIntegrationGatewayCustomDomainLimits(t *testing.T) {
	db, st := integrationDB(t)
	const scope = "user:cap-test"
	t.Cleanup(func() { db.Exec(`DELETE FROM gateway_custom_domain_limits WHERE scope = $1`, scope) })

	if _, err := st.GetGatewayCustomDomainLimit(scope); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing row: err = %v, want ErrNoRows", err)
	}
	five, zero, neg := int64(5), int64(0), int64(-3)
	for _, c := range []struct {
		name string
		in   *int64
		want *int64
	}{
		{"cap", &five, &five},
		{"zero is kept as zero", &zero, &zero},
		{"NULL is a row with no cap", nil, nil},
		{"negative reads as no cap", &neg, nil},
	} {
		if err := st.SetGatewayCustomDomainLimit(scope, c.in); err != nil {
			t.Fatalf("%s: set: %v", c.name, err)
		}
		l, err := st.GetGatewayCustomDomainLimit(scope)
		if err != nil {
			t.Fatalf("%s: get: %v", c.name, err)
		}
		if (l.MaxCustomDomains == nil) != (c.want == nil) || (c.want != nil && *l.MaxCustomDomains != *c.want) {
			t.Fatalf("%s: got %v, want %v", c.name, l.MaxCustomDomains, c.want)
		}
	}
	// The route table is a different table: writing here leaves it empty.
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM gateway_route_limits WHERE scope = $1`, scope).Scan(&n)
	if n != 0 {
		t.Fatalf("route table has %d rows for %s", n, scope)
	}
	if err := st.DeleteGatewayCustomDomainLimit(scope); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetGatewayCustomDomainLimit(scope); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("after delete: err = %v, want ErrNoRows", err)
	}
}
