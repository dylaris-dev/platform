package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers/httpx"
)

// An external API call spends the key's write budget, not the panel user's
// DNS budget; a panel call gets pointingChecksPerMin a minute per user.
func TestPointingAllowSkipsExternalCalls(t *testing.T) {
	h := &GatewayHandler{d: Deps{PointingLimiter: httpx.NewIPRateLimiter()}}
	withUser := func(id string) *http.Request {
		r := httptest.NewRequest(http.MethodPut, "/x", nil)
		return r.WithContext(context.WithValue(r.Context(), "userID", id))
	}

	t.Run("external call has no per-user budget", func(t *testing.T) {
		var allow func() bool
		asked := false
		serveExternal(httptest.NewRecorder(), withUser("u1"), func(w http.ResponseWriter, r *http.Request) {
			asked = true
			allow = h.pointingAllow(r)
			w.Write([]byte(`{"success":true}`))
		}, nil)
		if !asked || allow != nil {
			t.Fatalf("asked %v, allow nil %v; want nil for an external call", asked, allow == nil)
		}
	})

	t.Run("panel call is limited per user", func(t *testing.T) {
		allow := h.pointingAllow(withUser("u2"))
		if allow == nil {
			t.Fatal("a panel call has no budget")
		}
		for i := 0; i < pointingChecksPerMin; i++ {
			if !allow() {
				t.Fatalf("check %d refused, want %d a minute", i+1, pointingChecksPerMin)
			}
		}
		if allow() {
			t.Fatal("check over the budget allowed")
		}
		if !h.pointingAllow(withUser("u3"))() {
			t.Fatal("another user shares the budget")
		}
	})
}
