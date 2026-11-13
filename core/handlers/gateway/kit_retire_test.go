package gateway

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// RetireLinkKit on the multi-tenant fake: owner-scoped like the SQL, with the
// retirement records kept in retired.
func (f *extStore) RetireLinkKit(owner, link string, now, retireAt, monthStart time.Time, allow func(int64, int64) error) (time.Time, error) {
	idx := -1
	retiring := int64(0)
	for i := range f.kits {
		k := &f.kits[i]
		if k.OwnerID != owner || k.RevokedAt != nil {
			continue
		}
		if k.RetireAt != nil {
			retiring++
		}
		if k.NodeID == link {
			idx = i
		}
	}
	if idx < 0 {
		return time.Time{}, sql.ErrNoRows
	}
	if at := f.kits[idx].RetireAt; at != nil {
		return *at, store.ErrLinkKitRetiring
	}
	if err := allow(retiring, retiring+f.retiredThisMonth); err != nil {
		return time.Time{}, err
	}
	at := retireAt
	f.kits[idx].RetireAt = &at
	return retireAt, nil
}

func (f *extStore) retire(link string, at time.Time) {
	for i := range f.kits {
		if f.kits[i].NodeID == link {
			f.kits[i].RetireAt = &at
		}
	}
}

func retireReq(owner, link string) *http.Request {
	return extReq("POST", "/", owner, false, map[string]string{"linkID": link}, nil)
}

func TestExternalRetireRouteShape(t *testing.T) {
	f := newExtFixture(t)
	before := time.Now()
	rec := httptest.NewRecorder()
	f.h.RetireLinkKit(rec, retireReq(extTenantB, "link-b"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	out := decodeExt(t, rec)
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"link_id", "retire_at", "success"}) || out["success"] != true || out["link_id"] != "link-b" {
		t.Fatalf("got %s, want {success, link_id, retire_at}", rec.Body.String())
	}
	at, err := time.Parse(time.RFC3339, out["retire_at"].(string))
	if err != nil {
		t.Fatalf("retire_at %v: %v", out["retire_at"], err)
	}
	if d := at.Sub(before); d < services.KitRetireWindow-2*time.Second || d > services.KitRetireWindow+2*time.Second {
		t.Fatalf("retire_at is %v after the call, want %v", d, services.KitRetireWindow)
	}

	t.Run("already retiring returns the existing deadline", func(t *testing.T) {
		rec := httptest.NewRecorder()
		f.h.RetireLinkKit(rec, retireReq(extTenantB, "link-b"))
		again := decodeExt(t, rec)
		if rec.Code != http.StatusConflict || again["code"] != "kit_already_retiring" || again["retire_at"] != out["retire_at"] || again["error"] == "" {
			t.Fatalf("got %d %s, want 409 kit_already_retiring with retire_at %v", rec.Code, rec.Body.String(), out["retire_at"])
		}
	})

	for _, c := range []struct {
		name     string
		maxLinks *int64
		retiring bool  // another kit of B is retiring
		used     int64 // retirements already used this month besides it
		code     string
	}{
		{"no host limit", nil, true, 0, "retire_not_needed"},
		{"no kits at all", i64(0), true, 0, "retire_not_needed"},
		{"one host, one retiring", i64(1), true, 0, "retire_limit"},
		{"one host, the month's four used", i64(1), false, 4, "retire_budget"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newExtFixture(t)
			st := &capStore{extStore: f.st, maxLinks: c.maxLinks}
			f.h.warp.d.Store = st
			f.st.kits = append(f.st.kits, store.WarpAPIKey{ID: 3, NodeID: "link-b2", OwnerID: extTenantB})
			if c.retiring {
				f.st.retire("link-b2", time.Now().Add(time.Hour))
			}
			f.st.retiredThisMonth = c.used
			rec := httptest.NewRecorder()
			f.h.RetireLinkKit(rec, retireReq(extTenantB, "link-b"))
			out := decodeExt(t, rec)
			if rec.Code != http.StatusConflict || out["code"] != c.code {
				t.Fatalf("got %d %s, want 409 %s", rec.Code, rec.Body.String(), c.code)
			}
		})
	}
}

