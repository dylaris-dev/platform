package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"dylaris-core/handlers/audit"
	"dylaris-core/handlers/httpx"
	"dylaris-core/services"
	"dylaris-core/services/remotegw"
)

// GatewayProviderHandler serves the gateway provider choice: this platform's
// own gateway, or the managed one of another DYLARIS Core over its API.
// The provider key is write-only: no answer here ever carries it, and a
// stored or env key is only ever sent to the URL in force.
type GatewayProviderHandler struct {
	d Deps
	// limits buckets the calls per admin (keyed by user id, not by IP).
	limits *httpx.IPRateLimiter
	now    func() time.Time

	// The GET's account answer, for the URL+key it was asked with.
	acctMu  sync.Mutex
	acctFor string
	acct    remotegw.Account
	acctErr error
	acctExp time.Time
}

func NewGatewayProviderHandler(d Deps) *GatewayProviderHandler {
	return &GatewayProviderHandler{d: d, limits: httpx.NewIPRateLimiter(), now: time.Now}
}

const (
	// Save and Test share one bucket: both can make a provider call.
	providerWritesPerMinute = 10
	providerReadsPerMinute  = 60
	providerCallTimeout     = 15 * time.Second
	providerAccountTTL      = 30 * time.Second
	// A failure is kept too, shorter, so a broken key does not hammer the
	// provider from every open settings page.
	providerAccountFailTTL = 10 * time.Second
)

type providerFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type providerOutbox struct {
	Open     int        `json:"open"`
	OldestAt *time.Time `json:"oldestAt,omitempty"`
	// Refused: open changes the provider refused, newest first, at most
	// providerRefusedMax. Error is the stored answer, which the client already
	// stripped of the key.
	Refused []providerRefusal `json:"refused"`
}

type providerRefusal struct {
	Op     string `json:"op"`
	Domain string `json:"domain"`
	Error  string `json:"error"`
}

const providerRefusedMax = 5

type providerView struct {
	Success    bool   `json:"success"`
	Mode       string `json:"mode"`
	URL        string `json:"url"`
	KeySet     bool   `json:"keySet"`
	KeyFromEnv bool   `json:"keyFromEnv"`
	URLFromEnv bool   `json:"urlFromEnv"`
	// KeyUnreadable: a key is stored but cannot be decrypted (CLUSTER_SECRET
	// changed); it reads as no key, so remote mode is off until it is re-entered.
	KeyUnreadable bool `json:"keyUnreadable,omitempty"`
	// RoutingModeOK: the routing mode is gateway or both. In ip_port no server
	// gets a gateway address, so every route create is refused.
	RoutingModeOK bool              `json:"routingModeOk"`
	Outbox        providerOutbox    `json:"outbox"`
	Account       *remotegw.Account `json:"account,omitempty"`
	Error         *providerFailure  `json:"error,omitempty"`
}

// providerFailureOf turns a provider call error into what the panel shows.
// Never err.Error(): a transport error carries the request URL.
func providerFailureOf(err error) *providerFailure {
	var ae *remotegw.APIError
	switch {
	case errors.Is(err, remotegw.ErrBaseURL):
		return &providerFailure{"invalid_url", "The provider URL must start with https://."}
	case errors.Is(err, remotegw.ErrNoKey):
		return &providerFailure{"no_key", "No provider API key is set."}
	case errors.As(err, &ae):
		switch ae.Status {
		case http.StatusUnauthorized:
			return &providerFailure{"unauthorized", "The provider rejected this key."}
		case http.StatusForbidden:
			return &providerFailure{"forbidden", "The provider refused this key. The account may be suspended or the key lacks the gateway permission."}
		case http.StatusTooManyRequests:
			return &providerFailure{"rate_limited", "The provider is limiting requests from this key. Try again in a minute."}
		}
		// The provider's own message, written for end users; the client
		// already redacted the key from it.
		return &providerFailure{"provider_error", ae.Message}
	case errors.Is(err, remotegw.ErrBadAnswer):
		return &providerFailure{"provider_error", "The provider sent an answer that could not be read."}
	}
	return &providerFailure{"unreachable", "The provider could not be reached."}
}

// accountProblem is why a reachable account cannot run this platform's gateway.
func accountProblem(a remotegw.Account) *providerFailure {
	switch {
	case !a.Managed:
		return &providerFailure{"not_managed", "This provider account has no managed gateway plan."}
	case a.Suspended:
		return &providerFailure{"account_suspended", "This provider account is suspended."}
	case !slices.Contains(a.Scopes, "gateway.write"):
		return &providerFailure{"missing_scope", "This key lacks the gateway.write permission."}
	}
	return nil
}

