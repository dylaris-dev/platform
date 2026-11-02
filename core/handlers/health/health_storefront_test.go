package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// storefrontHandler builds a HealthHandler pointed at a stub storefront.
func storefrontHandler(t *testing.T, h http.HandlerFunc) *HealthHandler {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewHealthHandler(Deps{
		StoreEnabled:   true,
		StoreURL:       srv.URL,
		StoreSharedKey: "shared-key",
	})
}

func TestStorefrontComponentUpWhenTheKeyIsAccepted(t *testing.T) {
	h := storefrontHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Store-Key") != "shared-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"linked":false}`))
	})
	if comp := h.storefrontComponent(context.Background()); comp.Status != "up" {
		t.Fatalf("Status = %q (%s / %s), want up", comp.Status, comp.Detail, comp.Reason)
	}
}

// The failure this exists for. A mismatched key produced a bare "not linked",
// which is the exact same thing the panel shows for a customer who has simply
// not connected their account - so the one state that needs an operator was
// invisible, on the path that carries money.
func TestStorefrontComponentNamesARefusedKey(t *testing.T) {
	h := storefrontHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	comp := h.storefrontComponent(context.Background())
	if comp.Status != "down" {
		t.Fatalf("Status = %q, want down", comp.Status)
	}
	if !strings.Contains(comp.Reason, "STORE_SHARED_KEY") {
		t.Errorf("Reason = %q, want it to name the mismatched setting", comp.Reason)
	}
}

func TestStorefrontComponentReportsAnUnreachableStore(t *testing.T) {
	h := NewHealthHandler(Deps{
		StoreEnabled: true,
		// Reserved TEST-NET-1 (RFC 5737): guaranteed not to route anywhere.
		StoreURL:       "http://192.0.2.1:9",
		StoreSharedKey: "shared-key",
	})
	if comp := h.storefrontComponent(context.Background()); comp.Status != "down" {
		t.Fatalf("Status = %q, want down for an unreachable storefront", comp.Status)
	}
}

// A build with no storefront configured is the open-core case, not a fault.
func TestStorefrontComponentDisabledWithoutConfig(t *testing.T) {
	h := NewHealthHandler(Deps{StoreEnabled: false})
	if comp := h.storefrontComponent(context.Background()); comp.Status != "disabled" {
		t.Fatalf("Status = %q, want disabled when no storefront is configured", comp.Status)
	}
}
