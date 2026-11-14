// Package remotegw is the typed client for the managed gateway provider API
// (/api/external/gateway/*) that a self-hosted Core uses when our Core is its
// gateway provider, plus an in-memory fake of that API for tests.
package remotegw

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const apiPrefix = "/api/external/gateway"

// ErrBaseURL marks a base URL the client refuses to talk to. Retrying cannot
// fix it.
var ErrBaseURL = errors.New("remotegw: base URL must be https (http only for a loopback host)")

// ErrNoKey marks a call made with no provider key. Nothing is sent: an empty
// bearer would only earn a 401 and count against the provider's limits.
var ErrNoKey = errors.New("remotegw: no gateway provider key configured")

// ErrBadSegment marks a domain or link id that cannot be a path segment.
var ErrBadSegment = errors.New("remotegw: invalid path segment")

// ErrUnreachable marks a call that got no answer from the provider.
var ErrUnreachable = errors.New("gateway provider unreachable")

// ErrBadAnswer marks an answer that arrived but could not be read or parsed.
var ErrBadAnswer = errors.New("gateway provider answer unreadable")

// APIError is a refusal from the provider. Code is always set: the handler's
// own code when it sent one, else derived from the status (the auth layer
// answers without a code).
type APIError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gateway provider answered %d %s: %s", e.Status, e.Code, e.Message)
}

// IsCode reports whether err is an APIError with that code.
func IsCode(err error, code string) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == code
}

// Retryable is true for network errors, 429 and 5xx, never for another 4xx:
// those would fail the same way again.
func Retryable(err error) bool {
	if err == nil || errors.Is(err, ErrBaseURL) || errors.Is(err, ErrNoKey) || errors.Is(err, ErrBadSegment) || errors.Is(err, context.Canceled) {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == http.StatusTooManyRequests || ae.Status >= 500
	}
	return true
}

type Client struct {
	base    string
	baseErr error
	key     func() string
	http    *http.Client
}

