package handlers

import "testing"

// TestValidateTabURL pins the custom-tab URL guard (server_tabs.go): must
// parse, must carry a host, scheme restricted to http/https (no host
// allowlist - custom tabs are the user's own infra).
func TestValidateTabURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"empty rejected", "", true},
		{"valid http", "http://example.com/panel", false},
		{"valid https", "https://example.com/panel", false},
		{"ftp scheme rejected", "ftp://example.com/panel", true},
		{"no host rejected", "/just/a/path", true},
		{"scheme-only garbage rejected", "://broken", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateTabURL(c.raw)
			if (err != nil) != c.wantErr {
				t.Errorf("validateTabURL(%q) err = %v, wantErr %v", c.raw, err, c.wantErr)
			}
		})
	}
}

// TestValidateSubdomain pins the per-hoster-domain subdomain validator
// (gateway.go): len 1..63, plus a mode-specific charset regex. Unknown mode
// always rejects (fail closed).
func TestValidateSubdomain(t *testing.T) {
	cases := []struct {
		name string
		sub  string
		mode string
		want bool
	}{
		// letters mode
		{"letters: pure letters accepted", "myserver", "letters", true},
		{"letters: digits rejected", "server1", "letters", false},
		{"letters: uppercase rejected (regex is lowercase-only)", "MyServer", "letters", false},
		{"letters: hyphen rejected", "my-server", "letters", false},

		// alphanumeric mode
		{"alphanumeric: letters+digits accepted", "server123", "alphanumeric", true},
		{"alphanumeric: hyphen rejected", "server-123", "alphanumeric", false},

		// dns mode
		{"dns: hyphen in middle accepted", "my-server-123", "dns", true},
		{"dns: single char accepted", "a", "dns", true},
		{"dns: leading hyphen rejected", "-server", "dns", false},
		{"dns: trailing hyphen rejected", "server-", "dns", false},

		// unknown mode fails closed regardless of an otherwise-valid value
		{"unknown mode rejected", "validlookingsub", "bogus-mode", false},

		// shared length bound (checked before the mode switch)
		{"empty rejected regardless of mode", "", "dns", false},
		{"exactly 63 chars accepted (boundary)", stringOfLen(63, 'a'), "letters", true},
		{"64 chars rejected (over boundary)", stringOfLen(64, 'a'), "letters", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := validateSubdomain(c.sub, c.mode); got != c.want {
				t.Errorf("validateSubdomain(%q, %q) = %v, want %v", c.sub, c.mode, got, c.want)
			}
		})
	}
}

func stringOfLen(n int, r rune) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = r
	}
	return string(b)
}
