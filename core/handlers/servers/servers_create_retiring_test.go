package servers

import (
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

// createRetiringStore has one node whose managed link is being replaced; any
// write the create would make goes to the nil embedded store and panics.
type createRetiringStore struct {
	createInputStore
	kitReads int
}

func (f *createRetiringStore) GetNodeByID(id int) (*models.Node, error) {
	return &models.Node{ID: id, Name: "draining", Status: "online"}, nil
}

func (f *createRetiringStore) GetRemoteKit(nodeID uint) (*store.RemoteKit, error) {
	f.kitReads++
	at := time.Now().Add(time.Hour)
	return &store.RemoteKit{NodeID: nodeID, LinkID: "link-old", RetireAt: &at}, nil
}

// A node named by hand is refused while its link is being replaced, before
// anything is created; on the platform's own gateway nothing changes.
func TestCreateServerRefusesANamedNodeBeingReplaced(t *testing.T) {
	body := map[string]any{
		"uuid":   "3f1f0e8a-6c1e-4d8a-9c4a-0b1d2e3f4a5b",
		"name":   "s",
		"nodeId": "3",
		"docker": map[string]any{"ram": 1024, "cpuLimit": 1.0, "diskLimit": 2147483648},
	}
	create := func(remote bool) (*createRetiringStore, *httptest.ResponseRecorder) {
		fs := &createRetiringStore{createInputStore: createInputStore{user: &models.User{ID: "u1"}}}
		mode := func() (bool, bool) { return remote, true }
		rg := services.NewRemoteGateway(fs, func() *remotegw.Client { return nil })
		h := &ServerHandler{d: testDeps(Deps{Store: fs, GatewayRemote: mode, RemoteGateway: rg})}
		rec := httptest.NewRecorder()
		func() {
			// Local mode goes on into the create, which this fixture cannot serve.
			defer func() { _ = recover() }()
			h.CreateServer(rec, createServerReq(body, "u1", true))
		}()
		return fs, rec
	}

	fs, rec := create(true)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"kit_retiring"`) ||
		!strings.Contains(rec.Body.String(), "This node's link is being replaced. Choose another node.") {
		t.Fatalf("remote: %d %s", rec.Code, rec.Body.String())
	}
	if fs.kitReads != 1 {
		t.Fatalf("remote: kit read %d times", fs.kitReads)
	}

	fs, rec = create(false)
	if fs.kitReads != 0 || strings.Contains(rec.Body.String(), "kit_retiring") {
		t.Fatalf("local mode consulted the kit (%d reads): %d %s", fs.kitReads, rec.Code, rec.Body.String())
	}
}