// allowLoopbackHTTP admits the client's loopback http exception for a saved
// or tested URL. Only under go test (the fake provider is plain http); config
// has no dev switch for it, and an operator must not be able to point the key
// at a plain-text listener.
var allowLoopbackHTTP = testing.Testing()

// validProviderURL is the client's rule, https only (see allowLoopbackHTTP),
// plus a bare origin: the client appends the API path, and a query or
// userinfo has no business in it.
func validProviderURL(raw string) bool {
	if remotegw.CheckBaseURL(raw) != nil {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && !allowLoopbackHTTP) {
		return false
	}
	return u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && (u.Path == "" || u.Path == "/")
}

// normURL is how two provider URLs are compared.
func normURL(s string) string {
	return strings.TrimSuffix(strings.TrimSpace(s), "/")
}

func (h *GatewayProviderHandler) limited(w http.ResponseWriter, r *http.Request, bucket string, perMin int) bool {
	userID, _ := r.Context().Value("userID").(string)
	if h.limits.Allow(bucket+":"+userID, perMin) {
		return false
	}
	w.Header().Set("Retry-After", "60")
	httpx.SendJSONErrorCode(w, "Too many requests, try again in a minute.", "rate_limited", http.StatusTooManyRequests)
	return true
}

func (h *GatewayProviderHandler) account(ctx context.Context, base, key string) (remotegw.Account, error) {
	ctx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	defer cancel()
	return remotegw.New(base, func() string { return key }).Account(ctx)
}

// cachedAccount is account() for the GET: 30 s for an answer, 10 s for a
// failure, per URL+key. The lock is held across the call so concurrent page
// loads make one request.
func (h *GatewayProviderHandler) cachedAccount(ctx context.Context, base, key string) (remotegw.Account, error) {
	h.acctMu.Lock()
	defer h.acctMu.Unlock()
	if h.acctFor == base+"\n"+key && h.now().Before(h.acctExp) {
		return h.acct, h.acctErr
	}
	a, err := h.account(ctx, base, key)
	ttl := providerAccountTTL
	if err != nil {
		ttl = providerAccountFailTTL
	}
	h.acctFor, h.acct, h.acctErr, h.acctExp = base+"\n"+key, a, err, h.now().Add(ttl)
	return a, err
}

func (h *GatewayProviderHandler) dropAccountCache() {
	h.acctMu.Lock()
	h.acctExp = time.Time{}
	h.acctMu.Unlock()
}

func (h *GatewayProviderHandler) view(ctx context.Context) (providerView, error) {
	pc, err := h.d.GatewayProviderConfig()
	if err != nil {
		return providerView{}, err
	}
	v := providerView{Success: true, Mode: "local", URL: pc.URL, KeySet: pc.Key != "", KeyFromEnv: pc.KeyFromEnv, URLFromEnv: pc.URLFromEnv,
		RoutingModeOK: services.GatewayRoutingEnabled(h.d.Store)}
	if pc.Remote {
		v.Mode = "remote"
	}
	if !v.KeySet {
		// A cleared key has no row (Save deletes it); a row that reads as ""
		// is a sealed key GetSetting could not open.
		if _, err := h.d.Store.GetSetting(services.SettingGatewayProviderKey); err == nil {
			v.KeyUnreadable = true
		}
	}
	open, oldest, err := h.d.Store.CountOpenOutbox()
	if err != nil {
		return providerView{}, fmt.Errorf("count gateway outbox: %w", err)
	}
	v.Outbox.Open = open
	if open > 0 && !oldest.IsZero() {
		v.Outbox.OldestAt = &oldest
	}
	refused, err := h.d.Store.ListRefusedOutbox(providerRefusedMax)
	if err != nil {
		return providerView{}, fmt.Errorf("list refused gateway outbox: %w", err)
	}
	v.Outbox.Refused = make([]providerRefusal, 0, len(refused))
	for _, it := range refused {
		v.Outbox.Refused = append(v.Outbox.Refused, providerRefusal{Op: it.Op, Domain: it.Domain, Error: it.LastError})
	}
	// Only with a key: a platform that never set one makes no outbound call.
	if v.KeySet {
		if a, err := h.cachedAccount(ctx, pc.URL, pc.Key); err != nil {
			v.Error = providerFailureOf(err)
		} else {
			v.Account = &a
		}
	}
	return v, nil
}

