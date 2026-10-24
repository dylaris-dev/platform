package billing

import (
	"encoding/json"
	"testing"
)

// The rule these decide: what a store-sent address pool means, now that the
// platform speaks ONE convention.
//
//	absent -> leave the column alone
//	null   -> delete the row, so the tenant defers to the platform default
//	0      -> a real cap of none
//	n      -> the cap
//
// This used to be parsed with parseEntitlement, which is written for
// user_billing and converts any non-positive value into "clear the override".
// gateway_route_limits then fell through user_default and global and, with
// neither set, came out UNLIMITED. The one number meaning "no addresses"
// produced unlimited addresses.
//
// The store only ever sends 0 when the tenant HAS bought something and the
// operator has set the per-unit allowances to zero, i.e. "my products include no
// addresses". That is an instruction, not an absence, and JSON already tells the
// two apart.
func TestParseRoutePoolKeepsAMeaningfulZero(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantSet bool
		wantVal *int64
	}{
		{"absent leaves the column alone", "", false, nil},
		{"explicit null defers to the platform default", "null", true, nil},
		{"zero means no addresses, and is written as such", "0", true, i64(0)},
		{"a real allowance is written", "10", true, i64(10)},
		{"a negative is nonsense; none is the safe reading", "-5", true, i64(0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, set, err := parseRoutePool(json.RawMessage(c.raw))
			if err != nil {
				t.Fatalf("parseRoutePool: %v", err)
			}
			if set != c.wantSet {
				t.Fatalf("set = %v, want %v", set, c.wantSet)
			}
			switch {
			case c.wantVal == nil && got != nil:
				t.Fatalf("value = %d, want nil (delete the row)", *got)
			case c.wantVal != nil && got == nil:
				t.Fatalf("value = nil (the row would be DELETED and the tenant falls through to the platform default), want %d", *c.wantVal)
			case c.wantVal != nil && *got != *c.wantVal:
				t.Fatalf("value = %d, want %d", *got, *c.wantVal)
			}
		})
	}
}

// The sibling must NOT change. user_billing's max_nodes/max_links are pushed by
// the store as "how many did they buy", and a zero there means they bought none
// of that product - which parseEntitlement turns into "clear the override" so
// the ENTITLEMENT gate, not a cap, is what refuses them. Two different questions
// on two different tables, which is why they cannot share a parser.
func TestParseEntitlementStillClearsOnZero(t *testing.T) {
	got, set, err := parseEntitlement(json.RawMessage("0"))
	if err != nil {
		t.Fatalf("parseEntitlement: %v", err)
	}
	if !set || got != nil {
		t.Errorf("parseEntitlement(0) = (%v, %v), want (nil, true)", got, set)
	}
}

func i64(v int64) *int64 { return &v }
