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
	"dylaris-core/services/remotegw/remotegwtest"
)

// providerErr makes the fake provider refuse one call and returns what the
// real client made of the answer.
func providerErr(t *testing.T, status int, code string, retry time.Duration) error {
	t.Helper()
	fake := remotegwtest.NewFake(t)
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
		{"custom_domain_limit", func(t *testing.T) error { return providerErr(t, 403, "custom_domain_limit", 0) },
			want{403, "custom_domain_limit", "This platform's own-domain limit at the gateway provider is reached.", ""}},
		{"domain_unverified", func(t *testing.T) error { return providerErr(t, 403, "domain_unverified", 0) },
			want{403, "domain_unverified", "The gateway provider has not verified this domain yet. Publish its TXT record shown under Your own domains, then check again.", ""}},
		{"domain_blocked", func(t *testing.T) error { return providerErr(t, 403, "domain_blocked", 0) }, refusal(403, "domain_blocked")},
		{"target_not_allowed", func(t *testing.T) error { return providerErr(t, 400, "target_not_allowed", 0) }, refusal(400, "target_not_allowed")},
		{"invalid_request", func(t *testing.T) error { return providerErr(t, 400, "invalid_request", 0) }, refusal(400, "invalid_request")},
		{"billing_suspended", func(t *testing.T) error { return providerErr(t, 403, "billing_suspended", 0) }, refusal(403, "billing_suspended")},
		{"no_entitlement", func(t *testing.T) error { return providerErr(t, 403, "no_entitlement", 0) }, refusal(403, "no_entitlement")},
		{"forbidden", func(t *testing.T) error { return providerErr(t, 403, "forbidden", 0) }, refusal(403, "forbidden")},
		{"proxy_protocol_disabled", func(t *testing.T) error { return providerErr(t, 409, "proxy_protocol_disabled", 0) },
			want{409, "proxy_protocol_disabled", "Real player IP forwarding is not enabled on the gateway provider.", ""}},
		{"link_without_client_addr", func(t *testing.T) error { return providerErr(t, 409, "link_without_client_addr", 0) }, refusal(409, "link_without_client_addr")},
		{"route_suspended", func(t *testing.T) error { return providerErr(t, 403, "route_suspended", 0) }, refusal(403, "route_suspended")},
		{"kit_retiring", func(t *testing.T) error { return providerErr(t, 409, "kit_retiring", 0) },
			want{409, "kit_retiring", "This node's link is being replaced. Move the server to another node first.", ""}},
		{"retire_limit", func(t *testing.T) error { return providerErr(t, 409, "retire_limit", 0) },
			want{409, "retire_limit", "All replacement slots of this plan are in use. Wait until a link being replaced has run out.", ""}},
		{"retire_budget", func(t *testing.T) error { return providerErr(t, 409, "retire_budget", 0) },
			want{409, "retire_budget", "This plan's monthly number of link replacements is used up.", ""}},
		{"retire_not_needed", func(t *testing.T) error { return providerErr(t, 409, "retire_not_needed", 0) },
			want{409, "retire_not_needed", "This plan has no host limit, so no link needs replacing: add the new node and move the servers to it.", ""}},
		{"link_not_found", func(t *testing.T) error { return providerErr(t, 404, "link_not_found", 0) }, refusal(404, "link_not_found")},
		{"node's kit being replaced", func(*testing.T) error { return fmt.Errorf("create: %w", services.ErrGatewayKitRetiring) },
			want{409, "kit_retiring", "This node's link is being replaced. Move the server to another node first.", ""}},
		{"kit a node here holds", func(*testing.T) error { return services.ErrGatewayKitKnown },
			want{409, "kit_known", "This link belongs to a node of this platform. Replace or delete that node instead.", ""}},
		{"node without a kit", func(*testing.T) error { return services.ErrGatewayNoKit },
			want{404, "no_kit", "This node has no managed link yet.", ""}},
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

// Every limit the provider can answer on a kit mint, a kit retire or a route
// create keeps its code and gets a client status, never 502 provider_error.
func TestProviderLimitCodesKeepTheirCode(t *testing.T) {
	for _, c := range []struct {
		code       string
		in, status int
		msg        string // "" = the provider's own text passes through
	}{
		{"link_limit", 403, 409, "This plan's host limit is reached. Remove a node's link, or use Replace this node's link on a node you are retiring."},
		{"route_limit", 403, 403, ""},
		{"custom_domain_limit", 403, 403, "This platform's own-domain limit at the gateway provider is reached."},
		{"retire_limit", 409, 409, "All replacement slots of this plan are in use. Wait until a link being replaced has run out."},
		{"retire_budget", 409, 409, "This plan's monthly number of link replacements is used up."},
		{"retire_not_needed", 409, 409, "This plan has no host limit, so no link needs replacing: add the new node and move the servers to it."},
		{"kit_retiring", 409, 409, "This node's link is being replaced. Move the server to another node first."},
		{"kit_already_retiring", 409, 409, "This node's link is already being replaced."},
	} {
		t.Run(c.code, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if !GatewayProviderError(rec, providerErr(t, c.in, c.code, 0)) {
				t.Fatal("not handled")
			}
			var body struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			want := c.msg
			if want == "" {
				want = "fake failure"
			}
			if rec.Code != c.status || body.Code != c.code || body.Error != want {
				t.Fatalf("got %d %+v, want %d %s %q", rec.Code, body, c.status, c.code, want)
			}
		})
	}
}

