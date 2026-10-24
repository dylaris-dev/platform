package servers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/services"
)

type migrationCapStore struct {
	proxyProtoFakeStore
}

func (f *migrationCapStore) GetNodeByID(id int) (*models.Node, error) {
	return &models.Node{ID: id, Status: "online", Token: "node-src"}, nil
}

// A server with Real player IP on is not moved behind a link that cannot carry
// the address: the gateway would keep sending the header and every join would
// be refused.
func TestAMoveKeepsRealPlayerIPBehindALinkThatCarriesIt(t *testing.T) {
	_, base, rdb := newProxyProtoHandler(t, true)
	fs := &migrationCapStore{proxyProtoFakeStore: *base}
	h := &ServerHandler{state: &handlers.AppState{Store: fs, Redis: rdb, Gateway: services.NewRedisGateway(rdb, fs, "cs")}}
	srv := &models.Server{ID: 7, UUID: "srv-7", NodeID: 1, Status: "stopped", ProxyProtocol: true}
	target := &models.Node{ID: 2, Status: "online", Token: "node-dst", LinkToken: "link-dst"}

	rec := httptest.NewRecorder()
	h.queueMigration(rec, httptest.NewRequest(http.MethodPost, "/", nil), srv, target, "manual", "tester")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "link_without_client_addr") {
		t.Fatalf("answered %d %s, want 409 link_without_client_addr", rec.Code, rec.Body.String())
	}

	// A link that has only announced presence is no better.
	rdb.Set(t.Context(), "online_link:link-dst", "1", 0)
	rec = httptest.NewRecorder()
	h.queueMigration(rec, httptest.NewRequest(http.MethodPost, "/", nil), srv, target, "manual", "tester")
	if rec.Code != http.StatusConflict {
		t.Fatalf("answered %d for a link without the capability", rec.Code)
	}
}
