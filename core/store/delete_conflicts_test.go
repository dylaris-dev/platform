package store

import (
	"os"
	"strings"
	"testing"
)

// Three delete paths run into a foreign key that deliberately refuses:
// servers.owner_id (users), servers.node_id (nodes) and users.plan_id (plans).
// Only the first translated that into something an operator can act on; the
// other two collapsed it into a bare 500, which is the exact complaint the
// ErrUserOwnsServers comment already records ("a button that does not work and
// no hint that the remedy is to reassign or delete those servers first").
//
// Verified against the live testbed database, in a rolled-back transaction:
//
//	ERROR: update or delete on table "nodes" violates foreign key constraint
//	       "servers_node_id_fkey" on table "servers"
//
// These read the source, which is what makes them catch a FOURTH such path
// added later without the same treatment.
func TestRefusingForeignKeysAreTypedNotSwallowed(t *testing.T) {
	cases := []struct {
		name       string
		file       string
		constraint string
		errName    string
	}{
		{"user owns servers", "postgres.go", "servers_owner_id_fkey", "ErrUserOwnsServers"},
		{"node has servers", "postgres.go", "servers_node_id_fkey", "ErrNodeHasServers"},
		{"plan is in use", "plans.go", "users_plan_id_fkey", "ErrPlanInUse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatalf("read %s: %v", tc.file, err)
			}
			text := string(src)
			if !strings.Contains(text, tc.constraint) {
				t.Errorf("%s does not match on %q, so the violation reaches the handler untyped", tc.file, tc.constraint)
			}
			if !strings.Contains(text, "var "+tc.errName+" = errors.New(") {
				t.Errorf("%s is not declared in %s", tc.errName, tc.file)
			}
			// 23503 is foreign_key_violation. Matching the constraint name
			// without checking the code would also catch unrelated errors.
			if !strings.Contains(text, `pqErr.Code == "23503"`) {
				t.Errorf("%s does not check the foreign_key_violation code", tc.file)
			}
		})
	}
}

// The typed error is only worth anything if the handler answers 409 with the
// remedy rather than 500.
func TestDeleteConflictsAnswer409(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		errName string
	}{
		{"users", "../handlers/users/users.go", "ErrUserOwnsServers"},
		{"nodes", "../handlers/nodes/nodes.go", "ErrNodeHasServers"},
		// The plans case lived here until plan CRUD was removed. There is no
		// delete endpoint left to answer 409, so there is nothing to assert.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatalf("read %s: %v", tc.file, err)
			}
			text := string(src)
			// The exact branch shape, not just a mention of the identifier.
			// A weaker "does the file contain store.ErrX" check passed happily
			// when the condition was disabled with `if false && errors.Is(...)`
			// - the control proved the assertion, not the code.
			// Both branch styles are in use in this package; either is fine, but
			// the errors.Is has to BE the condition.
			want := ""
			for _, form := range []string{
				"if errors.Is(err, store." + tc.errName + ") {",
				"case errors.Is(err, store." + tc.errName + "):",
			} {
				if strings.Contains(text, form) {
					want = form
					break
				}
			}
			if want == "" {
				t.Fatalf("%s has no live branch guarding store.%s, so the refusal still reads as a fault", tc.file, tc.errName)
			}
			// That branch has to answer 409. Both spellings are in use here.
			after := text[strings.Index(text, want):]
			if end := strings.Index(after, "\n\t\t}"); end > 0 {
				after = after[:end]
			}
			if !strings.Contains(after, "409") && !strings.Contains(after, "http.StatusConflict") {
				t.Errorf("%s answers something other than 409 for a refusal that reflects the data", tc.file)
			}
		})
	}
}
