package servers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/services"
)

// Deleting a server takes its gateway address with it. Moved out of the node
// delete tests, whose route helpers it copies.

func seedNodeRoute(t *testing.T, rdb *redis.Client, domain, uuid string) {
	t.Helper()
	b, err := json.Marshal(services.GatewayRoute{Domain: domain, ServerUUID: uuid, TargetIP: "mc_" + uuid})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := rdb.Set(ctx, "route:"+domain, b, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.SAdd(ctx, "sys:index:routes", domain).Err(); err != nil {
		t.Fatal(err)
	}
}

// nodeDeleteGateway answers the one call the cleanup makes of the hub.
type nodeDeleteGateway struct {
	services.GatewayProvider
	deleted []string
}

func (g *nodeDeleteGateway) DeleteServerRoutes(string) error { return nil }

func (g *nodeDeleteGateway) DeleteRoute(domain string) error {
	g.deleted = append(g.deleted, domain)
	return nil
}

// The single-server delete, which is where this cleanup already lived. It now
// shares one implementation with the two node paths above, so it needs its own
// guard: a refactor that moved it could otherwise drop it without a single test
// noticing.
func TestDeleteServer_TakesItsAddressWithIt(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	seedNodeRoute(t, rdb, "play.example.com", "srv-uuid")
	seedNodeRoute(t, rdb, "other.example.com", "uuid-elsewhere")

	gw := &nodeDeleteGateway{}
	fs := &deleteDispatchFakeStore{nodeMissing: true}
	h := &ServerHandler{state: &handlers.AppState{
		Store:   fs,
		Redis:   rdb,
		Gateway: gw,
		Events:  services.NewSystemEventsPublisher(nil),
	}}
	rec := httptest.NewRecorder()

	h.DeleteServer(rec, deleteServerRequest())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if n, _ := rdb.Exists(context.Background(), "route:play.example.com").Result(); n != 0 {
		t.Error("the deleted server's address still answers")
	}
	if n, _ := rdb.Exists(context.Background(), "route:other.example.com").Result(); n == 0 {
		t.Error("removed an address belonging to another server")
	}
	if len(gw.deleted) != 1 || gw.deleted[0] != "play.example.com" {
		t.Errorf("the hub was told about %v, want [play.example.com]", gw.deleted)
	}
	// The other half a deleted server leaves behind: its backup archives, whose
	// only record cascades away with it.
	if !fs.archivesAskedFor {
		t.Error("the server's backup archives were never named, so they stay in the bucket with nothing pointing at them")
	}
}
