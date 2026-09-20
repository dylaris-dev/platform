package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// Deleting a machine has to take the addresses of its servers with it.
//
// Tested at the HANDLER and not only on services.RemoveServerRoutes, for the
// reason the BYON release before this one taught: a helper that is never called
// leaves every test on the helper green. Both of these paths delete their server
// rows with raw bulk SQL, so nothing about the cleanup is implied by the delete
// succeeding.

type nodeDeleteStore struct {
	store.Store
	node     *models.Node
	servers  []models.Server
	bulkDone bool
	nodeGone bool
	// archivesAskedFor records the server ids whose backup archives were looked
	// up. The lookup has to happen BEFORE the rows go: backup_jobs and
	// backup_runs cascade with the server, and the run row is the only record of
	// where an archive lives.
	archivesAskedFor []int
}

func (f *nodeDeleteStore) GetNodeByID(int) (*models.Node, error) { return f.node, nil }
func (f *nodeDeleteStore) ListServersByNode(int) ([]models.Server, error) {
	return f.servers, nil
}
func (f *nodeDeleteStore) DeleteServersByNode(int) error { f.bulkDone = true; return nil }
func (f *nodeDeleteStore) ListBackupRunRefsForServers(ids []int) ([]store.BackupRunRef, error) {
	if f.bulkDone {
		return nil, errors.New("the archives were looked up after the rows were already gone")
	}
	f.archivesAskedFor = append(f.archivesAskedFor, ids...)
	return nil, nil
}
func (f *nodeDeleteStore) DeleteNode(int) error { f.nodeGone = true; return nil }

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

// muxNodeRequest builds the request the router would hand the handler: the path
// variable and, for the /me routes, the caller identity the ownership check
// reads.
func muxNodeRequest(t *testing.T, method, target, id string, userID *string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	if userID != nil {
		r = r.WithContext(context.WithValue(r.Context(), "userID", *userID))
	}
	return mux.SetURLVars(r, map[string]string{"id": id})
}

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

func nodeDeleteFixture(t *testing.T, node *models.Node) (*NodeHandler, *nodeDeleteStore, *nodeDeleteGateway, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	fs := &nodeDeleteStore{
		node: node,
		servers: []models.Server{
			{ID: 1, UUID: "uuid-a", Name: "alpha"},
			{ID: 2, UUID: "uuid-b", Name: "beta"},
		},
	}
	gw := &nodeDeleteGateway{}
	seedNodeRoute(t, rdb, "a.example.com", "uuid-a")
	seedNodeRoute(t, rdb, "b.example.com", "uuid-b")
	seedNodeRoute(t, rdb, "other.example.com", "uuid-elsewhere")
	if err := rdb.Set(context.Background(), "dylaris:node:"+node.Token+":cpu", "{}", 0).Err(); err != nil {
		t.Fatal(err)
	}

	return NewNodeHandler(&AppState{Store: fs, Redis: rdb, Gateway: gw}), fs, gw, rdb
}

func assertNodeRoutesGone(t *testing.T, rdb *redis.Client, gw *nodeDeleteGateway) {
	t.Helper()
	ctx := context.Background()
	for _, d := range []string{"a.example.com", "b.example.com"} {
		if n, _ := rdb.Exists(ctx, "route:"+d).Result(); n != 0 {
			t.Errorf("%s still answers after the machine behind it was deleted", d)
		}
	}
	if n, _ := rdb.Exists(ctx, "route:other.example.com").Result(); n == 0 {
		t.Error("removed an address belonging to a server on another machine")
	}
	if len(gw.deleted) != 2 {
		t.Errorf("the hub was told about %v, want both addresses - otherwise its next sync writes them back", gw.deleted)
	}
}

// The operator-facing force delete. It removes every server on the machine, of
// every owner, and did so with no route cleanup at all: a deleted node's address
// was still answering at the edge on production, with a Dylaris placeholder
// behind it.
func TestForceDeleteNode_TakesTheAddressesOfItsServersWithIt(t *testing.T) {
	h, fs, gw, rdb := nodeDeleteFixture(t, &models.Node{ID: 5, Token: "node-tok", Status: "offline"})

	rec := httptest.NewRecorder()
	h.ForceDeleteNode(rec, muxNodeRequest(t, http.MethodDelete, "/api/nodes/5", "5", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if !fs.bulkDone || !fs.nodeGone {
		t.Fatalf("the delete itself did not happen (servers=%v node=%v)", fs.bulkDone, fs.nodeGone)
	}
	assertNodeRoutesGone(t, rdb, gw)
	assertArchivesAskedFor(t, fs)

	// The node's own housekeeping keys. This was the only node-delete door with
	// no cleanup call at all, so every force-deleted machine left its Redis user
	// and its keys behind for good - none of them expire.
	if n, _ := rdb.Exists(context.Background(), "dylaris:node:node-tok:cpu").Result(); n != 0 {
		t.Error("the node's Redis keys outlived the node")
	}
}

// The customer removing their OWN machine with its servers. Same bulk SQL, same
// omission, and here the address left behind is the customer's own.
func TestDeleteMyNode_TakesTheAddressesOfItsServersWithIt(t *testing.T) {
	owner := "tenant-1"
	h, fs, gw, rdb := nodeDeleteFixture(t, &models.Node{ID: 5, Token: "node-tok", Status: "online", OwnerID: &owner})

	r := muxNodeRequest(t, http.MethodDelete, "/api/me/nodes/5?servers=delete", "5", &owner)
	rec := httptest.NewRecorder()
	h.DeleteMyNode(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if !fs.bulkDone || !fs.nodeGone {
		t.Fatalf("the delete itself did not happen (servers=%v node=%v)", fs.bulkDone, fs.nodeGone)
	}
	assertNodeRoutesGone(t, rdb, gw)
}

// Keeping the machine's servers means keeping their addresses. The route belongs
// to the server, not to the node, and a customer moving a machine has not asked
// for anything of theirs to stop resolving.
func TestDeleteMyNode_LeavesTheAddressesWhenTheServersStay(t *testing.T) {
	owner := "tenant-1"
	h, fs, _, rdb := nodeDeleteFixture(t, &models.Node{ID: 5, Token: "node-tok", Status: "online", OwnerID: &owner})

	rec := httptest.NewRecorder()
	h.DeleteMyNode(rec, muxNodeRequest(t, http.MethodDelete, "/api/me/nodes/5", "5", &owner))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if fs.bulkDone {
		t.Fatal("deleted the servers without being asked to")
	}
	for _, d := range []string{"a.example.com", "b.example.com"} {
		if n, _ := rdb.Exists(context.Background(), "route:"+d).Result(); n == 0 {
			t.Errorf("%s stopped resolving although its server was kept", d)
		}
	}
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
	h := &ServerHandler{state: &AppState{
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

func (s *nodeDeleteStore) ListWarpKeyIDsBoundToNode(int) ([]int, error) { return nil, nil }

// assertArchivesAskedFor checks the backup half of the same story: the servers'
// archives have to be named while their rows still exist, or they stay in the
// bucket with nothing pointing at them.
func assertArchivesAskedFor(t *testing.T, fs *nodeDeleteStore) {
	t.Helper()
	if len(fs.archivesAskedFor) != len(fs.servers) {
		t.Errorf("archives looked up for %d server(s), want %d - the rest are left in the bucket", len(fs.archivesAskedFor), len(fs.servers))
	}
}
