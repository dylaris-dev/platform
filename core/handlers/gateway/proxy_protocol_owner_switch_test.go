package gateway

import (
	"context"
	"net/http"
	"testing"

	"dylaris-core/services"
)

// The owner's own switch supersedes a pending install request and vouches for
// the running sub-server.
func TestOwnerSwitchForgetsThePendingRequest(t *testing.T) {
	h, _, rdb := newProxyProtoHandler(t, true)
	fs := h.state.Store.(*proxyProtoFakeStore)
	fs.servers[7].ActiveSubServer = "main"
	seedRoute(t, rdb, "play.example.com", "tok-1", capsClientAddr)
	ctx := context.Background()
	if err := services.TrackProxyProtocolInstall(ctx, rdb, "srv-7", "main", false); err != nil {
		t.Fatal(err)
	}
	if rec := patchServerProxyProtocol(h, `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if ok, _ := rdb.HExists(ctx, "proxy_protocol_default:pending", "srv-7").Result(); ok {
		t.Fatal("pending request survived the owner's switch")
	}
	if ok, _ := services.ProxyProtocolSubConfigured(ctx, rdb, "srv-7", "main"); !ok {
		t.Fatal("active sub-server not recorded as configured")
	}
	if rec := patchServerProxyProtocol(h, `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if ok, _ := services.ProxyProtocolSubConfigured(ctx, rdb, "srv-7", "main"); ok {
		t.Fatal("configured sub-servers survived switching off")
	}
}
