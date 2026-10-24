package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// probeStoreLink lives on AppState rather than on StoreHandler because the
// entitlement gate needs the same answer and holds no handler. It reads only
// StoreURL and StoreSharedKey, both of which are state.
func (s *AppState) probeStoreLink(ctx context.Context, uuid string) (bool, string, error) {
	endpoint := strings.TrimRight(s.StoreURL, "/") + "/api/store/link-status?uuid=" + url.QueryEscape(uuid)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, "", err
	}
	req.Header.Set("X-Store-Key", s.StoreSharedKey)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("cannot reach the storefront at %s: %w", s.StoreURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// The single most likely misconfiguration, and the one that looks most
		// like normal operation from the panel.
		return false, "", fmt.Errorf("the storefront refused our key (HTTP %d) - STORE_SHARED_KEY does not match the value configured on %s", resp.StatusCode, s.StoreURL)
	}
	if resp.StatusCode != http.StatusOK {
		return false, "", fmt.Errorf("the storefront answered HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var parsed struct {
		Linked bool   `json:"linked"`
		Email  string `json:"email"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false, "", fmt.Errorf("the storefront answered something that is not the expected JSON: %w", err)
	}
	return parsed.Linked, parsed.Email, nil
}

// userRouteScope is the per-tenant key in gateway_route_limits. Kept in one place
// because effectiveRouteLimit resolves "user:<id>" -> "user_default" -> "global"
// by string, so a typo here does not fail, it silently falls through to the
// platform default and hands out the wrong number of addresses.
func userRouteScope(userID string) string { return "user:" + userID }

// adminRouteScope is an operator's address ceiling over the purchase. Asked
// before userRouteScope and never written by the store, so a renewal no longer
// replaces what an operator set - an abuse block of 0 included.
func adminRouteScope(userID string) string { return "admin:" + userID }

// storeAccountSummary fetches the tenant's picture from the storefront.
func (s *AppState) storeAccountSummary(ctx context.Context, uuid string) (map[string]interface{}, error) {
	endpoint := strings.TrimRight(s.StoreURL, "/") + "/api/store/account-summary?uuid=" + url.QueryEscape(uuid)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Store-Key", s.StoreSharedKey)

	resp, err := (&http.Client{Timeout: storeAccountTimeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the storefront at %s: %w", s.StoreURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("the storefront refused our key (HTTP %d) - STORE_SHARED_KEY does not match the value configured on %s", resp.StatusCode, s.StoreURL)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the storefront answered HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("the storefront answered something that is not the expected JSON: %w", err)
	}
	return parsed, nil
}

// storeSetBillingConsent moves one or both switches for this tenant.
//
// It returns the store's status and message rather than collapsing them: a 409
// "no active subscription", a 503 "no meter configured" and a 502 "the payment
// provider said no" are three different people's problem, and one generic
// failure would send all three to the wrong place.
// probeBillingConsentRoute reports whether the storefront's billing-consent
// endpoint is REACHABLE, without changing anything.
//
// It is the third channel Core depends on, and it was the one the health check
// left out. The reason given was that it is a write and no request to it
// changes nothing - but the key is checked before anything else, so a request
// that deliberately omits it is refused on the first line and writes nothing.
//
// The key is left off for a second reason, and it is the one that makes this
// work at all: a missing proxy route on the website answers 404, and so does
// the store itself for a UUID nobody owns. With a valid key those two are
// indistinguishable by status, which is why a "real" probe could not tell a
// broken route from a healthy one. Without a key only the store can answer
// 401, so a 401 proves both that the route exists and that the store is what
// is behind it.
//
// This is what went wrong before: on production only link-status was routed,
// and this component read "up" while every customer's billing page said the
// store could not be reached and nobody could switch metered billing on.
func (s *AppState) probeBillingConsentRoute(ctx context.Context) error {
	endpoint := strings.TrimRight(s.StoreURL, "/") + "/api/store/billing-consent"
	body, _ := json.Marshal(map[string]interface{}{"uuid": healthProbeUUID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// No X-Store-Key on purpose. See above.

	resp, err := (&http.Client{Timeout: storeAccountTimeout}).Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the storefront at %s: %w", s.StoreURL, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	if resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("the storefront answered HTTP %d where the store answers 401, so this route does not reach it", resp.StatusCode)
	}
	return nil
}

func (s *AppState) storeSetBillingConsent(ctx context.Context, uuid string, traffic, backup *bool) (int, string, error) {
	payload := map[string]interface{}{"uuid": uuid}
	if traffic != nil {
		payload["traffic"] = *traffic
	}
	if backup != nil {
		payload["backup"] = *backup
	}
	body, _ := json.Marshal(payload)

	endpoint := strings.TrimRight(s.StoreURL, "/") + "/api/store/billing-consent"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Store-Key", s.StoreSharedKey)

	resp, err := (&http.Client{Timeout: storeConsentTimeout}).Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("cannot reach the storefront at %s: %w", s.StoreURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return http.StatusOK, "", nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var parsed struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &parsed)
	if parsed.Error == "" {
		parsed.Error = fmt.Sprintf("The store refused the change (HTTP %d).", resp.StatusCode)
	}
	return resp.StatusCode, parsed.Error, nil
}

// usagePeriod resolves the billing month from an optional ?period=YYYY-MM query,
// defaulting to the current UTC month (the aggregator's key).
func usagePeriod(r *http.Request) time.Time {
	if p := r.URL.Query().Get("period"); p != "" {
		if t, err := time.Parse("2006-01", p); err == nil {
			return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		}
	}
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// storeAccountTimeout bounds the round trip. The panel renders the rest of the
// page without this, so a slow storefront must degrade to "we could not ask"
// rather than hold the whole view.
const storeAccountTimeout = 8 * time.Second

// storeConsentTimeout is longer: a consent change reports the usage up to now
// to Stripe and reads it from this Core before it answers. Cut off at 8s, the
// panel said "nothing was changed" about a change the store went on to apply.
const storeConsentTimeout = 45 * time.Second