// New builds a client. key is read on every request, so a key saved in the
// panel applies at once. A base URL that is not https (loopback excepted, for
// tests and dev) is refused by every call.
func New(baseURL string, key func() string) *Client {
	c := &Client{base: strings.TrimRight(baseURL, "/"), key: key, http: &http.Client{
		Timeout: 10 * time.Second,
		// A redirect must never carry the Authorization header to another host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	c.baseErr = CheckBaseURL(c.base)
	return c
}

// CheckBaseURL is the base URL rule every call enforces: https, or http for a
// loopback host. ErrBaseURL when it does not hold.
func CheckBaseURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "":
		return ErrBaseURL
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopback(u.Hostname()):
	default:
		return ErrBaseURL
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type Account struct {
	UserID            string        `json:"userId"`
	Product           string        `json:"product"`
	Managed           bool          `json:"managed"`
	EntitlementSource string        `json:"entitlementSource"`
	Suspended         bool          `json:"suspended"`
	Scopes            []string      `json:"scopes"`
	Limits            AccountLimits `json:"limits"`
}

// AccountLimits follows the platform convention: nil = no cap, 0 = none.
type AccountLimits struct {
	MaxLinks         *int64 `json:"maxLinks"`
	MaxRoutes        *int64 `json:"maxRoutes"`
	MaxCustomDomains *int64 `json:"maxCustomDomains"`
}

// Kit is one link kit. Online is nil when the provider could not tell.
// RetireAt is set on a retired kit: the provider revokes it then.
type Kit struct {
	ID       int        `json:"id"`
	Name     string     `json:"name"`
	LinkID   string     `json:"link_id"`
	Region   string     `json:"region"`
	Online   *bool      `json:"online"`
	Caps     []string   `json:"caps"`
	RetireAt *time.Time `json:"retire_at,omitempty"`
}

type MintedKit struct {
	LinkID  string `json:"link_id"`
	LinkKey string `json:"link_key"`
}

type Route struct {
	Domain        string `json:"domain"`
	TargetHost    string `json:"target_ip"`
	TargetPort    int    `json:"target_port"`
	LinkID        string `json:"link_id"`
	ProxyProtocol bool   `json:"proxy_protocol"`
	Suspended     bool   `json:"suspended"`
	// Voice and Bedrock are nil when the provider did not say: an older one
	// lists neither, and that must not read as "voice off" or "Java only".
	Voice   *RouteVoice   `json:"voice"`
	Bedrock *RouteBedrock `json:"bedrock"`
}

type HosterDomain struct {
	Domain     string `json:"domain"`
	Validation string `json:"validation"`
}

type Region struct {
	Region   string `json:"region"`
	Wildcard string `json:"wildcard"`
	Online   bool   `json:"online"`
}

type Regions struct {
	HosterDomains        []HosterDomain `json:"hosterDomains"`
	CustomDomainsEnabled bool           `json:"customDomainsEnabled"`
	CNAMETarget          string         `json:"cnameTarget"`
	Items                []Region       `json:"items"`
}

// Pool is a managed plan's allowance for the running month.
type Pool struct {
	IncludedGB    int64  `json:"includedGb"`
	MaxPurchaseGB *int64 `json:"maxPurchaseGb"`
	UsedBytes     int64  `json:"usedBytes"`
	Percent       int64  `json:"percent"`
}

type Usage struct {
	Period     string
	EdgeBytes  int64
	RelayBytes int64
	// Final is false while the month runs: the figures are an estimate.
	Final bool
	// Pool is nil unless the owner has a managed plan and the month is running.
	Pool *Pool
}

// do sends one request and decodes a 2xx body into out (nil = ignore it).
func (c *Client) do(ctx context.Context, method, path string, body, out interface{}) error {
	if c.baseErr != nil {
		return c.baseErr
	}
	key := c.key()
	if key == "" {
		return ErrNoKey
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("remotegw: encode request: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+apiPrefix+path, rd)
	if err != nil {
		return fmt.Errorf("remotegw: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", "dylaris-core")
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil { // the caller's cancel or deadline, not a provider fault
			return ctx.Err()
		}
		return fmt.Errorf("%w: %s", ErrUnreachable, redact(err.Error(), key))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %s", ErrBadAnswer, redact(err.Error(), key))
	}
	if resp.StatusCode >= 300 {
		return parseAPIError(resp, raw, key)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: not the expected JSON: %w", ErrBadAnswer, err)
	}
	return nil
}

func redact(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "[redacted]")
}

// parseAPIError reads both error shapes: the auth layer's {message} without a
// code, and the handlers' {error, code}.
func parseAPIError(resp *http.Response, raw []byte, key string) *APIError {
	ae := &APIError{Status: resp.StatusCode}
	var b struct {
		Message string `json:"message"`
		Error   string `json:"error"`
		Code    string `json:"code"`
	}
	if json.Unmarshal(raw, &b) == nil {
		ae.Code = b.Code
		ae.Message = b.Message
		if ae.Message == "" {
			ae.Message = b.Error
		}
	}
	if ae.Message == "" {
		ae.Message = http.StatusText(resp.StatusCode)
	}
	ae.Message = redact(ae.Message, key)
	if ae.Code == "" {
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			ae.Code = "unauthorized"
		case http.StatusForbidden:
			ae.Code = "forbidden"
		case http.StatusTooManyRequests:
			ae.Code = "rate_limited"
		default:
			ae.Code = "http_" + strconv.Itoa(resp.StatusCode)
		}
	}
	if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s > 0 {
		ae.RetryAfter = time.Duration(s) * time.Second
	}
	return ae
}

func (c *Client) Account(ctx context.Context) (Account, error) {
	var out struct {
		Account Account `json:"account"`
	}
	err := c.do(ctx, http.MethodGet, "/account", nil, &out)
	return out.Account, err
}

func (c *Client) Kits(ctx context.Context) ([]Kit, error) {
	var out struct {
		Items []Kit `json:"items"`
	}
	err := c.do(ctx, http.MethodGet, "/link-kits", nil, &out)
	return out.Items, err
}

func (c *Client) MintKit(ctx context.Context, name, region string) (MintedKit, error) {
	var out MintedKit
	err := c.do(ctx, http.MethodPost, "/link-kits", map[string]string{"name": name, "region": region}, &out)
	return out, err
}

// segment escapes one path segment and refuses the ones that would climb out
// of the endpoint ("." and "..") or collapse into the list route (empty).
func segment(s string) (string, error) {
	if s == "" || s == "." || s == ".." {
		return "", fmt.Errorf("%w: %q", ErrBadSegment, s)
	}
	return url.PathEscape(s), nil
}

func (c *Client) RevokeKit(ctx context.Context, linkID string) error {
	seg, err := segment(linkID)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, "/link-kits/"+seg, nil, nil)
}

// RetireKit retires a kit: it stops counting toward the link limit at once and
// the provider revokes it at the returned time.
func (c *Client) RetireKit(ctx context.Context, linkID string) (time.Time, error) {
	seg, err := segment(linkID)
	if err != nil {
		return time.Time{}, err
	}
	var out struct {
		RetireAt time.Time `json:"retire_at"`
	}
	if err := c.do(ctx, http.MethodPost, "/link-kits/"+seg+"/retire", nil, &out); err != nil {
		return time.Time{}, err
	}
	if out.RetireAt.IsZero() {
		return time.Time{}, fmt.Errorf("%w: retire answer without retire_at", ErrBadAnswer)
	}
	return out.RetireAt, nil
}

func (c *Client) Routes(ctx context.Context) ([]Route, error) {
	var out struct {
		Items []Route `json:"items"`
	}
	err := c.do(ctx, http.MethodGet, "/routes", nil, &out)
	return out.Items, err
}

// PutRoute is the provider's upsert, and so also the move to another kit.
func (c *Client) PutRoute(ctx context.Context, linkID, domain, targetHost string, port int) error {
	return c.do(ctx, http.MethodPost, "/routes", map[string]interface{}{
		"linkId": linkID, "domain": domain, "targetHost": targetHost, "targetPort": port,
	}, nil)
}

// DeleteRoute treats "already gone" as done, so a retried delete converges.
func (c *Client) DeleteRoute(ctx context.Context, domain string) error {
	seg, err := segment(domain)
	if err != nil {
		return err
	}
	err = c.do(ctx, http.MethodDelete, "/routes/"+seg, nil, nil)
	if IsCode(err, "not_found") {
		return nil
	}
	return err
}

func (c *Client) SetProxyProtocol(ctx context.Context, domain string, enabled bool) error {
	seg, err := segment(domain)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPatch, "/routes/"+seg+"/proxy-protocol", map[string]bool{"enabled": enabled}, nil)
}

