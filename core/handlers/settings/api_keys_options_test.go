package settings

import "testing"

// The whitelist is a NARROWING filter, so an entry that no key could carry
// narrows nothing. Dropping such an entry keeps the field editable after a
// capability is renamed; refusing the save would make it impossible to fix.
func TestSanitizeKeyCapList(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"trims and keeps order", " console.send , rcon.exec ", "console.send,rcon.exec"},
		{"drops duplicates", "rcon.exec,rcon.exec", "rcon.exec"},
		{"drops unknown ids", "rcon.exec,not.a.cap", "rcon.exec"},
		// A PANEL capability is rejected on a key by authz.ValidKeyCap, so
		// listing one here could only ever mislead the operator into thinking it
		// was permitted.
		{"drops panel capabilities", "rcon.exec,users.write", "rcon.exec"},
		{"empty stays empty, which means no restriction", "  ", ""},
		{"a list of only invalid ids collapses to no restriction", "nope,users.write", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeKeyCapList(c.in); got != c.want {
				t.Errorf("sanitizeKeyCapList(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
