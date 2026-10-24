package tabs

import "testing"

// A user-chosen link is typed by people, read aloud and pasted into chat
// clients that guess where a URL ends. The rule is narrower than the URL
// grammar for that reason, not for security - a chosen slug is guessable by
// definition, which is the trade the user makes for a readable one.
func TestValidateCustomShareSlug(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty means random", "", "", false},
		{"whitespace only means random", "   ", "", false},
		{"plain", "bluemap", "bluemap", false},
		{"digits", "survival2026", "survival2026", false},
		{"inner hyphen", "max-survival-map", "max-survival-map", false},
		{"uppercase is folded, not refused", "BlueMap", "bluemap", false},
		{"surrounding space trimmed", "  bluemap  ", "bluemap", false},
		{"too short", "abc", "", true},
		{"too long", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "", true},
		{"leading hyphen", "-bluemap", "", true},
		{"trailing hyphen", "bluemap-", "", true},
		{"double hyphen", "blue--map", "", true},
		{"underscore", "blue_map", "", true},
		{"dot", "blue.map", "", true},
		{"slash would change the path", "blue/map", "", true},
		{"space", "blue map", "", true},
		{"percent encoding", "blue%2fmap", "", true},
		{"unicode lookalike", "bluemaр", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := validateCustomShareSlug(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("validateCustomShareSlug(%q) = %q, want an error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateCustomShareSlug(%q): %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("validateCustomShareSlug(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// The slug lands in a URL path. Anything that could END the path segment, start
// a query or climb out of it has to be refused here, because nothing downstream
// re-checks it - it is read straight out of {token} by the router.
func TestValidateCustomShareSlug_RefusesPathControlCharacters(t *testing.T) {
	for _, bad := range []string{
		"a/b", "a?b", "a#b", "a&b", "..", "a..b", "a\b", "a b",
		"a%00b", "a:b", "a@b", "a[b", "a]b", "%2e%2e",
	} {
		if got, err := validateCustomShareSlug(bad); err == nil {
			t.Errorf("validateCustomShareSlug(%q) = %q, want refusal", bad, got)
		}
	}
}