func (c *Client) CheckDomain(ctx context.Context, domain string) (available bool, reason string, err error) {
	var out struct {
		Available bool   `json:"available"`
		Reason    string `json:"reason"`
	}
	err = c.do(ctx, http.MethodGet, "/check-domain?domain="+url.QueryEscape(domain), nil, &out)
	return out.Available, out.Reason, err
}

func (c *Client) Regions(ctx context.Context) (Regions, error) {
	var out Regions
	err := c.do(ctx, http.MethodGet, "/regions", nil, &out)
	return out, err
}

// CustomDomain is the provider's claim on one domain for this platform's
// account. TXTValue is the provider's own proof value: the customer publishes
// it next to the local one at TXTName.
type CustomDomain struct {
	Domain     string     `json:"domain"`
	State      string     `json:"state"`
	TXTName    string     `json:"txtName"`
	TXTValue   string     `json:"txtValue"`
	Attempts   int        `json:"attempts"`
	DeadlineAt *time.Time `json:"deadlineAt"`
	LapsesAt   *time.Time `json:"lapsesAt"`
}

func (c *Client) CustomDomains(ctx context.Context) ([]CustomDomain, error) {
	var out struct {
		Items []CustomDomain `json:"items"`
	}
	err := c.do(ctx, http.MethodGet, "/custom-domains", nil, &out)
	return out.Items, err
}

