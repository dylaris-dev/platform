package handlers

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	beamauth "dylaris-pkg/beam/auth"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

type proxyStampStore struct {
	store.Store
	servers []models.Server
}

func (f *proxyStampStore) GetServerByID(id int) (*models.Server, error) {
	for i := range f.servers {
		if f.servers[i].ID == id {
			s := f.servers[i]
			return &s, nil
		}
	}
	return nil, errors.New("not found")
}
func (f *proxyStampStore) ListAllServers() ([]models.Server, error) { return f.servers, nil }
func (f *proxyStampStore) UpdateServerProxyID(int, *int) error      { return nil }
func (f *proxyStampStore) UpdateServerOwner(int, *string) error     { return nil }
func (f *proxyStampStore) GetUserByID(id string) (*models.User, error) {
	return &models.User{ID: id}, nil
}
func (f *proxyStampStore) GetSetting(string) (string, error)     { return "", nil }
func (f *proxyStampStore) GetNodeByID(int) (*models.Node, error) { return &models.Node{ID: 1}, nil }

func (f *proxyStampStore) InsertAuditIdentity(*models.AuditEventIdentity) error { return nil }

func proxyStampState(t *testing.T) (*AppState, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	proxy := 1
	fs := &proxyStampStore{servers: []models.Server{
		{ID: 1, UUID: "proxy", ServerType: "proxy", OwnerID: "o", NodeID: 1},
		{ID: 2, UUID: "backend", OwnerID: "o", ProxyID: &proxy, NodeID: 1},
		{ID: 3, UUID: "unlinked", OwnerID: "o", NodeID: 1},
	}}
	return &AppState{Store: fs, Redis: rdb}, mr
}

// A grant with inherit on a proxy reaches its backends, so revoking it has to
// end beam sessions there too, not only on the proxy.
func TestStampingAProxyStampsItsBackends(t *testing.T) {
	st, mr := proxyStampState(t)
	stampBeamAccess(context.Background(), st, 1)
	for uuid, want := range map[string]bool{"proxy": true, "backend": true, "unlinked": false} {
		if got := mr.Exists(beamauth.AccessEpochKey(uuid)); got != want {
			t.Errorf("%s stamped = %v, want %v", uuid, got, want)
		}
	}
}

// Unlinking or moving to another proxy ends what was inherited; a new owner ends
// what the old one and their members held.
func TestUnlinkAndOwnerChangeStampTheServer(t *testing.T) {
	for name, call := range map[string]func(h *ServerHandler, w *httptest.ResponseRecorder){
		"unlink": func(h *ServerHandler, w *httptest.ResponseRecorder) {
			r := httptest.NewRequest("DELETE", "/x", nil)
			h.UnlinkServerFromProxy(w, mux.SetURLVars(r, map[string]string{"id": "2"}))
		},
		"relink": func(h *ServerHandler, w *httptest.ResponseRecorder) {
			r := httptest.NewRequest("POST", "/x", bytes.NewBufferString(`{"proxyId":1}`))
			h.LinkServerToProxy(w, mux.SetURLVars(r, map[string]string{"id": "3"}))
		},
		"owner": func(h *ServerHandler, w *httptest.ResponseRecorder) {
			r := httptest.NewRequest("PATCH", "/x", bytes.NewBufferString(`{"userId":"new"}`))
			ctx := context.WithValue(r.Context(), "isAdmin", true)
			ctx = context.WithValue(ctx, "userID", "admin")
			h.AdminUpdateServerOwner(w, mux.SetURLVars(r.WithContext(ctx), map[string]string{"id": "2"}))
		},
	} {
		t.Run(name, func(t *testing.T) {
			st, mr := proxyStampState(t)
			w := httptest.NewRecorder()
			call(&ServerHandler{state: st}, w)
			if w.Code != 200 {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			uuid := "backend"
			if name == "relink" {
				uuid = "unlinked"
			}
			if !mr.Exists(beamauth.AccessEpochKey(uuid)) {
				t.Fatal("the server was not stamped")
			}
		})
	}
}

type proxyDeleteStore struct{ deleteDispatchFakeStore }

func (f *proxyDeleteStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, UUID: "proxy", ServerType: "proxy", OwnerID: "alice", NodeID: 3}, nil
}
func (f *proxyDeleteStore) ListAllServers() ([]models.Server, error) {
	p := 7
	return []models.Server{{ID: 8, UUID: "backend", ProxyID: &p}}, nil
}

// Deleting a proxy nulls its backends' proxy_id through the foreign key, which
// ends what members inherited there without a single write Core could stamp.
func TestDeletingAProxyStampsItsBackends(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	h := &ServerHandler{state: &AppState{Store: &proxyDeleteStore{}, Redis: rdb, Events: services.NewSystemEventsPublisher(nil)}}
	rw := httptest.NewRecorder()
	h.DeleteServer(rw, deleteServerRequest())
	if rw.Code != 200 {
		t.Fatalf("status %d: %s", rw.Code, rw.Body.String())
	}
	if !mr.Exists(beamauth.AccessEpochKey("backend")) {
		t.Fatal("the proxy's backend was not stamped")
	}
}
