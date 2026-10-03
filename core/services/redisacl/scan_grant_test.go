package redisacl

import "testing"

// SCAN is a keyspace-wide read of NAMES, and Redis does not filter it by the
// ACL's key patterns.
//
// Measured against Valkey 8 rather than assumed: a user scoped to a single
// prefix ran SCAN and got back keys belonging to every other prefix; a GET on
// one of them then answered NOPERM. Values are protected, names are not - and
// the names are the sensitive part here, because every server UUID, node token,
// SFTP account name and link token on the platform is a key name, and those are
// exactly the identifiers a forged write needs as input.
//
// Only the node agent may hold it, and only because it genuinely walks the
// keyspace (Core discovery, port allocation, the disk-full sweep). Everything
// else here runs somewhere a tenant can reach: the log-shipper credential is in
// the environment of the tenant's own Minecraft container, beside plugins the
// tenant wrote, and it never called SCAN once.
func TestOnlyTheNodeAgentMayScan(t *testing.T) {
	cases := []struct {
		name  string
		rules []interface{}
		want  bool
	}{
		{"node agent", BuildNodeACLRules("node-a", "pw", []string{"srv-1"}), true},
		{"log-shipper in the tenant's container", BuildShipperACLRules("pw", "srv-1"), false},
		{"the node's link sidecar", BuildLinkACLRules("pw", "node-a", "tok"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mayScan(c.rules); got != c.want {
				if c.want {
					t.Error("the node agent lost +scan; its keyspace walks would fail with NOPERM")
				} else {
					t.Error("this principal may enumerate every key name on the platform")
				}
			}
			// KEYS must stay denied for all three, which -@dangerous does.
			if !grants(c.rules, "-@dangerous") {
				t.Error("KEYS is no longer denied")
			}
		})
	}
}

// mayScan evaluates the rules in order, as Redis does: the LAST rule that
// mentions SCAN - directly, or through a category it belongs to - decides.
//
// Checking for the literal "+scan" was the test this replaced, and it agreed
// with a comment that said SCAN was not granted while "+@read" granted it to
// every principal: SCAN is in @read and @keyspace, and "-@dangerous" does not
// remove it. Production had all nine scoped users able to SCAN.
func mayScan(rules []interface{}) bool {
	allowed := false
	for _, r := range rules {
		switch r {
		case "+@all", "allcommands", "+@read", "+@keyspace", "+scan":
			allowed = true
		case "-@all", "nocommands", "-@read", "-@keyspace", "-scan":
			allowed = false
		}
	}
	return allowed
}

func grants(rules []interface{}, want string) bool {
	for _, r := range rules {
		if s, ok := r.(string); ok && s == want {
			return true
		}
	}
	return false
}
