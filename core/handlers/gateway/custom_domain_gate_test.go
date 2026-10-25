package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/handlers"

	"dylaris-core/services"
	"dylaris-core/store"
)

// The hint had no caller at all, so neither route handler told the customer
// anything: the route was accepted, and four hours later it was gone. These
// pin what it must say once it is shown.
func TestCustomDomainDeadlineHint(t *testing.T) {
	const name, value = "_dylaris-verify.mc.example.com", "dylaris-verify=abc"

	// The record is the proof, so the instruction is useless without it.
	t.Run("names the TXT record and its value", func(t *testing.T) {
		got := customDomainDeadlineHint(name, value, []string{"route.eu.dylaris.com"})
		if !strings.Contains(got, name) || !strings.Contains(got, value) {
			t.Errorf("hint does not name the TXT record: %q", got)
		}
	})

	t.Run("names the full target, never the bare label", func(t *testing.T) {
		got := customDomainDeadlineHint(name, value, []string{"route.eu.dylaris.com"})
		if !strings.Contains(got, "route.eu.dylaris.com") {
			t.Errorf("hint does not name the target: %q", got)
		}
		if !strings.Contains(got, "4 hours") {
			t.Errorf("hint does not state the deadline: %q", got)
		}
	})

	t.Run("one target per region, and the customer picks", func(t *testing.T) {
		got := customDomainDeadlineHint(name, value, []string{"route.eu.dylaris.com", "route.us.dylaris.com"})
		for _, want := range []string{"route.eu.dylaris.com", "route.us.dylaris.com"} {
			if !strings.Contains(got, want) {
				t.Errorf("hint omits %s: %q", want, got)
			}
		}
	})

	// An operator who has configured no hoster domain yet leaves no target to
	// name. The deadline still applies, so it still has to be stated.
	t.Run("no targets still warns about the deadline", func(t *testing.T) {
		got := customDomainDeadlineHint(name, value, nil)
		if !strings.Contains(got, "4 hours") {
			t.Errorf("hint drops the deadline when there is no target: %q", got)
		}
		if strings.Contains(got, "CNAME to ") {
			t.Errorf("hint promises a CNAME target it does not have: %q", got)
		}
	})
}

// claimFakeStore records what the gate does to the claim table.
type claimFakeStore struct {
	store.Store
	settings  map[string]string
	claim     *store.CustomDomainClaim
	started   []string
	tokens    []string
	requested []string
	verified  int
}

func (f *claimFakeStore) SetCustomDomainTXTToken(_ int, token string) error {
	f.tokens = append(f.tokens, token)
	return nil
}

func (f *claimFakeStore) GetSetting(key string) (string, error) { return f.settings[key], nil }

func (f *claimFakeStore) GetCustomDomainClaim(_, _ string) (*store.CustomDomainClaim, error) {
	if f.claim == nil {
		return nil, store.ErrNoClaim
	}
	return f.claim, nil
}

func (f *claimFakeStore) RequestCustomDomainClaim(_, domain string) (*store.CustomDomainClaim, error) {
	if f.claim == nil {
		f.requested = append(f.requested, domain)
		f.claim = &store.CustomDomainClaim{ID: 7, Domain: domain, State: store.ClaimPending}
	}
	return f.claim, nil
}

func (f *claimFakeStore) MarkCustomDomainVerified(int) error {
	f.verified++
	if f.claim != nil {
		f.claim.State = store.ClaimVerified
	}
	return nil
}

func (f *claimFakeStore) StartCustomDomainClaim(_, domain string, _ time.Time) (*store.CustomDomainClaim, error) {
	f.started = append(f.started, domain)
	return &store.CustomDomainClaim{Domain: domain, State: store.ClaimPending}, nil
}

func claimHandler(fs *claimFakeStore) *GatewayHandler {
	return &GatewayHandler{state: &handlers.AppState{Store: fs}}
}

func userRequest() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/gateway/link-routes", nil)
	//lint:ignore SA1029 the handlers read a plain string key; the tests match them.
	return r.WithContext(context.WithValue(r.Context(), "isAdmin", false))
}