// capStore is extStore with a chosen link cap.
type capStore struct {
	*extStore
	maxLinks *int64
}

func (c *capStore) GetUserBilling(userID string) (*store.UserBilling, error) {
	return &store.UserBilling{UserID: userID, Status: "active", MaxLinks: c.maxLinks}, nil
}

// Tenant B cannot retire A's kit, and the answer is the one for a kit that
// does not exist.
func TestRetireIsOwnerScoped(t *testing.T) {
	for _, admin := range []bool{false, true} {
		f := newExtFixture(t)
		rec := httptest.NewRecorder()
		f.h.RetireLinkKit(rec, extReq("POST", "/", extTenantB, admin, map[string]string{"linkID": "link-a"}, nil))
		out := decodeExt(t, rec)
		if rec.Code != http.StatusNotFound || out["code"] != "link_not_found" {
			t.Fatalf("admin=%v: got %d %s, want 404 link_not_found", admin, rec.Code, rec.Body.String())
		}
		for _, m := range extMarkersOfA {
			if strings.Contains(rec.Body.String(), m) {
				t.Fatalf("answer carries %q of tenant A: %s", m, rec.Body.String())
			}
		}
		if f.st.kits[0].RetireAt != nil {
			t.Fatal("tenant A's kit was retired")
		}
		// A node key and nonsense get the same answer.
		for _, id := range []string{"node-b", "link-none"} {
			rec := httptest.NewRecorder()
			f.h.RetireLinkKit(rec, retireReq(extTenantB, id))
			if rec.Code != http.StatusNotFound || decodeExt(t, rec)["code"] != "link_not_found" {
				t.Fatalf("%s: got %d %s", id, rec.Code, rec.Body.String())
			}
		}
	}
}

// bedrockOnFlags is routeOnlyOnFlags with Bedrock offered to everyone.
type bedrockOnFlags struct{}

func (bedrockOnFlags) GetSetting(key string) (string, error) {
	switch key {
	case "feature_route_only_enabled":
		return "true", nil
	case services.BedrockSettingKey:
		return "all", nil
	}
	return "", nil
}

