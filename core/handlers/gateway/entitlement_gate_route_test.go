package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/handlers"
	"dylaris-core/store"
)

// Claiming an address passes the same gate as minting the kit. Only the panel
// checked it before, so a suspended tenant could still take subdomains through
// the API while their link was down.
func TestCreateLinkRoute_RefusedWhenSuspended(t *testing.T) {
	state := newEntGateState(&store.UserBilling{Status: "suspended", MaxLinks: ptrI64(1)}, true)
	h := &GatewayHandler{state: state}
	req := httptest.NewRequest(http.MethodPost, "/api/gateway/link-routes",
		strings.NewReader(`{"linkId":"link-x","subdomain":"a","targetHost":"127.0.0.1"}`))
	req = req.WithContext(context.WithValue(req.Context(), "userID", "u1"))
	rec := httptest.NewRecorder()
	h.CreateLinkRoute(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if got := decodeCode(t, rec); got != handlers.DenySuspended {
		t.Errorf("code = %q, want %q", got, handlers.DenySuspended)
	}
}
