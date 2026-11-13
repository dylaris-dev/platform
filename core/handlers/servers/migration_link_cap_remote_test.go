package servers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/remotegw"
	"dylaris-core/store"
)

// remoteMoveStore adds the remote kit table to the move fixture.
type remoteMoveStore struct {
	migrationCapStore
	kits map[uint]store.RemoteKit
}

func (f *remoteMoveStore) GetRemoteKit(nodeID uint) (*store.RemoteKit, error) {
	if k, ok := f.kits[nodeID]; ok {
		return &k, nil
	}
	return nil, nil
}

func (f *remoteMoveStore) ListRemoteKits() ([]store.RemoteKit, error) {
	var out []store.RemoteKit
	for _, k := range f.kits {
		out = append(out, k)
	}
	return out, nil
}

// On the managed gateway the move precheck asks the target node's kit, never
// the local Redis: a target without a kit is allowed (the move mints it), a
// kit that is offline or lacks client-addr is refused.
func TestRemoteMovePrecheckUsesKitCaps(t *testing.T) {
	_, base, rdb := newProxyProtoHandler(t, true)
	fake := remotegw.NewFake(t)
	api := remotegw.New(fake.URL(), fake.Key)
	fs := &remoteMoveStore{migrationCapStore: migrationCapStore{proxyProtoFakeStore: *base}, kits: map[uint]store.RemoteKit{}}
	remote := func() (bool, bool) { return true, true }
	newHandler := func() *ServerHandler {
		rg := services.NewRemoteGateway(fs, func() *remotegw.Client { return api })
		return &ServerHandler{d: testDeps(Deps{Store: fs, Redis: rdb, GatewayRemote: remote, RemoteGateway: rg,
			Gateway: services.NewSwitchingGateway(nil, rg, remote)})}
	}
	srv := &models.Server{ID: 7, UUID: "srv-7", NodeID: 1, Status: "stopped", ProxyProtocol: true}
	target := &models.Node{ID: 2, Status: "online", Token: "node-dst", LinkToken: "link-dst"}
	// The local Redis says the link carries it; that must not count.
	rdb.Set(t.Context(), "online_link:link-dst", `{"caps":["client-addr"]}`, 0)
	// A passed precheck stops at the busy guard right after it, short of a queue.
	rdb.Set(t.Context(), services.NodeBusyKey(srv.UUID), "test", 0)

	refused := func(h *ServerHandler) bool {
		rec := httptest.NewRecorder()
		h.queueMigration(rec, httptest.NewRequest(http.MethodPost, "/", nil), srv, target, "manual", "tester")
		body := rec.Body.String()
		if !strings.Contains(body, "link_without_client_addr") && !strings.Contains(body, "still working on this server") {
			t.Fatalf("stopped elsewhere: %d %s", rec.Code, body)
		}
		return strings.Contains(body, "link_without_client_addr")
	}

	if refused(newHandler()) {
		t.Fatal("a target with no kit yet was refused; the move mints it")
	}
	minted, err := api.MintKit(context.Background(), "dst", "")
	if err != nil {
		t.Fatal(err)
	}
	fs.kits[2] = store.RemoteKit{NodeID: 2, LinkID: minted.LinkID}
	if !refused(newHandler()) {
		t.Fatal("an offline kit was accepted")
	}
	fake.SetKitOnline(minted.LinkID, "voice")
	if !refused(newHandler()) {
		t.Fatal("a kit without client-addr was accepted")
	}
	fake.SetKitOnline(minted.LinkID, "client-addr")
	if refused(newHandler()) {
		t.Fatal("a kit with client-addr was refused")
	}
	fake.FailNext(1, 500, "http_500", 0)
	if !refused(newHandler()) {
		t.Fatal("a provider error did not fail closed")
	}
}

// A move onto a node whose link is being replaced is refused whatever the
// server's real-IP switch: the provider takes no route onto that link and
// removes it within hours. A move onto a node with a link in normal use passes.
func TestRemoteMovePrecheckRefusesRetiringTarget(t *testing.T) {
	_, base, rdb := newProxyProtoHandler(t, true)
	fake := remotegw.NewFake(t)
	api := remotegw.New(fake.URL(), fake.Key)
	fs := &remoteMoveStore{migrationCapStore: migrationCapStore{proxyProtoFakeStore: *base}, kits: map[uint]store.RemoteKit{}}
	remote := func() (bool, bool) { return true, true }
	rg := services.NewRemoteGateway(fs, func() *remotegw.Client { return api })
	h := &ServerHandler{d: testDeps(Deps{Store: fs, Redis: rdb, GatewayRemote: remote, RemoteGateway: rg,
		Gateway: services.NewSwitchingGateway(nil, rg, remote)})}
	srv := &models.Server{ID: 7, UUID: "srv-7", NodeID: 1, Status: "stopped"}
	target := &models.Node{ID: 2, Status: "online", Token: "node-dst"}
	rdb.Set(t.Context(), services.NodeBusyKey(srv.UUID), "test", 0)
	at := time.Now().Add(8 * time.Hour)
	fs.kits[2] = store.RemoteKit{NodeID: 2, LinkID: "link-old", RetireAt: &at}

	move := func() (int, string) {
		rec := httptest.NewRecorder()
		h.queueMigration(rec, httptest.NewRequest(http.MethodPost, "/", nil), srv, target, "manual", "tester")
		return rec.Code, rec.Body.String()
	}
	if code, body := move(); code != http.StatusConflict || !strings.Contains(body, `"code":"kit_retiring"`) {
		t.Fatalf("move onto a retiring link: %d %s", code, body)
	}
	srv.ProxyProtocol = true
	if code, body := move(); code != http.StatusConflict || !strings.Contains(body, `"code":"kit_retiring"`) {
		t.Fatalf("move with real IP onto a retiring link: %d %s", code, body)
	}
	srv.ProxyProtocol = false
	fs.kits[2] = store.RemoteKit{NodeID: 2, LinkID: "link-new"}
	if _, body := move(); strings.Contains(body, "kit_retiring") || !strings.Contains(body, "still working on this server") {
		t.Fatalf("move onto a link in use stopped elsewhere: %s", body)
	}
	if len(fake.Calls()) != 0 {
		t.Fatalf("the precheck called the provider: %v", fake.Calls())
	}
}
