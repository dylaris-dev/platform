package gateway

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/handlers"
)

// The rule this decides: Core's proof is byte-identical to what the gateway Hub
// verifies.
//
// FROZEN CROSS-REPO CONTRACT. The vector below is produced by
// `gateway/pkg/redisacl.Proof("test-cluster-secret", "core", 1700000000)` and
// the same vector is asserted on that side. The two repositories build
// independently, so nothing but a pinned value catches a change to the message
// format - and the failure it prevents is silent in the worst way: every DNS
// save from the panel returns 401 with nothing on either side explaining why.
func TestHubProofMatchesTheGatewayVector(t *testing.T) {
	const want = "6b05b07a6517e84980eacaec4efa2b53f983fd61a6b2a305f29e00b5649ca682"
	got := hubProof("test-cluster-secret", "core", 1700000000)
	if got != want {
		t.Fatalf("hubProof = %s, want %s\n\nThe gateway Hub will reject every request from this platform.\nIf the gateway deliberately changed its proof format, change it here in the same release.", got, want)
	}
}

// A different secret, principal or timestamp must produce a different proof -
// otherwise the signature is not binding any of them.
func TestHubProofBindsEveryInput(t *testing.T) {
	base := hubProof("secret", "core", 1700000000)
	cases := []struct {
		name              string
		secret, principal string
		ts                int64
	}{
		{"another secret", "other", "core", 1700000000},
		{"another principal", "secret", "edge", 1700000000},
		{"another timestamp", "secret", "core", 1700000001},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if hubProof(c.secret, c.principal, c.ts) == base {
				t.Error("the proof did not change; this input is not signed")
			}
		})
	}
}

// The rule this decides: with no gateway configured, the settings surface
// reports that state instead of failing.
//
// A platform-only install has no gateway, no relay and no records to write. An
// error there would send an admin looking for a broken hub they never deployed.
func TestForwardWithoutAHubReportsUnavailable(t *testing.T) {
	h := NewGatewayDNSHandler(&handlers.AppState{ClusterSecret: "secret"})
	rec := httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest(http.MethodGet, "/api/settings/gateway/dns", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["success"] != true {
		t.Errorf("success = %v, want true", body["success"])
	}
	if body["available"] != false {
		t.Errorf("available = %v, want false", body["available"])
	}
}

// The rule this decides: what actually goes on the wire to the Hub.
//
// Three things at once, and each is a defect if it drifts: the request is signed
// so the Hub accepts it, the CLUSTER_SECRET itself never travels, and the token
// the admin typed is forwarded rather than stored here.
func TestSaveForwardsASignedRequest(t *testing.T) {
	const secret = "cluster-secret-value"
	var seen map[string]any

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/dns-config" {
			t.Errorf("path = %s, want /internal/dns-config", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Errorf("decode: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"provider": "cloudflare", "zones": []string{"example.com"},
			"enabled": true, "has_token": true,
		})
	}))
	defer hub.Close()

	h := NewGatewayDNSHandler(&handlers.AppState{ClusterSecret: secret, GatewayHubURL: hub.URL})
	req := httptest.NewRequest(http.MethodPut, "/api/settings/gateway/dns",
		strings.NewReader(`{"provider":" cloudflare ","token":"cf-token","zones":["example.com"],"enabled":true}`))
	rec := httptest.NewRecorder()
	h.Save(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if seen["action"] != "write" {
		t.Errorf("action = %v, want write", seen["action"])
	}
	if seen["provider"] != "cloudflare" {
		t.Errorf("provider = %v, want it trimmed to cloudflare", seen["provider"])
	}
	if seen["token"] != "cf-token" {
		t.Errorf("token = %v, want it forwarded verbatim", seen["token"])
	}
	if seen["principal"] != hubProofPrincipal {
		t.Errorf("principal = %v, want %s", seen["principal"], hubProofPrincipal)
	}
	ts, ok := seen["ts"].(float64)
	if !ok || ts == 0 {
		t.Fatalf("ts = %v, want a unix timestamp", seen["ts"])
	}
	if want := hubProof(secret, hubProofPrincipal, int64(ts)); seen["proof"] != want {
		t.Errorf("proof = %v, want %s - the hub would reject this", seen["proof"], want)
	}
	if body, _ := json.Marshal(seen); strings.Contains(string(body), secret) {
		t.Error("the cluster secret itself is in the request body")
	}
}

