package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"dylaris-core/handlers"
)

// The DNS credential is entered in the panel and STORED IN THE GATEWAY HUB.
//
// Core keeps nothing. It forwards the form and reports what came back. That is
// deliberate and not laziness: the Hub's reconciler creates missing records and
// DELETES stale ones, and its "an edge name wins over a relay name" guard only
// holds inside one plan. Two components each holding a copy of the credential is
// one short step from two components each writing records, which shows up as an
// address that flaps and reads like a provider fault for weeks.
//
// So the row lives in exactly one database, in the Hub, and this file is a pipe.

// dnsForwardTimeout bounds the hop. The Hub only touches its own Postgres here -
// no provider call - so a slow answer means the Hub is unwell, and a panel that
// hangs on it is worse than one that says so.
const dnsForwardTimeout = 8 * time.Second

// hubProofPrincipal is the name Core signs its requests to the Hub under.
//
// It is part of the signed message, not an authorization: anything holding
// CLUSTER_SECRET can sign any principal. The real bar for this endpoint is
// possession of that secret, which is why the read direction never returns the
// token - overwriting is recoverable by retyping, exfiltration is not.
const hubProofPrincipal = "core"

// GatewayDNSHandler forwards the panel's DNS settings to the gateway Hub.
type GatewayDNSHandler struct {
	state *handlers.AppState
}

func NewGatewayDNSHandler(state *handlers.AppState) *GatewayDNSHandler {
	return &GatewayDNSHandler{state: state}
}

// dnsConfigPayload is the Hub's answer. Note the absence of a token field: the
// Hub does not send one, and this struct not having one is what guarantees it
// could not be forwarded on by accident.
type dnsConfigPayload struct {
	Provider  string   `json:"provider"`
	Zones     []string `json:"zones"`
	Enabled   bool     `json:"enabled"`
	HasToken  bool     `json:"has_token"`
	EnvLocked bool     `json:"env_locked"`
	Providers []struct {
		Name  string `json:"name"`
		Label string `json:"label"`
	} `json:"providers"`

	// The certificate half. It shares the credential above, so it shares the
	// form: a combined install configures both here and never opens the Hub's
	// own interface, which is off by default.
	AcmeEnabled   bool   `json:"acme_enabled"`
	AcmeEmail     string `json:"acme_email"`
	AcmeDirectory string `json:"acme_directory"`
	AcmeAgreed    bool   `json:"acme_agreed"`
	// CertStatus is passed through as-is. Its shape belongs to the gateway, and
	// re-declaring it here would be a second definition to keep in step for no
	// gain - Core only relays it.
	CertStatus json.RawMessage `json:"cert_status,omitempty"`
}

// Get GET /api/settings/gateway/dns - PANEL settings.read.
func (h *GatewayDNSHandler) Get(w http.ResponseWriter, r *http.Request) {
	h.forward(w, r, map[string]any{"action": "read"})
}

