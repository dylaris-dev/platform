package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Core reaches the storefront on THREE routes - link-status, account-summary
// and billing-consent - and the health check probed two.
//
// The reason given for leaving the third out was that it is a write and no
// request to it changes nothing. The store checks the shared key before
// anything else, so a request that omits the key is refused on the first line
// and writes nothing.
//
// The omission is also what makes the answer readable, which is the part that
// matters: a missing proxy route on the website answers 404, and so does the
// store itself for a UUID nobody owns. With a valid key those two are the same
// number. Only the store answers 401.
//
// This was not hypothetical: on production only link-status was routed, the
// storefront component read "up", and every customer's billing page said the
// store could not be reached.
func TestTheThirdStorefrontRouteIsProbedWithoutWriting(t *testing.T) {
	t.Run("a 401 from the store is the route working", func(t *testing.T) {
		var gotKey, gotMethod, gotPath, gotBody string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotKey, gotMethod, gotPath = r.Header.Get("X-Store-Key"), r.Method, r.URL.Path
			b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<12))
			gotBody = string(b)
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "Invalid store key"})
		}))
		defer srv.Close()

		st := &AppState{StoreURL: srv.URL, StoreSharedKey: "the-real-key"}
		if err := st.probeBillingConsentRoute(context.Background()); err != nil {
			t.Fatalf("a routed store was reported down: %v", err)
		}
		if gotMethod != http.MethodPost || gotPath != "/api/store/billing-consent" {
			t.Errorf("probed %s %s, want POST /api/store/billing-consent", gotMethod, gotPath)
		}
		// The key is omitted deliberately. Sending it would make a missing route
		// and a healthy one both answer 404.
		if gotKey != "" {
			t.Errorf("the probe sent a key (%q); then a 404 could not be read", gotKey)
		}
		// Nothing that could change a consent: the probe UUID and no flags.
		if !strings.Contains(gotBody, healthProbeUUID) {
			t.Errorf("body %q does not name the probe UUID", gotBody)
		}
		for _, field := range []string{"traffic", "backup"} {
			if strings.Contains(gotBody, field) {
				t.Errorf("the probe body carries %q; a probe must not be able to change a consent: %s", field, gotBody)
			}
		}
	})

	// The case the whole probe exists for: the website proxy is missing, so
	// something that is not the store answers.
	t.Run("a 404 is the route missing", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("<!DOCTYPE html><html>404</html>"))
		}))
		defer srv.Close()

		st := &AppState{StoreURL: srv.URL, StoreSharedKey: "the-real-key"}
		err := st.probeBillingConsentRoute(context.Background())
		if err == nil {
			t.Fatal("an unrouted storefront was reported healthy - the exact state that shipped")
		}
		if !strings.Contains(err.Error(), "404") {
			t.Errorf("the reason does not name what came back: %v", err)
		}
	})

	t.Run("an unreachable storefront is reported", func(t *testing.T) {
		st := &AppState{StoreURL: "http://127.0.0.1:1", StoreSharedKey: "k"}
		if err := st.probeBillingConsentRoute(context.Background()); err == nil {
			t.Fatal("a storefront that cannot be dialled was reported healthy")
		}
	})
}