// The rule this decides: the Hub's own rejection reaches the person at the form.
//
// "name at least one zone" is written for an admin. Flattening it into "the
// gateway returned 400" would leave them with a form that refuses to save and no
// statement of what is wrong with it.
func TestHubRejectionIsPassedThrough(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "name at least one zone: without one, every record is unroutable", http.StatusBadRequest)
	}))
	defer hub.Close()

	h := NewGatewayDNSHandler(&handlers.AppState{ClusterSecret: "s", GatewayHubURL: hub.URL})
	rec := httptest.NewRecorder()
	h.Save(rec, httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(`{"enabled":true}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "name at least one zone") {
		t.Errorf("the hub's message did not survive the hop: %s", rec.Body.String())
	}
}

// A 401 means the two services disagree about CLUSTER_SECRET. Passing the Hub's
// bare "unauthorized" through would read as "your login expired" on a screen the
// admin is already authenticated to.
func TestClusterSecretMismatchSaysSo(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer hub.Close()

	h := NewGatewayDNSHandler(&handlers.AppState{ClusterSecret: "s", GatewayHubURL: hub.URL})
	rec := httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if !strings.Contains(rec.Body.String(), "CLUSTER_SECRET") {
		t.Errorf("a secret mismatch was not named: %s", rec.Body.String())
	}
}

// An unreachable Hub must not read as a bad request from the admin.
func TestUnreachableHubIsABadGateway(t *testing.T) {
	h := NewGatewayDNSHandler(&handlers.AppState{ClusterSecret: "s", GatewayHubURL: "http://127.0.0.1:1"})
	rec := httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

// The response type has no token field, so a token the Hub might send could not
// be relayed to the browser even by accident. This asserts that stays true.
func TestTokenIsNeverRelayedToTheBrowser(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"provider": "cloudflare", "has_token": true,
			"token": "super-secret-cf-token", "enc_token": "deadbeef",
		})
	}))
	defer hub.Close()

	h := NewGatewayDNSHandler(&handlers.AppState{ClusterSecret: "s", GatewayHubURL: hub.URL})
	rec := httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	body := rec.Body.String()
	for _, leak := range []string{"super-secret-cf-token", "deadbeef"} {
		if strings.Contains(body, leak) {
			t.Errorf("%q reached the browser: %s", leak, body)
		}
	}
	if !strings.Contains(body, `"has_token":true`) {
		t.Errorf("has_token did not survive, so the form cannot say a token is stored: %s", body)
	}
}

// The rule this decides: the certificate half of the form reaches the Hub too.
//
// It shares the credential, so it shares the card and the payload. A field that
// silently stopped being forwarded would leave an admin ticking a box that never
// takes effect, on a screen that shows it ticked after the save.
func TestSaveForwardsTheCertificateHalf(t *testing.T) {
	var seen map[string]any
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seen)
		_ = json.NewEncoder(w).Encode(map[string]any{"provider": "cloudflare", "acme_enabled": true})
	}))
	defer hub.Close()

	h := NewGatewayDNSHandler(&handlers.AppState{ClusterSecret: "s", GatewayHubURL: hub.URL})
	rec := httptest.NewRecorder()
	h.Save(rec, httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(
		`{"provider":"cloudflare","zones":["example.com"],"enabled":true,`+
			`"acme_enabled":true,"acme_email":"  ops@example.com  ","acme_directory":" staging ","acme_agreed":true}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	for key, want := range map[string]any{
		"acme_enabled":   true,
		"acme_email":     "ops@example.com",
		"acme_directory": "staging",
		"acme_agreed":    true,
	} {
		if seen[key] != want {
			t.Errorf("%s = %v, want %v", key, seen[key], want)
		}
	}
}

// The certificate status is relayed as the gateway sent it. Core owns no part of
// that shape, and re-declaring it here would be a second definition to keep in
// step for nothing - but it still has to arrive, because it is the only place an
// admin learns why issuance failed.
func TestCertStatusIsRelayed(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"provider": "cloudflare",
			"cert_status": map[string]any{
				"last_run_at": "2026-08-25T20:00:00Z",
				"names": []map[string]any{
					{"name": "beam.example.com", "have": false, "error": "HTTP 400 invalidContact"},
				},
			},
		})
	}))
	defer hub.Close()

	h := NewGatewayDNSHandler(&handlers.AppState{ClusterSecret: "s", GatewayHubURL: hub.URL})
	rec := httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	body := rec.Body.String()
	for _, want := range []string{"cert_status", "beam.example.com", "HTTP 400 invalidContact"} {
		if !strings.Contains(body, want) {
			t.Errorf("%q did not survive the hop: %s", want, body)
		}
	}
}