func (h *GatewayProviderHandler) writeView(w http.ResponseWriter, r *http.Request) {
	v, err := h.view(r.Context())
	if err != nil {
		log.Printf("gateway provider settings: %v", err)
		httpx.SendJSONError(w, "Failed to read the gateway provider settings", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(v)
}

// Get GET /api/settings/gateway/provider returns the gateway provider mode,
// URL, whether a key is set (never the key), whether the routing mode lets the
// managed gateway hand out addresses (routingModeOk), the outbox backlog with
// up to five changes the provider refused and, with a key, the provider
// account. settings.read at the route.
func (h *GatewayProviderHandler) Get(w http.ResponseWriter, r *http.Request) {
	if h.limited(w, r, "provider-read", providerReadsPerMinute) {
		return
	}
	h.writeView(w, r)
}

type providerSaveRequest struct {
	Mode string  `json:"mode"`
	URL  *string `json:"url"`
	Key  *string `json:"key"`
}

const (
	invalidURLMessage  = "The provider URL must be https:// with no path, query or credentials."
	envKeyURLMessage   = "The provider key is set by GATEWAY_PROVIDER_KEY and is only sent to the provider address in force."
	routingModeMessage = "Switch the routing mode to Gateway or Both first. Servers get their addresses from the gateway only in those modes."
)

// Save POST /api/settings/gateway/provider saves the gateway provider mode,
// URL and key. settings.write at the route. An omitted url or key keeps the
// stored one; "" clears it. A new address needs the key sent with it, so a
// stored key never travels to a host it was not entered for. Switching to
// remote is refused with 409 routing_mode while the routing mode is IP:Port,
// and is checked against the provider first. Switching to local, or
// clearing the key, deletes nothing: the local route rows stay for a switch
// back.
func (h *GatewayProviderHandler) Save(w http.ResponseWriter, r *http.Request) {
	if h.limited(w, r, "provider-write", providerWritesPerMinute) {
		return
	}
	var req providerSaveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Mode != "local" && req.Mode != "remote" {
		httpx.SendJSONErrorCode(w, "mode must be local or remote", "invalid_mode", http.StatusBadRequest)
		return
	}
	pc, err := h.d.GatewayProviderConfig()
	if err != nil {
		log.Printf("gateway provider settings: %v", err)
		httpx.SendJSONError(w, "Failed to read the gateway provider settings", http.StatusInternalServerError)
		return
	}
	if req.Key != nil && pc.KeyFromEnv {
		httpx.SendJSONErrorCode(w, "The provider key is set by GATEWAY_PROVIDER_KEY and cannot be changed here.", "key_from_env", http.StatusConflict)
		return
	}
	if req.URL != nil && pc.URLFromEnv {
		httpx.SendJSONErrorCode(w, "The provider URL is set by GATEWAY_PROVIDER_URL and cannot be changed here.", "url_from_env", http.StatusConflict)
		return
	}
	base, key := pc.URL, pc.Key
	urlIn := ""
	if req.URL != nil {
		urlIn = normURL(*req.URL)
		if urlIn != "" && !validProviderURL(urlIn) {
			httpx.SendJSONErrorCode(w, invalidURLMessage, "invalid_url", http.StatusBadRequest)
			return
		}
		base = urlIn
		if base == "" {
			base = services.DefaultGatewayProviderURL
		}
	}
	if req.Key != nil {
		key = strings.TrimSpace(*req.Key)
	}
	if normURL(base) != normURL(pc.URL) && req.Key == nil {
		switch {
		case pc.KeyFromEnv:
			httpx.SendJSONErrorCode(w, envKeyURLMessage, "key_from_env", http.StatusConflict)
			return
		case pc.Key != "":
			httpx.SendJSONErrorCode(w, "Enter the key again to change the provider address.", "no_key", http.StatusBadRequest)
			return
		}
	}

	ctx := r.Context()
	if req.Mode == "remote" && !pc.Remote && !services.GatewayRoutingEnabled(h.d.Store) {
		// Not switched for the operator: changing the routing mode redeploys
		// every server.
		httpx.SendJSONErrorCode(w, routingModeMessage, "routing_mode", http.StatusConflict)
		return
	}
	if req.Mode == "remote" {
		// The stored flags, not the getters: those already read off in remote
		// mode. Tenants' kits and nodes are local gateway state the managed
		// gateway cannot carry.
		if h.d.FeatureFlags != nil && h.d.FeatureFlags.RouteOnlyOwnershipInForce(ctx) {
			httpx.SendJSONErrorCode(w, "Turn off BYON and route-only before switching to the managed gateway.", "tenancy_enabled", http.StatusConflict)
			return
		}
		if key == "" {
			httpx.SendJSONErrorCode(w, "Enter the provider API key before switching to the managed gateway.", "no_key", http.StatusBadRequest)
			return
		}
		a, err := h.account(ctx, base, key)
		if err != nil {
			f := providerFailureOf(err)
			httpx.SendJSONErrorCode(w, f.Message, f.Code, http.StatusBadRequest)
			return
		}
		if f := accountProblem(a); f != nil {
			httpx.SendJSONErrorCode(w, f.Message, f.Code, http.StatusBadRequest)
			return
		}
	}

	// "" deletes the row, so a cleared key is told apart from one that no
	// longer decrypts (see view).
	save := func(k, v string) error {
		if v == "" {
			return h.d.Store.DeleteSetting(k)
		}
		return h.d.Store.SetSetting(k, v)
	}
	// Three separate writes: the store has no transaction across settings
	// keys, so a failure can leave the earlier ones saved. The order picks
	// the safe leftover: the key first, so a new URL is never saved beside a
	// key that was not entered for it; the mode last, so a failed write never
	// turns remote mode on.
	if req.Key != nil {
		err = save(services.SettingGatewayProviderKey, key)
	}
	if err == nil && req.URL != nil {
		err = save(services.SettingGatewayProviderURL, urlIn)
	}
	if err == nil {
		err = h.d.Store.SetSetting(services.SettingGatewayProvider, req.Mode)
	}
	if err != nil {
		log.Printf("gateway provider settings: save: %v", err)
		httpx.SendJSONError(w, "Failed to save the gateway provider settings", http.StatusInternalServerError)
		return
	}
	if h.d.ResetGatewayRemote != nil {
		h.d.ResetGatewayRemote()
	}
	h.dropAccountCache()
	// Open tabs reload /api/system/features, which carries the mode.
	h.d.Events.Publish(ctx, "features.changed", map[string]interface{}{
		"feature": "gatewayProvider",
		"enabled": req.Mode == "remote",
	})

	actorID, _ := ctx.Value("userID").(string)
	meta := map[string]interface{}{"mode": req.Mode}
	if req.URL != nil {
		meta["url"] = base
	}
	if req.Key != nil {
		meta["keyChanged"] = key != ""
		meta["keyCleared"] = key == ""
	}
	audit.LogIdentity(h.d.Store, r, audit.AuditEventGatewayProviderChanged, actorID, "", meta)

	h.writeView(w, r)
}

type providerTestRequest struct {
	URL *string `json:"url"`
	Key *string `json:"key"`
}

type providerTestResult struct {
	Success bool              `json:"success"`
	OK      bool              `json:"ok"`
	Account *remotegw.Account `json:"account,omitempty"`
	Error   *providerFailure  `json:"error,omitempty"`
}

// Test POST /api/settings/gateway/provider/test checks a provider URL and key
// against the provider's account. settings.write at the route.
//
// Persists nothing. A url or key in the request is tried instead of the one in
// force, but the stored or env key only goes to the URL in force: another
// address needs the key typed in. ok means the account could run this
// platform's gateway.
func (h *GatewayProviderHandler) Test(w http.ResponseWriter, r *http.Request) {
	if h.limited(w, r, "provider-write", providerWritesPerMinute) {
		return
	}
	var req providerTestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	pc, err := h.d.GatewayProviderConfig()
	if err != nil {
		log.Printf("gateway provider settings: %v", err)
		httpx.SendJSONError(w, "Failed to read the gateway provider settings", http.StatusInternalServerError)
		return
	}
	base, key := pc.URL, pc.Key
	if req.URL != nil {
		if u := normURL(*req.URL); u != "" {
			if !validProviderURL(u) {
				httpx.SendJSONErrorCode(w, invalidURLMessage, "invalid_url", http.StatusBadRequest)
				return
			}
			base = u
		}
	}
	keyGiven := req.Key != nil && strings.TrimSpace(*req.Key) != ""
	if keyGiven {
		key = strings.TrimSpace(*req.Key)
	} else if normURL(base) != normURL(pc.URL) {
		if pc.KeyFromEnv {
			httpx.SendJSONErrorCode(w, envKeyURLMessage, "key_from_env", http.StatusConflict)
			return
		}
		httpx.SendJSONErrorCode(w, "Enter the key again to test a different provider address.", "no_key", http.StatusBadRequest)
		return
	} else {
		// A test of the key in force replaces what the GET shows.
		h.dropAccountCache()
	}

	res := providerTestResult{Success: true}
	if key == "" {
		res.Error = providerFailureOf(remotegw.ErrNoKey)
	} else if a, err := h.account(r.Context(), base, key); err != nil {
		res.Error = providerFailureOf(err)
	} else {
		res.Account = &a
		res.Error = accountProblem(a)
		res.OK = res.Error == nil
	}
	json.NewEncoder(w).Encode(res)
}
