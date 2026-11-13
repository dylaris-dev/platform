package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/services"
	"dylaris-core/services/remotegw"
)

// providerErr makes the fake provider refuse one call and returns what the
// real client made of the answer.
func providerErr(t *testing.T, status int, code string, retry time.Duration) error {
	t.Helper()
	fake := remotegw.NewFake(t)
	fake.FailNext(1, status, code, retry)
	_, _, err := remotegw.New(fake.URL(), fake.Key).CheckDomain(context.Background(), "a.example.com")
	if err == nil {
		t.Fatal("the fake did not fail")
	}
	return err
}

func TestGatewayProviderError(t *testing.T) {
	type want struct {
		status     int
		code, msg  string
		retryAfter string
	}
	refusal := func(status int, code string) want { return want{status, code, "fake failure", ""} }
	cases := []struct {
		name string
		err  func(t *testing.T) error
		want want
	}{
		{"domain_taken", func(t *testing.T) error { return providerErr(t, 409, "domain_taken", 0) }, refusal(409, "domain_taken")},
		{"route_limit", func(t *testing.T) error { return providerErr(t, 403, "route_limit", 0) }, refusal(403, "route_limit")},
		{"custom_domain_limit", func(t *testing.T) error { return providerErr(t, 403, "custom_domain_limit", 0) }, refusal(403, "custom_domain_limit")},
		{"domain_unverified", func(t *testing.T) error { return providerErr(t, 403, "domain_unverified", 0) }, refusal(403, "domain_unverified")},
		{"domain_blocked", func(t *testing.T) error { return providerErr(t, 403, "domain_blocked", 0) }, refusal(403, "domain_blocked")},
		{"target_not_allowed", func(t *testing.T) error { return providerErr(t, 400, "target_not_allowed", 0) }, refusal(400, "target_not_allowed")},
		{"invalid_request", func(t *testing.T) error { return providerErr(t, 400, "invalid_request", 0) }, refusal(400, "invalid_request")},
		{"billing_suspended", func(t *testing.T) error { return providerErr(t, 403, "billing_suspended", 0) }, refusal(403, "billing_suspended")},
		{"no_entitlement", func(t *testing.T) error { return providerErr(t, 403, "no_entitlement", 0) }, refusal(403, "no_entitlement")},
		{"forbidden", func(t *testing.T) error { return providerErr(t, 403, "forbidden", 0) }, refusal(403, "forbidden")},
		{"key rejected (auth layer, no code)", func(t *testing.T) error { return providerErr(t, 401, "", 0) },
			want{502, "provider_key_rejected", "The gateway provider rejected the API key.", ""}},
		{"rate limited", func(t *testing.T) error { return providerErr(t, 429, "rate_limited", 7*time.Second) },
			want{503, "rate_limited", "fake failure", "7"}},
		{"rate limited, no wait named", func(t *testing.T) error { return providerErr(t, 429, "rate_limited", 0) },
			want{503, "rate_limited", "fake failure", "30"}},
		{"rate limited, sub-second wait", func(*testing.T) error {
			return &remotegw.APIError{Status: 429, Code: "rate_limited", Message: "slow down", RetryAfter: 300 * time.Millisecond}
		}, want{503, "rate_limited", "slow down", "1"}},
		{"answer that cannot be parsed", func(t *testing.T) error {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>")) }))
			t.Cleanup(srv.Close)
			_, _, err := remotegw.New(srv.URL, func() string { return "k" }).CheckDomain(context.Background(), "a.example.com")
			if err == nil || !strings.Contains(err.Error(), "gateway provider answer unreadable") {
				t.Fatalf("client error = %v", err)
			}
			return err
		}, want{502, "provider_error", "The gateway provider sent an answer that could not be read. Try again in a moment.", ""}},
		{"anything else", func(t *testing.T) error { return providerErr(t, 500, "internal", 0) }, refusal(502, "provider_error")},
		{"server id cannot be a target", func(*testing.T) error { return fmt.Errorf("create: %w", services.ErrGatewayTargetName) },
			want{400, "invalid_target", "This server's id cannot be used as a gateway target.", ""}},
		{"address held by another server here", func(*testing.T) error { return services.ErrGatewayDomainTaken },
			want{409, "domain_taken", "this address is already in use", ""}},
		{"mode sentinel", func(*testing.T) error { return services.ErrGatewayRemote },
			want{409, "gateway_remote", "Not available with the managed gateway.", ""}},
		{"provider unreachable", func(t *testing.T) error {
			srv := httptest.NewServer(http.NotFoundHandler())
			url := srv.URL
			srv.Close()
			_, _, err := remotegw.New(url, func() string { return "k" }).CheckDomain(context.Background(), "a.example.com")
			return err
		}, want{502, "provider_unreachable", "The gateway provider could not be reached. Try again in a moment.", ""}},
		{"timeout", func(*testing.T) error { return fmt.Errorf("create: %w", context.DeadlineExceeded) },
			want{502, "provider_unreachable", "The gateway provider could not be reached. Try again in a moment.", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if !GatewayProviderError(rec, tc.err(t)) {
				t.Fatal("not handled")
			}
			var body struct {
				Success bool   `json:"success"`
				Error   string `json:"error"`
				Code    string `json:"code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if rec.Code != tc.want.status || body.Code != tc.want.code || body.Error != tc.want.msg || body.Success {
				t.Fatalf("got %d %+v, want %+v", rec.Code, body, tc.want)
			}
			if got := rec.Header().Get("Retry-After"); got != tc.want.retryAfter {
				t.Fatalf("Retry-After = %q, want %q", got, tc.want.retryAfter)
			}
		})
	}

	for _, err := range []error{nil, errors.New("db down")} {
		rec := httptest.NewRecorder()
		if GatewayProviderError(rec, err) || rec.Body.Len() != 0 {
			t.Fatalf("%v: handled, body %q", err, rec.Body)
		}
	}
}