// A retiring kit keeps its routes (deletable, movable away) and takes nothing new.
func TestRetiringKitRefusesNewRoutes(t *testing.T) {
	newRoute := map[string]interface{}{"linkId": "link-b", "domain": "new.example.com", "targetHost": "192.168.1.5"}
	setupRetiring := func(t *testing.T, retiring string) *extFixture {
		f := newExtFixture(t)
		f.st.kits = append(f.st.kits, store.WarpAPIKey{ID: 3, NodeID: "link-b2", OwnerID: extTenantB})
		f.st.retire(retiring, time.Now().Add(time.Hour))
		return f
	}
	// b.example.com runs on link-b.
	setup := func(t *testing.T) *extFixture { return setupRetiring(t, "link-b") }
	refused := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusConflict || decodeExt(t, rec)["code"] != "kit_retiring" {
			t.Fatalf("got %d %s, want 409 kit_retiring", rec.Code, rec.Body.String())
		}
	}

	t.Run("new route", func(t *testing.T) {
		f := setup(t)
		rec := httptest.NewRecorder()
		f.h.CreateRoute(rec, extReq("POST", "/", extTenantB, false, nil, newRoute))
		refused(t, rec)
		if n := len(f.gw.createRouteViaLinkCalls); n != 0 {
			t.Fatalf("gateway got %d create calls", n)
		}
	})
	t.Run("route moved onto it", func(t *testing.T) {
		f := setupRetiring(t, "link-b2")
		rec := httptest.NewRecorder()
		f.h.CreateRoute(rec, extReq("POST", "/", extTenantB, false, nil,
			map[string]interface{}{"linkId": "link-b2", "domain": "b.example.com", "targetHost": "192.168.1.20"}))
		refused(t, rec)
	})
	t.Run("route moved away from it", func(t *testing.T) {
		f := setup(t)
		rec := httptest.NewRecorder()
		f.h.CreateRoute(rec, extReq("POST", "/", extTenantB, false, nil,
			map[string]interface{}{"linkId": "link-b2", "domain": "b.example.com", "targetHost": "192.168.1.20"}))
		if rec.Code != http.StatusCreated {
			t.Fatalf("move away = %d %s, want 201", rec.Code, rec.Body.String())
		}
	})
	t.Run("route on it deleted", func(t *testing.T) {
		f := setup(t)
		rec := httptest.NewRecorder()
		f.h.DeleteRoute(rec, extReq("DELETE", "/", extTenantB, false, map[string]string{"domain": "b.example.com"}, nil))
		if rec.Code != http.StatusOK || len(f.gw.deleted) != 1 {
			t.Fatalf("delete = %d %s (deleted %v), want 200", rec.Code, rec.Body.String(), f.gw.deleted)
		}
	})
	t.Run("voice on", func(t *testing.T) {
		f := setup(t)
		rec := httptest.NewRecorder()
		f.h.SetRouteVoice(rec, extReq("PUT", "/", extTenantB, false, map[string]string{"domain": "b.example.com"}, map[string]bool{"enabled": true}))
		refused(t, rec)
	})
	t.Run("voice off is never refused", func(t *testing.T) {
		f := setup(t)
		rec := httptest.NewRecorder()
		f.h.SetRouteVoice(rec, extReq("PUT", "/", extTenantB, false, map[string]string{"domain": "b.example.com"}, map[string]bool{"enabled": false}))
		if rec.Code != http.StatusOK {
			t.Fatalf("voice off = %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("bedrock on", func(t *testing.T) {
		f := setup(t)
		f.h.gw.d.FeatureFlags = services.NewFeatureFlags(bedrockOnFlags{})
		rec := httptest.NewRecorder()
		f.h.SetRouteBedrock(rec, extReq("PUT", "/", extTenantB, false, map[string]string{"domain": "b.example.com"}, map[string]bool{"java": true, "bedrock": true}))
		refused(t, rec)
	})
	t.Run("roll", func(t *testing.T) {
		f := setup(t)
		rec := httptest.NewRecorder()
		f.h.RollLinkKit(rec, retireReq(extTenantB, "link-b"))
		refused(t, rec)
	})
}

func (f *extStore) GetLinkRouteVoice(string) (*store.LinkRouteVoice, error)  { return nil, nil }
func (f *extStore) ListRouteEditions(int) ([]store.RouteEdition, error)      { return nil, nil }
func (f *extStore) ListBedrockServerRefs() ([]store.BedrockServerRef, error) { return nil, nil }
func (f *extStore) GetLinkRouteEdition(string) (*store.LinkRouteEdition, error) {
	return nil, nil
}

// The kit list carries retire_at on a retiring kit only, and retiring beside
// used, which counts live kits alone. Nothing that was there changes type.
func TestKitListCarriesRetireAt(t *testing.T) {
	f := newExtFixture(t)
	at := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	f.st.kits = append(f.st.kits, store.WarpAPIKey{ID: 3, NodeID: "link-b2", OwnerID: extTenantB})
	f.st.retire("link-b", at)
	rec := httptest.NewRecorder()
	f.h.ListLinkKits(rec, extReq("GET", "/", extTenantB, false, nil, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items    []map[string]interface{} `json:"items"`
		Used     *float64                 `json:"used"`
		Retiring *float64                 `json:"retiring"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Used == nil || *out.Used != 1 || out.Retiring == nil || *out.Retiring != 1 {
		t.Fatalf("used %v retiring %v, want 1 and 1: %s", out.Used, out.Retiring, rec.Body.String())
	}
	byID := map[string]map[string]interface{}{}
	for _, it := range out.Items {
		byID[it["link_id"].(string)] = it
	}
	if byID["link-b"]["retire_at"] != "2026-11-13T20:00:00Z" {
		t.Fatalf("retiring kit: %v", byID["link-b"])
	}
	if _, ok := byID["link-b2"]["retire_at"]; ok {
		t.Fatalf("a kit that is not retiring carries retire_at: %v", byID["link-b2"])
	}
	for _, it := range out.Items {
		if _, ok := it["id"].(float64); !ok {
			t.Fatalf("id changed type: %v", it)
		}
		if _, ok := it["caps"].([]interface{}); !ok {
			t.Fatalf("caps changed type: %v", it)
		}
	}
}

// retireMintStore is one owner at a cap of one, through the real mint handler.
type retireMintStore struct {
	store.Store
	settings map[string]string
	kits     []store.WarpAPIKey
}

func (f *retireMintStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *retireMintStore) GetUserBilling(u string) (*store.UserBilling, error) {
	return &store.UserBilling{UserID: u, Status: "active", MaxLinks: i64(1)}, nil
}
func (f *retireMintStore) CountLinkKitsByOwner(owner string) (int, error) {
	n := 0
	for _, k := range f.kits {
		if k.OwnerID == owner && k.RevokedAt == nil && k.RetireAt == nil {
			n++
		}
	}
	return n, nil
}
func (f *retireMintStore) CreateLinkKitUnderCap(k store.WarpAPIKey, cap int64) (bool, error) {
	if n, _ := f.CountLinkKitsByOwner(k.OwnerID); int64(n) >= cap {
		return false, nil
	}
	f.kits = append(f.kits, k)
	return true, nil
}
func (f *retireMintStore) RetireLinkKit(owner, link string, now, retireAt, monthStart time.Time, allow func(int64, int64) error) (time.Time, error) {
	for i := range f.kits {
		if f.kits[i].NodeID == link && f.kits[i].OwnerID == owner {
			if err := allow(0, 0); err != nil {
				return time.Time{}, err
			}
			f.kits[i].RetireAt = &retireAt
			return retireAt, nil
		}
	}
	return time.Time{}, sql.ErrNoRows
}
func (*retireMintStore) ListBeamIssuerKeysByOwner(string) ([]models.BeamIssuerKey, error) {
	return nil, nil
}

func TestRetireFreesTheHostSlotAtOnce(t *testing.T) {
	rdb := newLinkRouteRedis(t)
	fs := &retireMintStore{settings: map[string]string{"routing_mode": "gateway", "feature_route_only_enabled": "true"},
		kits: []store.WarpAPIKey{{ID: 1, NodeID: "link-old", OwnerID: "alice"}}}
	d := Deps{Store: fs, FeatureFlags: services.NewFeatureFlags(fs), Gateway: services.NewRedisGateway(rdb, fs, "cs"), Redis: rdb}
	warp := NewWarpHandler(d, nil)
	ext := NewExternalGatewayHandler(warp, NewGatewayHandler(d), nil, passGate, nil, nil)
	mint := func() *httptest.ResponseRecorder {
		r := extReq("POST", "/", "alice", false, nil, nil)
		r.Body = io.NopCloser(strings.NewReader(`{"name":"new node"}`))
		rec := httptest.NewRecorder()
		ext.MintLinkKit(rec, r)
		return rec
	}
	if rec := mint(); rec.Code != http.StatusForbidden || decodeExt(t, rec)["code"] != "link_limit" {
		t.Fatalf("premise: mint at the limit = %d %s, want 403 link_limit", rec.Code, rec.Body.String())
	}
	rec := httptest.NewRecorder()
	ext.RetireLinkKit(rec, retireReq("alice", "link-old"))
	if rec.Code != http.StatusOK {
		t.Fatalf("retire = %d %s", rec.Code, rec.Body.String())
	}
	if rec := mint(); rec.Code != http.StatusOK {
		t.Fatalf("mint after retire = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if rec := mint(); rec.Code != http.StatusForbidden {
		t.Fatalf("second mint after one retire = %d, want 403: the retire freed one slot", rec.Code)
	}
}
