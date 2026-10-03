package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeDBType(t *testing.T) {
	cases := map[string]string{
		"timescaledb": "timescaledb",
		"TimescaleDB": "timescaledb",
		" timescale ": "timescaledb",
		"ts":          "timescaledb",
		"postgres":    "postgres",
		"PostgreSQL":  "postgres",
		"pg":          "postgres",
		"plain":       "postgres",
		"":            "postgres", // unknown/empty -> safer plain backend
		"mysql":       "postgres", // unknown -> plain
	}
	for in, want := range cases {
		if got := NormalizeDBType(in); got != want {
			t.Errorf("NormalizeDBType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUsesTimescale(t *testing.T) {
	if !UsesTimescale("timescaledb") {
		t.Error("UsesTimescale(timescaledb) = false, want true")
	}
	if !UsesTimescale("TIMESCALE") {
		t.Error("UsesTimescale(TIMESCALE) = false, want true")
	}
	if UsesTimescale("postgres") {
		t.Error("UsesTimescale(postgres) = true, want false")
	}
	if UsesTimescale("") {
		t.Error("UsesTimescale(empty) = true, want false (defaults to plain)")
	}
}

// TestLoadConfig_RejectsPlaceholderSecrets locks in the fail-closed boot
// guard: LoadConfig must refuse to start with an unset or still-default
// JWT_SECRET/CLUSTER_SECRET, since either would make sessions or
// inter-service auth forgeable. See the Pre-flight note: core/config/ has
// no .env file, so godotenv.Load() is a no-op here and t.Setenv is
// authoritative.
func TestLoadConfig_RejectsPlaceholderSecrets(t *testing.T) {
	cases := []struct {
		name          string
		jwtSecret     string
		clusterSecret string
		wantErrSubstr string
	}{
		{"jwt secret unset (empty)", "", "a-real-cluster-secret-value", "JWT_SECRET"},
		{"jwt secret still the default placeholder", "change-this-secret", "a-real-cluster-secret-value", "JWT_SECRET"},
		{"cluster secret unset (empty)", "a-real-jwt-secret-value", "", "CLUSTER_SECRET"},
		{"cluster secret still the default placeholder", "a-real-jwt-secret-value", "dylaris-cluster-secret", "CLUSTER_SECRET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("JWT_SECRET", tc.jwtSecret)
			t.Setenv("CLUSTER_SECRET", tc.clusterSecret)
			_, err := LoadConfig()
			if err == nil {
				t.Fatal("expected LoadConfig to reject the placeholder/empty secret, got nil error")
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantErrSubstr)
			}
		})
	}
}

func TestLoadConfig_AcceptsRealSecrets(t *testing.T) {
	t.Setenv("JWT_SECRET", "a-real-random-jwt-secret-value")
	t.Setenv("CLUSTER_SECRET", "a-real-random-cluster-secret-value")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.JWTSecret != "a-real-random-jwt-secret-value" {
		t.Errorf("JWTSecret = %q", cfg.JWTSecret)
	}
	if cfg.ClusterSecret != "a-real-random-cluster-secret-value" {
		t.Errorf("ClusterSecret = %q", cfg.ClusterSecret)
	}
}

// TestGetSecret_FilePrecedence locks in the Docker/Portainer secrets
// precedence: <KEY>_FILE (trimmed) wins over the plain <KEY> env var, an
// unreadable/empty file falls through to the plain env var, and with
// neither set the fallback is used.
func TestGetSecret_FilePrecedence(t *testing.T) {
	t.Run("file takes precedence over plain env", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "secret")
		if err := os.WriteFile(path, []byte("  file-value  \n"), 0o600); err != nil {
			t.Fatalf("write temp secret file: %v", err)
		}
		t.Setenv("TESTSECRET_FILE", path)
		t.Setenv("TESTSECRET", "env-value")
		if got := getSecret("TESTSECRET", "fallback-value"); got != "file-value" {
			t.Errorf("getSecret = %q, want %q", got, "file-value")
		}
	})

	t.Run("missing file falls back to plain env", func(t *testing.T) {
		t.Setenv("TESTSECRET_FILE", filepath.Join(t.TempDir(), "does-not-exist"))
		t.Setenv("TESTSECRET", "env-value")
		if got := getSecret("TESTSECRET", "fallback-value"); got != "env-value" {
			t.Errorf("getSecret = %q, want %q", got, "env-value")
		}
	})

	t.Run("empty file falls back to plain env", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "empty-secret")
		if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
			t.Fatalf("write temp secret file: %v", err)
		}
		t.Setenv("TESTSECRET_FILE", path)
		t.Setenv("TESTSECRET", "env-value")
		if got := getSecret("TESTSECRET", "fallback-value"); got != "env-value" {
			t.Errorf("getSecret = %q, want %q", got, "env-value")
		}
	})

	t.Run("neither set returns the fallback", func(t *testing.T) {
		if got := getSecret("TESTSECRET", "fallback-value"); got != "fallback-value" {
			t.Errorf("getSecret = %q, want %q", got, "fallback-value")
		}
	})

	t.Run("plain env only", func(t *testing.T) {
		t.Setenv("TESTSECRET", "env-only-value")
		if got := getSecret("TESTSECRET", "fallback-value"); got != "env-only-value" {
			t.Errorf("getSecret = %q, want %q", got, "env-only-value")
		}
	})
}