// AddCustomDomain is idempotent on the provider: a domain already claimed
// answers its existing claim and TXT value.
func (c *Client) AddCustomDomain(ctx context.Context, domain string) (CustomDomain, error) {
	var out struct {
		Domain CustomDomain `json:"domain"`
	}
	err := c.do(ctx, http.MethodPost, "/custom-domains", map[string]string{"domain": domain}, &out)
	return out.Domain, err
}

func (c *Client) IssueCustomDomainTXT(ctx context.Context, domain string) (CustomDomain, error) {
	seg, err := segment(domain)
	if err != nil {
		return CustomDomain{}, err
	}
	var out struct {
		Domain CustomDomain `json:"domain"`
	}
	err = c.do(ctx, http.MethodPost, "/custom-domains/"+seg+"/txt-token", nil, &out)
	return out.Domain, err
}

// VerifyCustomDomainTXT asks the provider to check its TXT value now. The
// provider answers "not found yet" as a 409 without a code of its own, which is
// verified=false with no error. Everything else is an APIError: verify_cooldown
// (429), not_found (404, no claim), bad_request (400: already verified and not
// failing, or no TXT value issued yet; the claim list tells the two apart).
func (c *Client) VerifyCustomDomainTXT(ctx context.Context, domain string) (verified bool, err error) {
	seg, err := segment(domain)
	if err != nil {
		return false, err
	}
	err = c.do(ctx, http.MethodPost, "/custom-domains/"+seg+"/verify-txt", nil, nil)
	if IsCode(err, "conflict") {
		return false, nil
	}
	return err == nil, err
}

// DeleteCustomDomain treats "no such claim" as done. domain_in_use (409) means
// a route on the domain still exists on the provider.
func (c *Client) DeleteCustomDomain(ctx context.Context, domain string) error {
	seg, err := segment(domain)
	if err != nil {
		return err
	}
	err = c.do(ctx, http.MethodDelete, "/custom-domains/"+seg, nil, nil)
	if IsCode(err, "not_found") {
		return nil
	}
	return err
}

