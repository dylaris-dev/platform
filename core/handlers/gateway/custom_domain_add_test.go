package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"

	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// addFakeStore is one user's claim table.
type addFakeStore struct {
	store.Store
	settings map[string]string
	claims   map[string]*store.CustomDomainClaim
}

func (f *addFakeStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *addFakeStore) ListCustomDomainClaimsByUser(string) ([]store.CustomDomainClaim, error) {
	var out []store.CustomDomainClaim
	for _, c := range f.claims {
		out = append(out, *c)
	}
	return out, nil
}
func (f *addFakeStore) RequestCustomDomainClaim(_, domain string) (*store.CustomDomainClaim, error) {
	if c, ok := f.claims[domain]; ok {
		return c, nil
	}
	c := &store.CustomDomainClaim{ID: len(f.claims) + 1, Domain: domain, State: store.ClaimPending}
	f.claims[domain] = c
	return c, nil
}
func (f *addFakeStore) SetCustomDomainTXTToken(id int, token string) error {
	for _, c := range f.claims {
		if c.ID == id {
			c.TXTToken = token
		}
	}
	return nil
}
func (f *addFakeStore) GetCustomDomainClaim(_, domain string) (*store.CustomDomainClaim, error) {
	if c, ok := f.claims[domain]; ok {
		return c, nil
	}
	return nil, store.ErrNoClaim
}

func userCall(h http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	//lint:ignore SA1029 the handlers read plain string keys; the tests match them.
	ctx := context.WithValue(r.Context(), "userID", "u1")
	//lint:ignore SA1029 same
	ctx = context.WithValue(ctx, "isAdmin", false)
	rec := httptest.NewRecorder()
	h(rec, r.WithContext(ctx))
	return rec
}

func TestAddingADomainReturnsTheRecordAndServesNothing(t *testing.T) {
	fs := &addFakeStore{claims: map[string]*store.CustomDomainClaim{}, settings: map[string]string{
		"gateway_custom_domains_enabled": "true",
		"gateway_hoster_domains":         `[{"domain":"eu.dylaris.com","validation":"letters"}]`,
	}}
	h := NewCustomDomainHandler(&handlers.AppState{Store: fs}, noTXT{})

	rec := userCall(h.Add, "/api/gateway/custom-domains", `{"domain":"play.example.com"}`)
	var out struct {
		Domain customDomainView `json:"domain"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || out.Domain.State != store.ClaimPending || out.Domain.DeadlineAt != nil ||
		out.Domain.TXTName != "_dylaris-verify.play.example.com" || out.Domain.TXTValue == "" {
		t.Fatalf("status %d view %+v", rec.Code, out.Domain)
	}
	if rec := userCall(h.Add, "/api/gateway/custom-domains", `{"domain":"x.eu.dylaris.com"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("a hoster subdomain was accepted as an own domain: %d", rec.Code)
	}
	fs.settings["gateway_custom_domains_enabled"] = "false"
	if rec := userCall(h.Add, "/api/gateway/custom-domains", `{"domain":"mc.example.org"}`); rec.Code != http.StatusConflict {
		t.Fatalf("own domains are off, status %d", rec.Code)
	}
}

// A manual check looks the record up at most every five minutes. Nothing
// counts a miss, so clicking can never block a domain.
func TestManualCheckHasACooldownAndNeverBlocks(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	fs := &addFakeStore{settings: map[string]string{}, claims: map[string]*store.CustomDomainClaim{
		"play.example.com": {ID: 1, Domain: "play.example.com", State: store.ClaimPending, TXTToken: "dylaris-verify=abc"},
	}}
	h := NewCustomDomainHandler(&handlers.AppState{Store: fs, Redis: rdb}, noTXT{})
	path := "/api/gateway/custom-domains/play.example.com/verify-txt"
	verify := func() int {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		r = mux.SetURLVars(r, map[string]string{"domain": "play.example.com"})
		//lint:ignore SA1029 the handlers read plain string keys.
		r = r.WithContext(context.WithValue(r.Context(), "userID", "u1"))
		rec := httptest.NewRecorder()
		h.VerifyTXT(rec, r)
		return rec.Code
	}
	if code := verify(); code != http.StatusConflict {
		t.Fatalf("first check: status %d, want 409 (record not there yet)", code)
	}
	for i := 0; i < 5; i++ {
		if code := verify(); code != http.StatusTooManyRequests {
			t.Fatalf("check %d within the cooldown: status %d, want 429", i+2, code)
		}
	}
	if c := fs.claims["play.example.com"]; c.State != store.ClaimPending || c.Attempts != 0 {
		t.Fatalf("clicks changed the claim: %+v", c)
	}
	mr.FastForward(verifyCheckCooldown)
	if code := verify(); code != http.StatusConflict {
		t.Fatalf("after the cooldown: status %d, want a real check (409)", code)
	}
}

func (f *addFakeStore) ListServersByOwner(string) ([]models.Server, error) { return nil, nil }
func (f *addFakeStore) DeleteCustomDomainClaim(_, domain string) (bool, error) {
	_, ok := f.claims[domain]
	delete(f.claims, domain)
	return ok, nil
}

// A domain can be removed again, but not while a route of the account still
// uses it: the claim is what keeps that route re-checked.
func TestRemovingADomain(t *testing.T) {
	rdb := newLinkRouteRedis(t)
	fs := &addFakeStore{settings: map[string]string{}, claims: map[string]*store.CustomDomainClaim{
		"play.example.com": {ID: 1, Domain: "play.example.com", State: store.ClaimVerified},
		"mc.example.com":   {ID: 2, Domain: "mc.example.com", State: store.ClaimPending},
	}}
	seedLinkRoute(t, rdb, "play.example.com", services.GatewayRoute{CoreOwned: true, OwnerID: "u1"})
	h := NewCustomDomainHandler(&handlers.AppState{Store: fs, Redis: rdb}, noTXT{})
	del := func(domain string) int {
		r := httptest.NewRequest(http.MethodDelete, "/api/gateway/custom-domains/"+domain, nil)
		r = mux.SetURLVars(r, map[string]string{"domain": domain})
		//lint:ignore SA1029 the handlers read plain string keys.
		r = r.WithContext(context.WithValue(r.Context(), "userID", "u1"))
		rec := httptest.NewRecorder()
		h.Delete(rec, r)
		return rec.Code
	}
	if code := del("play.example.com"); code != http.StatusConflict || fs.claims["play.example.com"] == nil {
		t.Fatalf("a domain with a route was removed: status %d", code)
	}
	if code := del("mc.example.com"); code != http.StatusOK || fs.claims["mc.example.com"] != nil {
		t.Fatalf("removing an unused domain: status %d", code)
	}
	if code := del("nope.example.com"); code != http.StatusNotFound {
		t.Fatalf("unknown domain: status %d", code)
	}
}
