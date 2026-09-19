package handlers

import "testing"

// TestValidateLocalTarget pins validateLocalTarget (gateway_link_routes.go).
// Route-only targets are dialed by the CUSTOMER's own Link on their local
// network, so LAN/loopback/private IPs are intentionally accepted here -
// only empty, malformed, or wildcard hostnames are rejected.
func TestValidateLocalTarget(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		wantErr bool
	}{
		{"valid hostname", "my-pc.local", false},
		{"valid IPv4", "203.0.113.5", false},
		{"loopback IPv4 accepted (Link, not Core, decides what it dials)", "127.0.0.1", false},
		{"loopback IPv6 refused: the Link only treats 127.0.0.1 as this machine", "::1", true},
		{"IPv4-mapped loopback refused for the same reason", "::ffff:127.0.0.1", true},
		{"LAN IPv4 accepted", "192.168.1.50", false},
		{"mixed-case hostname is lowercased and accepted", "My-PC.LOCAL", false},
		{"wildcard rejected", "*.example.com", true},
		{"empty rejected", "", true},
		{"whitespace-only rejected", "   ", true},
		{"malformed hostname (space) rejected", "not a host", true},
		{"malformed hostname (invalid chars) rejected", "host_$$_name", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateLocalTarget(c.host)
			if (err != nil) != c.wantErr {
				t.Errorf("validateLocalTarget(%q) err = %v, wantErr %v", c.host, err, c.wantErr)
			}
		})
	}
}

// TestIsAllowedModrinthURL pins the SSRF guard on mod-download URLs
// (server_mods.go): https-only, host must be in modrinthAllowedHosts.
func TestIsAllowedModrinthURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want bool
	}{
		{"allowed host over https", "https://cdn.modrinth.com/data/abc/file.jar", true},
		{"http scheme rejected even for allowed host", "http://cdn.modrinth.com/data/abc/file.jar", false},
		{"disallowed host rejected", "https://evil.example.com/data/abc/file.jar", false},
		{"garbage url rejected", "not-a-url-at-all", false},
		{"https with no path separator after host rejected", "https://cdn.modrinth.com", false},
		{"https with empty host (triple slash) rejected", "https:///data/abc/file.jar", false},
		{"ftp scheme rejected", "ftp://cdn.modrinth.com/data/abc/file.jar", false},
		{"subdomain-lookalike host rejected", "https://cdn.modrinth.com.evil.com/data/abc/file.jar", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isAllowedModrinthURL(c.url); got != c.want {
				t.Errorf("isAllowedModrinthURL(%q) = %v, want %v", c.url, got, c.want)
			}
		})
	}
}

// TestDefaultTargetDirForLoader pins the mods-vs-plugins default mapping
// (server_mods.go). Unknown/empty loaders default to "mods" (the safer
// fallback per the source comment).
func TestDefaultTargetDirForLoader(t *testing.T) {
	cases := []struct {
		name   string
		loader string
		want   string
	}{
		{"paper", "paper", "plugins"},
		{"spigot", "spigot", "plugins"},
		{"bukkit", "bukkit", "plugins"},
		{"purpur", "purpur", "plugins"},
		{"pufferfish", "pufferfish", "plugins"},
		{"velocity", "velocity", "plugins"},
		{"waterfall", "waterfall", "plugins"},
		{"bungeecord", "bungeecord", "plugins"},
		{"case-insensitive match", "PAPER", "plugins"},
		{"fabric defaults to mods", "fabric", "mods"},
		{"forge defaults to mods", "forge", "mods"},
		{"quilt defaults to mods", "quilt", "mods"},
		{"neoforge defaults to mods", "neoforge", "mods"},
		{"unknown loader defaults to mods", "totally-unknown", "mods"},
		{"empty loader defaults to mods", "", "mods"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := defaultTargetDirForLoader(c.loader); got != c.want {
				t.Errorf("defaultTargetDirForLoader(%q) = %q, want %q", c.loader, got, c.want)
			}
		})
	}
}

// TestNormalizeSide pins the canonical-side coercion (server_modpack_snapshot.go).
func TestNormalizeSide(t *testing.T) {
	cases := []struct {
		name string
		side string
		want string
	}{
		{"client", "client", "client"},
		{"server", "server", "server"},
		{"both", "both", "both"},
		{"empty defaults to both", "", "both"},
		{"unknown defaults to both", "bogus", "both"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeSide(c.side); got != c.want {
				t.Errorf("normalizeSide(%q) = %q, want %q", c.side, got, c.want)
			}
		})
	}
}

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
