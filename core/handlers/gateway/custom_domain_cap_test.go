package gateway

import (
	"database/sql"
	"fmt"
	"net/http"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

func i64(n int64) *int64 { return &n }

// capFakeStore holds both tables so a test can prove they answer independently.
type capFakeStore struct {
	store.Store
	settings map[string]string
	routes   map[string]*models.GatewayRouteLimit
	domains  map[string]*models.GatewayCustomDomainLimit
}

func (f *capFakeStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *capFakeStore) GetGatewayRouteLimit(scope string) (*models.GatewayRouteLimit, error) {
	if l, ok := f.routes[scope]; ok {
		return l, nil
	}
	return nil, sql.ErrNoRows
}
func (f *capFakeStore) GetGatewayCustomDomainLimit(scope string) (*models.GatewayCustomDomainLimit, error) {
	if l, ok := f.domains[scope]; ok {
		return l, nil
	}
	return nil, sql.ErrNoRows
}

func domRow(n *int64) *models.GatewayCustomDomainLimit {
	return &models.GatewayCustomDomainLimit{MaxCustomDomains: n}
}

func TestCustomDomainCapScopes(t *testing.T) {
	cases := []struct {
		name       string
		settings   map[string]string
		rows       map[string]*models.GatewayCustomDomainLimit
		want       int64 // -1 = no cap
		wantScoped bool
	}{
		{"nothing set: default setting of 50", nil, nil, 50, false},
		{"global setting applies", map[string]string{"gateway_max_custom_domains": "7"}, nil, 7, false},
		{"setting unlimited", map[string]string{"gateway_max_custom_domains": "unlimited"}, nil, -1, false},
		{"user scope beats the setting", map[string]string{"gateway_max_custom_domains": "7"},
			map[string]*models.GatewayCustomDomainLimit{"user:u1": domRow(i64(3))}, 3, true},
		{"zero is none, not unlimited", nil,
			map[string]*models.GatewayCustomDomainLimit{"user:u1": domRow(i64(0))}, 0, true},
		{"a NULL row is unlimited for the account even with a setting of 50", map[string]string{"gateway_max_custom_domains": "50"},
			map[string]*models.GatewayCustomDomainLimit{"user:u1": domRow(nil)}, -1, true},
		{"admin scope beats user scope", nil,
			map[string]*models.GatewayCustomDomainLimit{"admin:u1": domRow(i64(1)), "user:u1": domRow(i64(9))}, 1, true},
		{"user_default answers when the user has no row", nil,
			map[string]*models.GatewayCustomDomainLimit{"user_default": domRow(i64(4)), "global": domRow(i64(8))}, 4, true},
		{"global answers last", nil,
			map[string]*models.GatewayCustomDomainLimit{"global": domRow(i64(8))}, 8, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, scoped := customDomainCapFor(&capFakeStore{settings: c.settings, domains: c.rows}, "u1")
			if scoped != c.wantScoped {
				t.Fatalf("scoped = %v, want %v", scoped, c.wantScoped)
			}
			switch {
			case c.want < 0 && got != nil:
				t.Fatalf("cap = %d, want no cap", *got)
			case c.want >= 0 && (got == nil || *got != c.want):
				t.Fatalf("cap = %v, want %d", got, c.want)
			}
		})
	}
}

// A custom-domain cap on an account must not change what the same account may
// do with routes: the route row says unlimited, and stays that way.
func TestCustomDomainCapDoesNotTouchRouteLimit(t *testing.T) {
	fs := &capFakeStore{
		routes:  map[string]*models.GatewayRouteLimit{"user:u1": {MaxRoutes: nil}},
		domains: map[string]*models.GatewayCustomDomainLimit{"user:u1": domRow(i64(5))},
	}
	h := &GatewayHandler{d: Deps{Store: fs}}
	if got := h.effectiveRouteLimit("u1"); got != nil {
		t.Fatalf("route limit = %d, want unlimited (nil)", *got)
	}
	if got, _ := customDomainCapFor(fs, "u1"); got == nil || *got != 5 {
		t.Fatalf("custom-domain cap = %v, want 5", got)
	}
}

// The external API derives its stable error codes from these refusal strings.
func TestCustomDomainCapErrorMapsToItsExternalCode(t *testing.T) {
	h := &GatewayHandler{d: Deps{Store: &capFakeStore{domains: map[string]*models.GatewayCustomDomainLimit{"user:u1": domRow(i64(2))}}}}
	if msg := h.customDomainCapError("u1", 1); msg != "" {
		t.Fatalf("under the cap refused: %q", msg)
	}
	msg := h.customDomainCapError("u1", 2)
	if msg == "" {
		t.Fatal("at the cap was allowed")
	}
	for _, c := range []struct{ msg, code string }{
		{msg, "custom_domain_limit"},
		{fmt.Sprintf("You have used all %d addresses on our domains. Point your own domain at us instead.", 3), "route_limit"},
		{"Route creation is disabled for your account", "route_limit"},
	} {
		if got := externalErrorCode(http.StatusForbidden, c.msg); got != c.code {
			t.Errorf("%q -> %q, want %q", c.msg, got, c.code)
		}
	}
}

type addCapStore struct {
	addFakeStore
	domains map[string]*models.GatewayCustomDomainLimit
}

func (f *addCapStore) GetGatewayCustomDomainLimit(scope string) (*models.GatewayCustomDomainLimit, error) {
	if l, ok := f.domains[scope]; ok {
		return l, nil
	}
	return nil, sql.ErrNoRows
}

// A scoped cap above the claim bound of 50 must not be blocked by it, an
// unlimited one is not bounded at all, and a lower one does not lower it.
func TestClaimCapFollowsAScopedCap(t *testing.T) {
	run := func(rows map[string]*models.GatewayCustomDomainLimit) int {
		fs := &addCapStore{addFakeStore: addFakeStore{claims: map[string]*store.CustomDomainClaim{}, settings: map[string]string{
			"gateway_custom_domains_enabled": "true",
			"gateway_hoster_domains":         `[{"domain":"eu.dylaris.com","validation":"letters"}]`,
		}}, domains: rows}
		for i := 0; i < 50; i++ {
			d := fmt.Sprintf("d%d.example.com", i)
			fs.claims[d] = &store.CustomDomainClaim{ID: i + 1, Domain: d}
		}
		h := NewCustomDomainHandler(Deps{Store: fs}, noTXT{})
		return userCall(h.Add, "/api/gateway/custom-domains", `{"domain":"new.example.com"}`).Code
	}
	if code := run(nil); code != http.StatusConflict {
		t.Fatalf("51st claim with no scoped cap = %d, want 409", code)
	}
	if code := run(map[string]*models.GatewayCustomDomainLimit{"user:u1": domRow(i64(100))}); code == http.StatusConflict {
		t.Fatalf("51st claim under a scoped cap of 100 was refused")
	}
	if code := run(map[string]*models.GatewayCustomDomainLimit{"user:u1": domRow(nil)}); code == http.StatusConflict {
		t.Fatalf("51st claim for an account with no cap was refused")
	}
	if code := run(map[string]*models.GatewayCustomDomainLimit{"user:u1": domRow(i64(10))}); code != http.StatusConflict {
		t.Fatalf("a scoped cap of 10 raised the claim bound: %d", code)
	}
}