// TAB_PROXY_HOST_SUFFIX is the whole configuration now, and an operator will
// type it in whatever shape their other variables use. Every one of these
// reaches the host matcher, which compares a BARE suffix, so anything left
// unrepaired matches nothing at all while reading perfectly correct in the
// compose file - the exact silent failure the two-variable pair it replaced
// used to produce.
func TestNormalizeTabProxyHostSuffix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "share.example.com", "share.example.com"},
		{"empty stays empty", "", ""},
		{"uppercase folded", "Share.Example.COM", "share.example.com"},
		{"whitespace trimmed", "  share.example.com  ", "share.example.com"},
		{"https scheme stripped", "https://share.example.com", "share.example.com"},
		{"http scheme stripped", "http://share.example.com", "share.example.com"},
		{"trailing slash stripped", "https://share.example.com/", "share.example.com"},
		{"path stripped", "https://share.example.com/tabs", "share.example.com"},
		{"port stripped", "share.example.com:8443", "share.example.com"},
		{"leading dot stripped", ".share.example.com", "share.example.com"},
		{"trailing dot stripped", "share.example.com.", "share.example.com"},
		{"single label refused", "share", ""},
		{"scheme only refused", "https://", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeTabProxyHostSuffix(tc.in); got != tc.want {
				t.Errorf("normalizeTabProxyHostSuffix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateAdminSecret(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty is valid (feature disabled)", "", false},
		{"too short (5)", "short", true},
		{"one below the floor (15)", "012345678901234", true},
		{"exactly the floor (16)", "0123456789012345", false},
		{"comfortably long", "correct-horse-battery-staple-2026", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAdminSecret(tc.in)
			if tc.wantErr && err == nil {
				t.Fatalf("validateAdminSecret(%q) = nil, want error", tc.in)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateAdminSecret(%q) = %v, want nil", tc.in, err)
			}
		})
	}
}

func TestIsLocalOrigin(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"http://localhost:25510", true},
		{"http://localhost", true},
		{"https://localhost:8443", true},
		{"http://127.0.0.1:25510", true},
		{"http://127.5.6.7", true},
		{"http://[::1]:25510", true},
		{"http://10.0.0.5:25510", true},
		{"http://172.16.4.9:3000", true},
		{"http://172.31.255.1", true},
		{"http://192.168.1.50:25510", true},
		{"http://[fd12:3456::1]:25510", true}, // IPv6 ULA
		// Public / non-local must NOT match - only FRONTEND_URL allowlists those.
		{"https://api.dylaris.com", false},
		{"https://panel.example.com", false},
		{"http://8.8.8.8", false},
		{"http://172.32.0.1", false},  // just outside 172.16/12
		{"http://169.254.1.1", false}, // link-local, not covered
		{"null", false},               // opaque origin
		{"", false},
		{"not a url", false},
	}
	for _, tc := range cases {
		if got := IsLocalOrigin(tc.in); got != tc.want {
			t.Errorf("IsLocalOrigin(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestValidateStoreSharedKey(t *testing.T) {
	if validateStoreSharedKey("") != nil {
		t.Error("an unset key (no store) was refused")
	}
	if validateStoreSharedKey(strings.Repeat("k", 31)) == nil {
		t.Error("a 31-character key was accepted")
	}
	if validateStoreSharedKey(strings.Repeat("k", 32)) != nil {
		t.Error("a 32-character key was refused")
	}
}