// Core must not post a signed proof - or, on the write path, the operator's DNS
// credential - to whoever answers a name that has not resolved.
//
// The proof is a one-way HMAC over (principal, ts); nothing in the Hub's reply
// is bound to the cluster secret, so the recipient holds a working credential
// for the whole skew window. The Hub serves /internal/* on the same listener
// standalone customers reach over the internet, so a stranger can spend it.
//
// Measured in this project: an unresolved single-label service name comes back
// as 46.225.53.182 rather than NXDOMAIN.
func TestCheckHubProofTarget(t *testing.T) {
	// What the operator named outright is their own call, and no lookup happens
	// for these - so this half does not depend on DNS.
	for _, ok := range []string{
		"http://10.0.0.5:25530",
		"http://203.0.113.7:25530",
		"http://hub.example.com:25530",
		"https://gateway.dylaris.com/",
	} {
		if err := checkHubProofTarget(ok); err != nil {
			t.Errorf("checkHubProofTarget(%q) = %v, want nil", ok, err)
		}
	}

	// localhost is the one single-label name that resolves everywhere, and it
	// resolves private.
	if err := checkHubProofTarget("http://localhost:25530"); err != nil {
		t.Errorf("localhost was refused: %v", err)
	}

	// A single-label name that is not a service is refused, whichever way the
	// resolver chooses to be unhelpful: NXDOMAIN or a wildcard answer. Both are
	// refusals, and which one happens is a property of the resolver.
	if err := checkHubProofTarget("http://hub-that-does-not-exist-anywhere:25530"); err == nil {
		t.Error("accepted a single-label name that is not a service")
	}

	for _, bad := range []string{"", "   "} {
		if err := checkHubProofTarget(bad); err == nil {
			t.Errorf("checkHubProofTarget(%q) = nil, want an error", bad)
		}
	}
}

func TestHubAddrIsPrivateOrReserved(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"10.0.0.5", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"127.0.0.1", true},
		{"169.254.1.1", true},
		{"100.64.0.1", true}, // CGNAT, which net.IP.IsPrivate does not cover
		{"::1", true},
		{"46.225.53.182", false}, // the address the wildcard actually answered with
		{"8.8.8.8", false},
		{"100.63.255.255", false},
		{"100.128.0.0", false},
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("bad test address %q", tt.ip)
			}
			if got := hubAddrIsPrivateOrReserved(ip); got != tt.want {
				t.Errorf("hubAddrIsPrivateOrReserved(%s) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
	if !hubAddrIsPrivateOrReserved(nil) {
		t.Error("a nil address must not read as public")
	}
}
