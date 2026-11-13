package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func (f *remoteKitStore) SetRemoteKitRetireAt(nodeID uint, linkID string, at time.Time) error {
	if k, ok := f.kits[nodeID]; ok && k.LinkID == linkID {
		k.RetireAt = &at
		f.kits[nodeID] = k
	}
	return nil
}

func kitCall(h *RemoteKitHandler, fn func(*RemoteKitHandler) http.HandlerFunc, method string, vars map[string]string) (*httptest.ResponseRecorder, map[string]interface{}) {
	r := httptest.NewRequest(method, "/", nil)
	r = mux.SetURLVars(r, vars)
	r = r.WithContext(context.WithValue(r.Context(), "userID", "admin-1"))
	rec := httptest.NewRecorder()
	fn(h)(rec, r)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func retireCall(h *RemoteKitHandler, id string) (*httptest.ResponseRecorder, map[string]interface{}) {
	return kitCall(h, func(h *RemoteKitHandler) http.HandlerFunc { return h.RetireManagedLink }, http.MethodPost, map[string]string{"id": id})
}

func TestRetireManagedLinkEndpoint(t *testing.T) {
	h, fs, fake := newRemoteKitHandler(t, true)
	fake.SetLinkLimit(1)
	if rec, _ := retireCall(h, "3"); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no_kit") {
		t.Fatalf("no kit yet: %d %s, want 404 no_kit", rec.Code, rec.Body.String())
	}
	getManagedLink(h, "3")
	rec, out := retireCall(h, "3")
	at, err := time.Parse(time.RFC3339, fmt.Sprint(out["retireAt"]))
	if rec.Code != 200 || err != nil || time.Until(at) < 7*time.Hour {
		t.Fatalf("retire: %d %s", rec.Code, rec.Body.String())
	}
	last := fs.audit[len(fs.audit)-1]
	meta, _ := json.Marshal(last.Metadata)
	if last.EventType != "managed_link_kit_retired" || !strings.Contains(string(meta), "link-fake1") ||
		!strings.Contains(string(meta), `"nodeId":3`) || strings.Contains(string(meta), "lk_secret") {
		t.Fatalf("audit %s %s", last.EventType, meta)
	}
	// The deploy kit still shows the retiring link, with its deadline.
	rec, view := getManagedLink(h, "3")
	if rec.Code != 200 || view["linkId"] != "link-fake1" || view["retireAt"] != out["retireAt"] {
		t.Fatalf("kit view while retiring: %d %s", rec.Code, rec.Body.String())
	}
	// A provider refusal passes through with its code.
	fs.nodes[6] = &models.Node{ID: 6, Name: "node-6"}
	getManagedLink(h, "6")
	if rec, out := retireCall(h, "6"); rec.Code != http.StatusConflict || out["code"] != "retire_limit" ||
		out["error"] != "All replacement slots of this plan are in use. Wait until a link being replaced has run out." {
		t.Fatalf("second retire at one host: %d %s", rec.Code, rec.Body.String())
	}
	// A customer's node is not the operator's to replace.
	calls := len(fake.Calls())
	if rec, _ := retireCall(h, "4"); rec.Code != http.StatusNotFound || len(fake.Calls()) != calls {
		t.Fatalf("foreign node: %d %s, calls %v", rec.Code, rec.Body.String(), fake.Calls()[calls:])
	}
}

func TestOrphanKitsEndpoints(t *testing.T) {
	h, fs, fake := newRemoteKitHandler(t, true)
	fake.SetLinkLimit(5)
	getManagedLink(h, "3") // link-fake1, known
	cl := remotegw.New(fake.URL(), fake.Key)
	lost, _ := cl.MintKit(context.Background(), "lost node", "eu")
	retiring, _ := cl.MintKit(context.Background(), "old node", "eu")
	at, _ := cl.RetireKit(context.Background(), retiring.LinkID)
	h.d.RemoteGateway = services.NewRemoteGateway(fs, func() *remotegw.Client { return cl })

	list := func() (*httptest.ResponseRecorder, []map[string]interface{}) {
		rec, _ := kitCall(h, func(h *RemoteKitHandler) http.HandlerFunc { return h.OrphanKits }, http.MethodGet, nil)
		var out struct {
			Items []map[string]interface{} `json:"items"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec, out.Items
	}
	rec, items := list()
	if rec.Code != 200 || len(items) != 2 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	byID := map[string]map[string]interface{}{}
	for _, it := range items {
		byID[fmt.Sprint(it["linkId"])] = it
	}
	if it := byID[lost.LinkID]; it["name"] != "lost node" || it["region"] != "eu" || it["online"] != false {
		t.Fatalf("lost kit item: %v", it)
	} else if _, has := it["retireAt"]; has {
		t.Fatalf("a kit that is not retiring carries retireAt: %v", it)
	}
	if byID[retiring.LinkID]["retireAt"] != at.UTC().Format(time.RFC3339) {
		t.Fatalf("retiring kit item: %v", byID[retiring.LinkID])
	}
	if _, has := byID["link-fake1"]; has {
		t.Fatal("a kit a node here holds is listed")
	}

	revoke := func(link string) (*httptest.ResponseRecorder, map[string]interface{}) {
		return kitCall(h, func(h *RemoteKitHandler) http.HandlerFunc { return h.RevokeOrphanKit }, http.MethodDelete, map[string]string{"linkId": link})
	}
	if rec, out := revoke("link-fake1"); rec.Code != http.StatusConflict || out["code"] != "kit_known" {
		t.Fatalf("revoke of a known kit: %d %s", rec.Code, rec.Body.String())
	}
	if kits, _ := cl.Kits(context.Background()); len(kits) != 3 {
		t.Fatalf("provider kits after the refusal: %+v", kits)
	}
	audits := len(fs.audit)
	if rec, out := revoke(lost.LinkID); rec.Code != 200 || out["queued"] != false {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	last := fs.audit[len(fs.audit)-1]
	if len(fs.audit) != audits+1 || last.EventType != "provider_orphan_kit_revoked" || last.Metadata["linkId"] != lost.LinkID {
		t.Fatalf("audit %+v", fs.audit[audits:])
	}
	if _, items := list(); len(items) != 1 {
		t.Fatalf("after the revoke: %v", items)
	}
}

func TestNewManagedLinkEndpointsRefuseLocally(t *testing.T) {
	h, _, fake := newRemoteKitHandler(t, false)
	for name, fn := range map[string]func(*RemoteKitHandler) http.HandlerFunc{
		"retire":        func(h *RemoteKitHandler) http.HandlerFunc { return h.RetireManagedLink },
		"orphans":       func(h *RemoteKitHandler) http.HandlerFunc { return h.OrphanKits },
		"revoke orphan": func(h *RemoteKitHandler) http.HandlerFunc { return h.RevokeOrphanKit },
	} {
		rec, out := kitCall(h, fn, http.MethodPost, map[string]string{"id": "3", "linkId": "link-x"})
		if rec.Code != http.StatusConflict || out["code"] != "gateway_local" {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if len(fake.Calls()) != 0 {
		t.Fatalf("local mode called the provider: %v", fake.Calls())
	}
}
