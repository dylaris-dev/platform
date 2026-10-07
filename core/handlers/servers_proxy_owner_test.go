package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/models"
	"dylaris-core/store"
)

// proxyLinkStore holds two servers and one proxy and records the write, so the
// refusal can be told apart from a write that happened and was reported badly.
type proxyLinkStore struct {
	store.Store
	servers map[int]*models.Server
	linked  bool
}

func (f *proxyLinkStore) GetSetting(string) (string, error) { return "", nil }
func (f *proxyLinkStore) GetServerByID(id int) (*models.Server, error) {
	if s, ok := f.servers[id]; ok {
		return s, nil
	}
	return nil, errNotFoundForProxyLink
}
func (f *proxyLinkStore) UpdateServerProxyID(int, *int) error {
	f.linked = true
	return nil
}
func (f *proxyLinkStore) ListAllServers() ([]models.Server, error) {
	out := make([]models.Server, 0, len(f.servers))
	for _, s := range f.servers {
		out = append(out, *s)
	}
	return out, nil
}

type proxyLinkErr string

func (e proxyLinkErr) Error() string { return string(e) }

const errNotFoundForProxyLink = proxyLinkErr("not found")

func proxyLinkReq(serverID, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPut, "/api/servers/"+serverID+"/proxy", bytes.NewReader([]byte(body)))
	r = mux.SetURLVars(r, map[string]string{"id": serverID})
	ctx := context.WithValue(r.Context(), "userID", "owner-a")
	ctx = context.WithValue(ctx, "username", "owner-a")
	ctx = context.WithValue(ctx, "isAdmin", false)
	return r.WithContext(ctx)
}

func proxyLinkFixture() *proxyLinkStore {
	return &proxyLinkStore{servers: map[int]*models.Server{
		1: {ID: 1, UUID: "uuid-a", OwnerID: "owner-a", ServerType: "game"},
		2: {ID: 2, UUID: "uuid-own-proxy", OwnerID: "owner-a", ServerType: "proxy"},
		3: {ID: 3, UUID: "uuid-their-proxy", OwnerID: "owner-b", ServerType: "proxy"},
	}}
}

// Linking is what puts a proxy on the backend's ingress allow-list, so a link
// across two accounts hands a stranger's container direct network access to
// this server and puts the server's name and container hostname into that
// stranger's endpoint list.
//
// Measured on production: a server owned by one account was linked to another
// account's proxy through this route - by the owner, and again by a friend
// holding nothing but network.write - and the second account's
// /proxy-endpoint then listed the server by name.
func TestLinkServerToProxyRefusesAnotherAccountsProxy(t *testing.T) {
	fs := proxyLinkFixture()
	h := NewServerHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()

	h.LinkServerToProxy(rec, proxyLinkReq("1", `{"proxyId":3}`))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if fs.linked {
		t.Error("the link was written anyway; the refusal has to come before the write")
	}
}

// The same route with one owner still links, or the check above would have
// switched proxies off rather than bounded them.
func TestLinkServerToProxyAllowsTheOwnersOwnProxy(t *testing.T) {
	fs := proxyLinkFixture()
	h := NewServerHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()

	h.LinkServerToProxy(rec, proxyLinkReq("1", `{"proxyId":2}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !fs.linked {
		t.Error("the link was not written")
	}
}

// The disclosure half: a proxy lists its backends, and a row belonging to
// another account must not appear there even if one exists.
func TestProxyEndpointsSkipAnotherAccountsServer(t *testing.T) {
	fs := proxyLinkFixture()
	their := 3
	fs.servers[1].ProxyID = &their // as if written before the refusal existed
	h := NewServerHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()

	r := httptest.NewRequest(http.MethodGet, "/api/servers/3/proxy-endpoint", nil)
	r = mux.SetURLVars(r, map[string]string{"id": "3"})
	h.GetProxyEndpoint(rec, r)

	if body := rec.Body.String(); bytes.Contains([]byte(body), []byte("uuid-a")) {
		t.Errorf("the other account's server appears in the listing: %s", body)
	}
}

func (f *proxyLinkStore) GetNodeByID(id int) (*models.Node, error) {
	if id == 8 {
		customer := "owner-a"
		return &models.Node{ID: 8, OwnerID: &customer}, nil
	}
	return &models.Node{ID: id}, nil
}

// An inherit grant on a proxy reaches every server linked to it, so choosing
// the link is choosing who holds rights on the server - the owner's call, like
// setting inherit. A member with network.write on the server could link it
// under a proxy where they hold an inherit grant and gain console and files.
func TestLinkServerToProxyIsTheOwnersCall(t *testing.T) {
	cases := []struct {
		name   string
		user   string
		admin  bool
		nodeID int
		want   int
	}{
		{"owner", "owner-a", false, 7, http.StatusOK},
		{"member with network.write", "member-m", false, 7, http.StatusForbidden},
		{"admin on a platform node", "admin-x", true, 7, http.StatusOK},
		{"admin on the customer's own node", "admin-x", true, 8, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := proxyLinkFixture()
			fs.servers[1].NodeID = c.nodeID
			h := NewServerHandler(&AppState{Store: fs})
			r := proxyLinkReq("1", `{"proxyId":2}`)
			ctx := context.WithValue(r.Context(), "userID", c.user)
			ctx = context.WithValue(ctx, "isAdmin", c.admin)
			rec := httptest.NewRecorder()

			h.LinkServerToProxy(rec, r.WithContext(ctx))

			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.want, rec.Body.String())
			}
			if fs.linked != (c.want == http.StatusOK) {
				t.Errorf("linked = %v after status %d", fs.linked, rec.Code)
			}
		})
	}
}
