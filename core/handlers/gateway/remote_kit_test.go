package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/remotegw"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

type remoteKitStore struct {
	store.Store
	nodes    map[int]*models.Node
	kits     map[uint]store.RemoteKit
	settings map[string]string
	audit    []*models.AuditEventIdentity
}

func (f *remoteKitStore) GetNodeByID(id int) (*models.Node, error) {
	if n, ok := f.nodes[id]; ok {
		return n, nil
	}
	return nil, sql.ErrNoRows
}
func (f *remoteKitStore) GetRemoteKit(nodeID uint) (*store.RemoteKit, error) {
	if k, ok := f.kits[nodeID]; ok {
		return &k, nil
	}
	return nil, nil
}
func (f *remoteKitStore) InsertRemoteKit(k store.RemoteKit) (bool, error) {
	if _, ok := f.kits[k.NodeID]; ok {
		return false, nil
	}
	f.kits[k.NodeID] = k
	return true, nil
}
func (f *remoteKitStore) ListRemoteKits() ([]store.RemoteKit, error) {
	var out []store.RemoteKit
	for _, k := range f.kits {
		out = append(out, k)
	}
	return out, nil
}
func (f *remoteKitStore) GetSetting(k string) (string, error) {
	if v, ok := f.settings[k]; ok {
		return v, nil
	}
	return "", sql.ErrNoRows
}
func (f *remoteKitStore) InsertAuditIdentity(ev *models.AuditEventIdentity) error {
	f.audit = append(f.audit, ev)
	return nil
}

func newRemoteKitHandler(t *testing.T, remote bool) (*RemoteKitHandler, *remoteKitStore, *remotegw.Fake) {
	t.Helper()
	owner := "tenant-1"
	fs := &remoteKitStore{
		nodes:    map[int]*models.Node{3: {ID: 3, Name: "node-3"}, 4: {ID: 4, Name: "customer", OwnerID: &owner}},
		kits:     map[uint]store.RemoteKit{},
		settings: map[string]string{"feature_byon_enabled": "true"},
	}
	fake := remotegw.NewFake(t)
	rg := services.NewRemoteGateway(fs, func() *remotegw.Client { return remotegw.New(fake.URL(), fake.Key) })
	h := NewRemoteKitHandler(Deps{
		Store:         fs,
		FeatureFlags:  services.NewFeatureFlags(fs),
		GatewayRemote: func() (bool, bool) { return remote, true },
		RemoteGateway: rg,
		GatewayProviderConfig: func() (services.ProviderConfig, error) {
			return services.ProviderConfig{Remote: remote, URL: fake.URL(), Key: fake.Key()}, nil
		},
	})
	return h, fs, fake
}

func getManagedLink(h *RemoteKitHandler, id string) (*httptest.ResponseRecorder, map[string]interface{}) {
	r := httptest.NewRequest(http.MethodGet, "/api/nodes/"+id+"/managed-link", nil)
	r = mux.SetURLVars(r, map[string]string{"id": id})
	r = r.WithContext(context.WithValue(r.Context(), "userID", "admin-1"))
	rec := httptest.NewRecorder()
	h.ManagedLink(rec, r)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestManagedLinkEndpointMintsOnce(t *testing.T) {
	h, fs, fake := newRemoteKitHandler(t, true)
	rec, out := getManagedLink(h, "3")
	if rec.Code != 200 || out["linkId"] != "link-fake1" || out["linkKey"] != "lk_secret_link-fake1" ||
		out["coreUrl"] != fake.URL() {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	// The node reports no network name: an empty network, never a guess
	// presented as fact.
	if out["network"] != "" || out["networkKnown"] != false || out["networkHint"] != "dylaris_net" {
		t.Fatalf("network fields: %s", rec.Body.String())
	}
	if v, present := out["online"]; !present || v != false {
		t.Fatalf("online = %v (present %v), want the provider's false", v, present)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q", cc)
	}
	fake.SetKitOnline("link-fake1")
	rec, out = getManagedLink(h, "3")
	if rec.Code != 200 || out["linkId"] != "link-fake1" || out["linkKey"] != "lk_secret_link-fake1" {
		t.Fatalf("second view: %d %s", rec.Code, rec.Body.String())
	}
	mints := 0
	for _, c := range fake.Calls() {
		if c == "POST /link-kits" {
			mints++
		}
	}
	if mints != 1 {
		t.Fatalf("provider calls %v, want exactly one mint", fake.Calls())
	}
	if len(fs.audit) != 2 || fs.audit[0].EventType != "managed_link_kit_viewed" {
		t.Fatalf("audit = %v", fs.audit)
	}
	meta, _ := json.Marshal(fs.audit[0].Metadata)
	if strings.Contains(string(meta), "lk_secret") || !strings.Contains(string(meta), "link-fake1") {
		t.Fatalf("audit metadata = %s", meta)
	}
}

// A customer's node never yields its kit key to an operator; the route-level
// cap (routes_remote_gate_test) keeps non-admins out altogether.
func TestManagedLinkEndpointRefusesForeignNode(t *testing.T) {
	h, fs, fake := newRemoteKitHandler(t, true)
	for _, id := range []string{"4", "99"} {
		if rec, _ := getManagedLink(h, id); rec.Code != http.StatusNotFound {
			t.Fatalf("node %s: %d %s, want 404", id, rec.Code, rec.Body.String())
		}
	}
	if len(fake.Calls()) != 0 || len(fs.kits) != 0 || len(fs.audit) != 0 {
		t.Fatalf("refused view reached the provider %v, stored %v or audited %v", fake.Calls(), fs.kits, fs.audit)
	}
}

func TestProviderUsageIsCached(t *testing.T) {
	h, _, fake := newRemoteKitHandler(t, true)
	get := func() (int, map[string]interface{}) {
		rec := httptest.NewRecorder()
		h.ProviderUsage(rec, httptest.NewRequest(http.MethodGet, "/api/gateway/provider/usage", nil))
		var out map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	// A failure is cached for 10 s (services tests pin the expiry): the
	// second ask is answered without a call.
	fake.FailNext(1, 503, "unavailable", 0)
	if code, _ := get(); code == 200 {
		t.Fatal("provider failure answered 200")
	}
	if code, _ := get(); code == 200 {
		t.Fatal("the cached failure answered 200")
	}
	fs := h.d.Store
	h.d.RemoteGateway = services.NewRemoteGateway(fs, func() *remotegw.Client { return remotegw.New(fake.URL(), fake.Key) })
	code, out := get()
	pool, _ := out["pool"].(map[string]interface{})
	if code != 200 || out["edgeBytes"] != float64(1000) || out["relayBytes"] != float64(20) || out["final"] != false || pool["includedGb"] != float64(5000) {
		t.Fatalf("%d %v", code, out)
	}
	get()
	n := 0
	for _, c := range fake.Calls() {
		if c == "GET /usage" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("usage calls = %d (%v), want 2: the failure, then one cached answer", n, fake.Calls())
	}
}

func TestManagedOnlyEndpointsRefuseLocally(t *testing.T) {
	h, _, fake := newRemoteKitHandler(t, false)
	rec, out := getManagedLink(h, "3")
	if rec.Code != http.StatusConflict || out["code"] != "gateway_local" {
		t.Fatalf("managed-link: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ProviderUsage(rec, httptest.NewRequest(http.MethodGet, "/api/gateway/provider/usage", nil))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "gateway_local") {
		t.Fatalf("usage: %d %s", rec.Code, rec.Body.String())
	}
	if len(fake.Calls()) != 0 {
		t.Fatalf("local mode called the provider: %v", fake.Calls())
	}
}
