package health

import "testing"

// The rule this decides: how an operator-set URL becomes a name to resolve and a
// port to dial.
//
// It exists because the DNS check had no notion of the API host at all. The
// panel's browser calls it for every request, so a missing api. record produces
// a panel that loads and then does nothing - and the check reported everything
// green, because it only ever looked at FRONTEND_URL.
func TestHostAndDialTarget(t *testing.T) {
	cases := []struct {
		name, in, wantHost, wantDial string
	}{
		{"https defaults to 443", "https://api.example.com", "api.example.com", "api.example.com:443"},
		{"http defaults to 80", "http://api.example.com", "api.example.com", "api.example.com:80"},
		{"an explicit port wins", "https://api.example.com:8443", "api.example.com", "api.example.com:8443"},
		{"a path is irrelevant", "https://api.example.com/api", "api.example.com", "api.example.com:443"},
		{"surrounding whitespace is trimmed", "  https://api.example.com  ", "api.example.com", "api.example.com:443"},
		// A bare hostname parses as a path with no host, which would otherwise
		// produce an empty name that resolves to nothing and reads as a broken
		// record rather than an unset setting.
		{"a bare hostname yields nothing", "api.example.com", "", ""},
		{"empty yields nothing", "", "", ""},
		{"garbage yields nothing", "://", "", ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, dial := hostAndDialTarget(c.in)
			if host != c.wantHost || dial != c.wantDial {
				t.Errorf("hostAndDialTarget(%q) = (%q, %q), want (%q, %q)", c.in, host, dial, c.wantHost, c.wantDial)
			}
		})
	}
}