// Usage fetches the month named by period (YYYY-MM); "" is the running month.
func (c *Client) Usage(ctx context.Context, period string) (Usage, error) {
	path := "/usage"
	if period != "" {
		path += "?period=" + url.QueryEscape(period)
	}
	var out struct {
		Usage struct {
			Period     string `json:"period"`
			EdgeBytes  int64  `json:"edgeBytes"`
			RelayBytes int64  `json:"relayBytes"`
		} `json:"usage"`
		Final bool  `json:"final"`
		Pool  *Pool `json:"pool"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return Usage{}, err
	}
	return Usage{Period: out.Usage.Period, EdgeBytes: out.Usage.EdgeBytes, RelayBytes: out.Usage.RelayBytes, Final: out.Final, Pool: out.Pool}, nil
}

// RouteVoice is an address's voice relay on the provider: Port is the public
// voice port, TargetPort the voice mod's port on the target host. Both are 0
// while voice is off.
type RouteVoice struct {
	Enabled    bool `json:"enabled"`
	Port       int  `json:"port"`
	TargetPort int  `json:"target_port"`
}

// RouteBedrock is an address's editions on the provider. HasKey: it has a
// Floodgate key to download. Refusal is why Bedrock cannot be added now.
type RouteBedrock struct {
	Java    bool   `json:"java"`
	Bedrock bool   `json:"bedrock"`
	HasKey  bool   `json:"has_key"`
	Refusal string `json:"refusal"`
}

// SetVoice switches the address's voice relay. targetPort 0 is the provider's
// default (the voice mods' 24454). Turning it on again keeps the port.
func (c *Client) SetVoice(ctx context.Context, domain string, enabled bool, targetPort int) (RouteVoice, error) {
	seg, err := segment(domain)
	if err != nil {
		return RouteVoice{}, err
	}
	var out struct {
		Voice *RouteVoice `json:"voice"`
	}
	if err := c.do(ctx, http.MethodPut, "/routes/"+seg+"/voice", map[string]interface{}{"enabled": enabled, "target_port": targetPort}, &out); err != nil {
		return RouteVoice{}, err
	}
	if out.Voice == nil {
		return RouteVoice{}, fmt.Errorf("%w: voice answer without voice", ErrBadAnswer)
	}
	return *out.Voice, nil
}

// SetBedrock sets the address's editions; at least one must be true.
func (c *Client) SetBedrock(ctx context.Context, domain string, java, bedrock bool) (RouteBedrock, error) {
	seg, err := segment(domain)
	if err != nil {
		return RouteBedrock{}, err
	}
	var out struct {
		Bedrock *RouteBedrock `json:"bedrock"`
	}
	if err := c.do(ctx, http.MethodPut, "/routes/"+seg+"/bedrock", map[string]bool{"java": java, "bedrock": bedrock}, &out); err != nil {
		return RouteBedrock{}, err
	}
	if out.Bedrock == nil {
		return RouteBedrock{}, fmt.Errorf("%w: bedrock answer without bedrock", ErrBadAnswer)
	}
	return *out.Bedrock, nil
}

// floodgateKeyLen is the size of the key Floodgate reads as key.pem.
const floodgateKeyLen = 16

// FloodgateKey downloads the address's Floodgate key: exactly 16 bytes, or
// ErrBadAnswer. The key is a secret: no error here carries it, only its length.
func (c *Client) FloodgateKey(ctx context.Context, domain string) ([]byte, error) {
	seg, err := segment(domain)
	if err != nil {
		return nil, err
	}
	var out struct {
		Key *string `json:"key"`
	}
	if err := c.do(ctx, http.MethodGet, "/routes/"+seg+"/floodgate-key", nil, &out); err != nil {
		if errors.Is(err, ErrBadAnswer) {
			// The JSON error may quote a piece of the body.
			return nil, fmt.Errorf("%w: floodgate key answer is not the expected JSON", ErrBadAnswer)
		}
		return nil, err
	}
	if out.Key == nil {
		return nil, fmt.Errorf("%w: floodgate key answer without key", ErrBadAnswer)
	}
	key, err := base64.StdEncoding.DecodeString(*out.Key)
	if err != nil {
		return nil, fmt.Errorf("%w: floodgate key is not base64", ErrBadAnswer)
	}
	if len(key) != floodgateKeyLen {
		return nil, fmt.Errorf("%w: floodgate key is %d bytes, want %d", ErrBadAnswer, len(key), floodgateKeyLen)
	}
	return key, nil
}

// RoutePlayers is one address's play sessions by edition.
type RoutePlayers struct {
	Domain  string `json:"domain"`
	Java    int    `json:"java"`
	Bedrock int    `json:"bedrock"`
}

// PlayerPeak is the highest concurrent players over a span of days, each
// column its own maximum.
type PlayerPeak struct {
	Java    int `json:"java"`
	Bedrock int `json:"bedrock"`
	Total   int `json:"total"`
}

type PlayersPeak struct {
	Today      PlayerPeak `json:"today"`
	Last30Days PlayerPeak `json:"last30Days"`
}

// Players is the account's play sessions now. Peak is nil from a provider
// that does not report it.
type Players struct {
	AsOf          time.Time
	Java, Bedrock int
	Items         []RoutePlayers
	Peak          *PlayersPeak
}

// Players reads the account's player counts. A provider older than the
// endpoint answers its router's 404 without a handler code: that is an empty
// Players and no error, so a customer Core can run against it.
func (c *Client) Players(ctx context.Context) (Players, error) {
	var out struct {
		AsOf  time.Time `json:"asOf"`
		Total struct {
			Java    int `json:"java"`
			Bedrock int `json:"bedrock"`
		} `json:"total"`
		Items []RoutePlayers `json:"items"`
		Peak  *PlayersPeak   `json:"peak"`
	}
	err := c.do(ctx, http.MethodGet, "/players", nil, &out)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound && ae.Code == "http_404" {
		return Players{}, nil
	}
	if err != nil {
		return Players{}, err
	}
	return Players{AsOf: out.AsOf, Java: out.Total.Java, Bedrock: out.Total.Bedrock, Items: out.Items, Peak: out.Peak}, nil
}