// The own-domain codes: each keeps its code with a client status, the
// cooldown is not read as a rate limit on the key, and a provider message is
// never passed on where it names the provider's TXT value.
func TestProviderStatusOwnDomainCodes(t *testing.T) {
	const value = "dylaris-verify=00112233445566778899aabbccddeeff"
	for _, c := range []struct {
		code, providerMsg string
		in, status        int
		msg               string
	}{
		{"verify_cooldown", "You checked this domain a moment ago. Try again in 5 minutes.", 429, 429, "You checked this domain a moment ago. Try again in 5 minutes."},
		{"too_many_domains", "You can add up to 50 domains.", 409, 409, "This platform's own-domain limit at the gateway provider is reached."},
		{"custom_domain_limit", "limit", 403, 403, "This platform's own-domain limit at the gateway provider is reached."},
		{"domain_in_use", "Delete the routes on play.own.net first.", 409, 409, "Delete the routes on play.own.net first."},
		{"custom_domains_off", "Your own domains are not enabled on this platform.", 409, 409, "Own domains are not enabled for this account on the managed gateway."},
		{"domain_unverified", "Prove you own this domain first: add a TXT record _dylaris-verify.play.own.net with the value " + value, 403, 403,
			"The gateway provider has not verified this domain yet. Publish its TXT record shown under Your own domains, then check again."},
	} {
		t.Run(c.code, func(t *testing.T) {
			err := fmt.Errorf("wrapped: %w", &remotegw.APIError{Status: c.in, Code: c.code, Message: c.providerMsg})
			rec := httptest.NewRecorder()
			if !GatewayProviderError(rec, err) {
				t.Fatal("not handled")
			}
			var body struct{ Error, Code string }
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != c.status || body.Code != c.code || body.Error != c.msg {
				t.Fatalf("got %d %+v, want %d %s %q", rec.Code, body, c.status, c.code, c.msg)
			}
			if strings.Contains(rec.Body.String(), value) || rec.Header().Get("Retry-After") != "" {
				t.Fatalf("body %s, Retry-After %q", rec.Body, rec.Header().Get("Retry-After"))
			}
			if s, code, msg, ok := ProviderErrorAnswer(err); !ok || s != c.status || code != c.code || msg != c.msg {
				t.Fatalf("ProviderErrorAnswer = %d %s %q %v", s, code, msg, ok)
			}
		})
	}
}

// checkProviderCodes runs each provider refusal through GatewayProviderError.
func checkProviderCodes(t *testing.T, cases []struct{ code, providerMsg, msg string }) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			err := fmt.Errorf("wrapped: %w", &remotegw.APIError{Status: http.StatusConflict, Code: c.code, Message: c.providerMsg})
			rec := httptest.NewRecorder()
			if !GatewayProviderError(rec, err) {
				t.Fatal("not handled")
			}
			var body struct{ Error, Code string }
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != http.StatusConflict || body.Code != c.code || body.Error != c.msg {
				t.Fatalf("got %d %+v, want 409 %s %q", rec.Code, body, c.code, c.msg)
			}
		})
	}
}

func TestProviderStatusVoiceCodes(t *testing.T) {
	checkProviderCodes(t, []struct{ code, providerMsg, msg string }{
		{"voice_not_allowed", "Voice chat is not available for this account.", "Voice chat is not enabled for this platform's account on the managed gateway."},
		{"voice_not_allowed", "Voice chat is not available for this account. " + ManagedPlanNeededText,
			"Voice chat is not enabled for this platform's account on the managed gateway. It needs an active managed gateway plan for this platform."},
		{"link_without_voice", "The link of this route has not reported that it can carry voice chat.", "This node's managed link has not reported that it can carry voice chat. Make sure it is running and up to date."},
		{"no_routes", "Voice chat needs an address on one of the platform's domains.", RemoteVoiceNoRoutesText},
		{"no_relay", "No voice relay is running in this address's region yet.", "No voice relay is running in this address's region yet."},
		{"voice_pool_full", "Every voice port in this region is taken.", "Every voice port in this region is taken."},
		{"kit_retiring", "This link is being replaced and takes no new routes.", kitRetiringText},
	})
}

func TestProviderStatusBedrockCodes(t *testing.T) {
	checkProviderCodes(t, []struct{ code, providerMsg, msg string }{
		{"bedrock_disabled", "Bedrock is turned off on this platform.", "Bedrock is not enabled for this platform's account on the managed gateway."},
		{"bedrock_disabled", "Bedrock is turned off on this platform. " + ManagedPlanNeededText,
			"Bedrock is not enabled for this platform's account on the managed gateway. It needs an active managed gateway plan for this platform."},
		{"bedrock_unavailable", "Bedrock needs the gateway: no Bedrock entry is running in this address's region yet.", "Bedrock needs the gateway: no Bedrock entry is running in this address's region yet."},
		{"kit_retiring", "This link is being replaced and takes no new routes.", kitRetiringText},
	})
}