// The check and the arm are separate because the gate runs BEFORE the port
// check, the per-account route cap and the create itself. Arming there left a
// live four-hour claim behind every refused request - and the verifier counts a
// missed deadline whether or not any route exists, so two refusals could
// permanently block a domain the customer never managed to route.
func TestCustomDomainGateDoesNotArmTheClaim(t *testing.T) {
	settings := map[string]string{
		"gateway_cname_target":   "route",
		"gateway_hoster_domains": `[{"domain":"eu.dylaris.com","validation":"alphanumeric"}]`,
	}

	// Ownership first: an unproven domain gets no route, so a CNAME left
	// pointing at us cannot be served to whoever claims it. The claim it starts
	// has NO deadline (no Start, no strike) and carries the token to publish.
	t.Run("an unproven domain is refused with the record to publish", func(t *testing.T) {
		fs := &claimFakeStore{settings: settings}
		h := claimHandler(fs)
		h.txtResolver = noTXT{}
		err := h.customDomainGate(userRequest(), "u1", "mc.example.com", true)
		if err == nil {
			t.Fatal("a domain nobody has proven was let through to a live route")
		}
		if len(fs.started) != 0 {
			t.Errorf("the check started a deadline %v; nothing may run out before a route exists", fs.started)
		}
		if len(fs.requested) != 1 || len(fs.tokens) != 1 || !strings.Contains(err.Error(), fs.tokens[0]) {
			t.Fatalf("requested %v tokens %v, refusal %q: it must name the token to publish", fs.requested, fs.tokens, err)
		}
	})

	t.Run("a record already published proves it on the spot", func(t *testing.T) {
		fs := &claimFakeStore{settings: settings,
			claim: &store.CustomDomainClaim{ID: 7, Domain: "mc.example.com", State: store.ClaimPending, TXTToken: "dylaris-verify=abc"}}
		h := claimHandler(fs)
		h.txtResolver = txtResolver{}
		if err := h.customDomainGate(userRequest(), "u1", "mc.example.com", true); err != nil {
			t.Fatalf("a proven domain was refused: %v", err)
		}
		if fs.verified != 1 {
			t.Fatalf("the proof was not recorded (verified=%d)", fs.verified)
		}
	})

	t.Run("a verified claim passes without a lookup", func(t *testing.T) {
		fs := &claimFakeStore{settings: settings,
			claim: &store.CustomDomainClaim{ID: 7, Domain: "mc.example.com", State: store.ClaimVerified}}
		h := claimHandler(fs)
		h.txtResolver = noTXT{}
		if err := h.customDomainGate(userRequest(), "u1", "mc.example.com", true); err != nil {
			t.Fatalf("a verified domain was refused: %v", err)
		}
	})

	t.Run("arming happens once, and names the expanded target", func(t *testing.T) {
		fs := &claimFakeStore{settings: settings}
		notice := claimHandler(fs).armCustomDomainClaim(userRequest(), "u1", "mc.example.com", true)
		if len(fs.started) != 1 || fs.started[0] != "mc.example.com" {
			t.Fatalf("started = %v, want [mc.example.com]", fs.started)
		}
		if !strings.Contains(notice, "route.eu.dylaris.com") {
			t.Errorf("notice does not name the real target: %q", notice)
		}
		// Every claim carries its proof token from the moment it is armed.
		if len(fs.tokens) != 1 || !strings.Contains(notice, fs.tokens[0]) {
			t.Errorf("arming minted %v and the notice says %q; the customer must be told the token", fs.tokens, notice)
		}
	})

	t.Run("a permanently blocked domain is refused by the check", func(t *testing.T) {
		fs := &claimFakeStore{settings: settings,
			claim: &store.CustomDomainClaim{Domain: "mc.example.com", State: store.ClaimPermablocked, Attempts: 2}}
		err := claimHandler(fs).customDomainGate(userRequest(), "u1", "mc.example.com", true)
		if err == nil {
			t.Fatal("a permanently blocked domain was allowed through")
		}
		if len(fs.started) != 0 {
			t.Error("a refused request still touched the claim")
		}
	})

	t.Run("a proven domain is not re-armed", func(t *testing.T) {
		fs := &claimFakeStore{settings: settings,
			claim: &store.CustomDomainClaim{Domain: "mc.example.com", State: store.ClaimVerified}}
		if notice := claimHandler(fs).armCustomDomainClaim(userRequest(), "u1", "mc.example.com", true); notice != "" {
			t.Errorf("a verified domain was put back on a clock: %q", notice)
		}
		if len(fs.started) != 0 {
			t.Errorf("started = %v, want none", fs.started)
		}
	})
}

// noTXT answers every TXT lookup with nothing published.
type noTXT struct{ services.DomainResolver }

func (noTXT) LookupTXT(context.Context, string) ([]string, error) { return nil, nil }
