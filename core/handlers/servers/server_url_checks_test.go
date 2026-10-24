package servers

import "testing"

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
