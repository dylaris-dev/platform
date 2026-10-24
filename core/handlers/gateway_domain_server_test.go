package handlers

import (
	"testing"

	"dylaris-core/store"
)

// TestIsSnapshotFetchHostAllowed pins the SSRF host-allowlist gate
// (server_modpack_snapshot.go): cdn.modrinth.com or this Core's own pack mirror
// host, everything else refused.
func TestIsSnapshotFetchHostAllowed(t *testing.T) {
	localMirrorSettings := map[string]string{
		"modpack_storage_provider": "local",
		"core_public_url":          "https://core.example.com",
	}

	t.Run("modrinth cdn is allowed", func(t *testing.T) {
		h := newServerSnapshotHandler(localMirrorSettings)
		if !h.isSnapshotFetchHostAllowed("https://cdn.modrinth.com/data/abc/file.mrpack") {
			t.Fatalf("expected cdn.modrinth.com to be allowed")
		}
	})

	t.Run("this core's own mirror host is allowed", func(t *testing.T) {
		h := newServerSnapshotHandler(localMirrorSettings)
		if !h.isSnapshotFetchHostAllowed("https://core.example.com/mirror/modpacks/abc/pack.mrpack") {
			t.Fatalf("expected the configured core_public_url host to be allowed")
		}
	})

	// The Solder "public bucket" base no longer widens this list. A leftover
	// solder_mirror_url must not open a host a pack install never fetches from:
	// every panel-built pack is served from Core's own mirror now.
	t.Run("a leftover solder mirror url opens nothing", func(t *testing.T) {
		h := newServerSnapshotHandler(map[string]string{
			"modpack_storage_provider": "s3",
			"core_public_url":          "https://core.example.com",
			"solder_delivery_mode":     "public",
			"solder_mirror_url":        "https://cdn.myhoster.example/mirror",
		})
		if h.isSnapshotFetchHostAllowed("https://cdn.myhoster.example/mirror/pack.mrpack") {
			t.Fatalf("a Solder-only setting still widened the SSRF allowlist")
		}
	})

	t.Run("arbitrary host is refused", func(t *testing.T) {
		h := newServerSnapshotHandler(localMirrorSettings)
		if h.isSnapshotFetchHostAllowed("https://evil.example.com/data/abc/file.mrpack") {
			t.Fatalf("expected an arbitrary host to be refused")
		}
	})

	t.Run("garbage URL is refused", func(t *testing.T) {
		h := newServerSnapshotHandler(localMirrorSettings)
		if h.isSnapshotFetchHostAllowed("%zz") {
			t.Fatalf("expected an unparseable URL to be refused")
		}
	})

	// NOTE (real behavior, not a fix): isSnapshotFetchHostAllowed only ever
	// inspects u.Hostname() - it does not check u.Scheme at all. An
	// http:// URL to the same allowlisted host passes this gate exactly like
	// https://. This is not a standalone SSRF hole: the actual fetch goes
	// through services.SafeFetch, which independently rejects any scheme
	// other than "http"/"https" - so http is a deliberately still-allowed
	// scheme downstream, just not vetted a second time at this layer.
	t.Run("http scheme on an allowed host is NOT rejected by this gate (scheme is not checked here)", func(t *testing.T) {
		h := newServerSnapshotHandler(localMirrorSettings)
		if !h.isSnapshotFetchHostAllowed("http://cdn.modrinth.com/data/abc/file.mrpack") {
			t.Fatalf("expected http scheme on an allowed host to still pass this host-only gate")
		}
	})
}

// TestCnameLabelValidation pins that the custom-domain CNAME setting is a single
// DNS LABEL, not a full domain. It is prefixed onto every hoster base, so a
// value like "route.eu.example.com" would silently build
// "route.eu.example.com.eu.example.com" - a name that resolves nowhere and only
// surfaces as a customer whose CNAME never works. Save-side validation uses the
// same subRegexDNS the subdomain picker uses, so this pins the shared contract.
func TestCnameLabelValidation(t *testing.T) {
	valid := []string{"route", "connect", "mc", "r", "play-here", "a1"}
	for _, v := range valid {
		if !subRegexDNS.MatchString(v) {
			t.Errorf("label %q should be accepted as a CNAME label", v)
		}
	}

	invalid := []string{
		"route.eu.example.com", // the whole point: a full domain is rejected
		"route.eu",
		".route",
		"route.",
		"-route", // a DNS label may not start with a hyphen
		"route-", // ...nor end with one
		"ROUTE",  // callers lowercase first; the raw form must not slip through
		"ro ute",
		"route_1",
	}
	for _, v := range invalid {
		if subRegexDNS.MatchString(v) {
			t.Errorf("label %q should be rejected as a CNAME label", v)
		}
	}
}

// newServerSnapshotHandler builds a ServerHandler over a settings map. It was
// gateway_domain_test.go's, built on gatewayDomainFakeStore; the snapshot host
// check only reads settings, so a settings fake is all it needs here.
func newServerSnapshotHandler(settings map[string]string) *ServerHandler {
	return &ServerHandler{state: &AppState{Store: &snapshotSettingsStore{settings: settings}}}
}

type snapshotSettingsStore struct {
	store.Store
	settings map[string]string
}

func (f *snapshotSettingsStore) GetSetting(key string) (string, error) { return f.settings[key], nil }