// Save PUT /api/settings/gateway/dns - PANEL settings.write.
//
// A blank token means "keep the stored one", decided by the Hub rather than
// here: the field is write-only in the UI, so an admin editing a zone must not
// silently erase the credential. Keeping that rule in one place means this
// forwarder cannot drift from it.
func (h *GatewayDNSHandler) Save(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var req struct {
		Provider      string   `json:"provider"`
		Token         string   `json:"token"`
		Zones         []string `json:"zones"`
		Enabled       bool     `json:"enabled"`
		AcmeEnabled   bool     `json:"acme_enabled"`
		AcmeEmail     string   `json:"acme_email"`
		AcmeDirectory string   `json:"acme_directory"`
		AcmeAgreed    bool     `json:"acme_agreed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		dnsError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	h.forward(w, r, map[string]any{
		"action":         "write",
		"provider":       strings.TrimSpace(req.Provider),
		"token":          req.Token,
		"zones":          req.Zones,
		"enabled":        req.Enabled,
		"acme_enabled":   req.AcmeEnabled,
		"acme_email":     strings.TrimSpace(req.AcmeEmail),
		"acme_directory": strings.TrimSpace(req.AcmeDirectory),
		"acme_agreed":    req.AcmeAgreed,
	})
}

// Probe POST /api/settings/gateway/dns/probe - PANEL settings.write.
//
// Asks the Hub to try the credential and report which zones it can see. It
// stores nothing, which is the point: the credential is the one part of a DNS
// configuration that cannot be checked by reading it back, and the only way to
// find out whether it worked used to be to save it, switch record writing on,
// and wait for a reconciler tick that either wrote something or logged a failure
// nobody was watching.
//
// settings.write rather than settings.read because it sends an
// operator-supplied credential to a third-party API.
func (h *GatewayDNSHandler) Probe(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var req struct {
		Provider string `json:"provider"`
		// Empty means "use the stored one", so a re-check does not need a secret
		// that no screen ever shows back.
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		dnsError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	raw, ok := h.callHub(w, r, map[string]any{
		"action":   "probe",
		"provider": strings.TrimSpace(req.Provider),
		"token":    req.Token,
	})
	if !ok {
		return
	}

	// The verdict's shape belongs to the gateway, so it is relayed rather than
	// re-declared here - the same reason cert_status is a json.RawMessage.
	var probe json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		dnsError(w, http.StatusBadGateway, "the gateway hub's answer could not be read")
		return
	}
	dnsJSON(w, http.StatusOK, map[string]any{
		"success":   true,
		"available": true,
		"probe":     probe,
	})
}

func (h *GatewayDNSHandler) forward(w http.ResponseWriter, r *http.Request, body map[string]any) {
	raw, ok := h.callHub(w, r, body)
	if !ok {
		return
	}
	var cfg dnsConfigPayload
	if err := json.Unmarshal(raw, &cfg); err != nil {
		dnsError(w, http.StatusBadGateway, "the gateway hub's answer could not be read")
		return
	}
	dnsJSON(w, http.StatusOK, map[string]any{
		"success":   true,
		"available": true,
		"config":    cfg,
	})
}

// callHub signs and posts one request to the Hub and returns its body.
//
// It writes the response itself on every failure - including the
// no-gateway-configured case, which is a successful "not available" rather than
// an error - and reports ok=false so the caller simply returns. Split out of
// forward() when the probe needed the same transport with a different answer
// shape; there is one place that knows how to talk to the Hub.
func (h *GatewayDNSHandler) callHub(w http.ResponseWriter, r *http.Request, body map[string]any) ([]byte, bool) {
	hubURL := strings.TrimSpace(h.state.GatewayHubURL)
	if hubURL == "" {
		// Not an error. A platform-only install has no gateway and no records to
		// write, and the screen renders that state instead of a form that cannot
		// work.
		dnsJSON(w, http.StatusOK, map[string]any{
			"success":   true,
			"available": false,
		})
		return nil, false
	}
	if h.state.ClusterSecret == "" {
		dnsError(w, http.StatusServiceUnavailable, "CLUSTER_SECRET is not configured")
		return nil, false
	}

	// Before anything is signed. The proof is a one-way HMAC over
	// (principal, ts) and nothing in the Hub's reply is bound to the cluster
	// secret, so whoever answers holds a working credential for the whole skew
	// window - and this payload carries the operator's DNS token with it. The
	// Hub serves /internal/* on the same listener standalone customers reach
	// over the internet, so a stranger can spend what they were handed.
	if err := checkHubProofTarget(hubURL); err != nil {
		dnsError(w, http.StatusBadGateway, "not sending anything to the configured gateway hub: "+err.Error())
		return nil, false
	}

	ts := time.Now().Unix()
	body["principal"] = hubProofPrincipal
	body["ts"] = ts
	body["proof"] = hubProof(h.state.ClusterSecret, hubProofPrincipal, ts)

	payload, err := json.Marshal(body)
	if err != nil {
		dnsError(w, http.StatusInternalServerError, "could not build the request")
		return nil, false
	}

	ctx, cancel := context.WithTimeout(r.Context(), dnsForwardTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(hubURL, "/")+"/internal/dns-config", bytes.NewReader(payload))
	if err != nil {
		dnsError(w, http.StatusInternalServerError, "could not build the request")
		return nil, false
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		dnsError(w, http.StatusBadGateway, "the gateway hub did not answer")
		return nil, false
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		dnsError(w, http.StatusBadGateway, "the gateway hub's answer could not be read")
		return nil, false
	}

	if resp.StatusCode != http.StatusOK {
		// The Hub's rejections are written for the person at the form ("name at
		// least one zone"), so they are passed through rather than flattened.
		// Its 401 is not: that is a cluster-secret mismatch between the two
		// services, which is an operator problem and needs to say so.
		msg := strings.TrimSpace(string(raw))
		if resp.StatusCode == http.StatusUnauthorized {
			msg = "the gateway hub rejected this platform's CLUSTER_SECRET - the two must match"
		}
		if msg == "" {
			msg = fmt.Sprintf("the gateway hub returned %d", resp.StatusCode)
		}
		dnsError(w, http.StatusBadRequest, msg)
		return nil, false
	}

	return raw, true
}

// hubProof reproduces the gateway's proof-of-possession:
// hex(HMAC-SHA256(CLUSTER_SECRET, principal + ":" + ts)).
//
// FROZEN CROSS-REPO CONTRACT. The other side is
// `gateway/pkg/redisacl.Proof` / `VerifyProof`. It is duplicated rather than
// shared because the two repositories build independently, exactly like the beam
// stream header - and like that header it must not change on one side alone. The
// secret itself never travels.
// checkHubProofTarget refuses to send a signed proof - and, on the write path,
// the operator's DNS credential - to a host that cannot be the gateway Hub.
//
// The rule is narrow on purpose, and identical to the gateway's own
// redisacl.CheckProofTarget (repeated rather than shared: the two live in
// separate repositories):
//
//   - An IP literal or a dotted name is what the operator configured. Their call.
//   - A SINGLE-LABEL name ("hub") is a container or service name by
//     construction. No such name exists in public DNS, so an answer outside
//     private address space is a lie - measured in this project, an unresolved
//     service name came back as 46.225.53.182 rather than NXDOMAIN.
//
// A name that does not resolve is refused too: there is nothing to send to.
func checkHubProofTarget(rawURL string) error {
	host := rawURL
	if u, err := url.Parse(strings.TrimSpace(rawURL)); err == nil && u.Host != "" {
		host = u.Hostname()
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("no host in the configured hub URL")
	}
	if net.ParseIP(host) != nil || strings.Contains(host, ".") {
		return nil
	}
	addrs, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("%q does not resolve", host)
	}
	for _, ip := range addrs {
		if !hubAddrIsPrivateOrReserved(ip) {
			return fmt.Errorf("%q resolved to the public address %s, which a single-label name cannot legitimately do", host, ip)
		}
	}
	return nil
}

func hubAddrIsPrivateOrReserved(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	// 100.64.0.0/10 (CGNAT, RFC 6598) is not covered by IsPrivate.
	if len(ip) == net.IPv4len && ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127 {
		return true
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

func hubProof(clusterSecret, principal string, ts int64) string {
	mac := hmac.New(sha256.New, []byte(clusterSecret))
	mac.Write([]byte(principal + ":" + strconv.FormatInt(ts, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

func dnsError(w http.ResponseWriter, code int, msg string) {
	dnsJSON(w, code, map[string]any{"success": false, "message": msg})
}

func dnsJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
