package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// beamKeyStore keeps beam issuer keys per owner, filtered the way the real
// queries are.
type beamKeyStore struct {
	store.Store
	keys    []models.BeamIssuerKey
	managed map[string]bool
	billing *store.UserBilling
}

func (f *beamKeyStore) CreateBeamIssuerKeyUnderCap(k models.BeamIssuerKey, cap int) (bool, error) {
	n := 0
	for _, x := range f.keys {
		if x.OwnerID == k.OwnerID && x.RevokedAt == nil {
			n++
		}
	}
	if n >= cap {
		return false, nil
	}
	k.CreatedAt = time.Now()
	f.keys = append(f.keys, k)
	return true, nil
}

func (f *beamKeyStore) ListBeamIssuerKeysByOwner(owner string) ([]models.BeamIssuerKey, error) {
	var out []models.BeamIssuerKey
	for _, k := range f.keys {
		if k.OwnerID == owner {
			out = append(out, k)
		}
	}
	return out, nil
}

func (f *beamKeyStore) RevokeBeamIssuerKey(owner, kid string) (bool, error) {
	for i := range f.keys {
		if f.keys[i].ID == kid && f.keys[i].OwnerID == owner && f.keys[i].RevokedAt == nil {
			now := time.Now()
			f.keys[i].RevokedAt = &now
			return true, nil
		}
	}
	return false, nil
}

func (f *beamKeyStore) GetUserBilling(string) (*store.UserBilling, error) { return f.billing, nil }

func (f *beamKeyStore) GetTrafficLimit(scope, region, kind string) (*models.TrafficLimit, error) {
	if f.managed[strings.TrimPrefix(scope, "user:")] && region == services.TrafficRegionAny && kind == services.TrafficKindEdge {
		return &models.TrafficLimit{}, nil
	}
	return nil, nil
}

func (f *beamKeyStore) ListWarpAPIKeysByOwner(string) ([]store.WarpAPIKey, error) { return nil, nil }
func (f *beamKeyStore) GetSetting(string) (string, error)                         { return "", nil }

func newBeamKeyHandler(t *testing.T) (*ExternalGatewayHandler, *beamKeyStore, *redis.Client) {
	t.Helper()
	fs := &beamKeyStore{managed: map[string]bool{extTenantA: true, extTenantB: true}}
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	d := Deps{Store: fs, Redis: rdb, SuspendGrace: time.Hour}
	return &ExternalGatewayHandler{gw: NewGatewayHandler(d)}, fs, rdb
}

func beamCall(h http.HandlerFunc, method, path, owner, body string, vars map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), "userID", owner))
	if vars != nil {
		r = mux.SetURLVars(r, vars)
	}
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

func registerBody(n int) string {
	return `{"public_key":"` + base64.StdEncoding.EncodeToString(make([]byte, n)) + `"}`
}

func extCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Code string `json:"code"`
		Kid  string `json:"kid"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out.Code
}

func TestRegisterBeamKey(t *testing.T) {
	h, fs, rdb := newBeamKeyHandler(t)
	path := "/api/external/gateway/beam/keys"

	rec := beamCall(h.RegisterBeamKey, http.MethodPost, path, extTenantA, registerBody(32), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid key: status %d %s", rec.Code, rec.Body.String())
	}
	var ok struct{ Kid string }
	json.Unmarshal(rec.Body.Bytes(), &ok)
	if !services.ValidBeamKid(ok.Kid) {
		t.Fatalf("kid %q is not 32 hex", ok.Kid)
	}
	if n, _ := rdb.Exists(context.Background(), services.BeamIssuerKeyPrefix+ok.Kid).Result(); n != 1 {
		t.Error("registered key was not published")
	}

	for _, c := range []struct {
		name, owner, body string
		status            int
		code              string
	}{
		{"31 bytes", extTenantA, registerBody(31), http.StatusBadRequest, "invalid_public_key"},
		{"33 bytes", extTenantA, registerBody(33), http.StatusBadRequest, "invalid_public_key"},
		{"not base64", extTenantA, `{"public_key":"!!"}`, http.StatusBadRequest, "invalid_public_key"},
		{"no managed product", extTenantC, registerBody(32), http.StatusForbidden, "managed_required"},
	} {
		rec := beamCall(h.RegisterBeamKey, http.MethodPost, path, c.owner, c.body, nil)
		if rec.Code != c.status || extCode(t, rec) != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.name, rec.Code, rec.Body.String(), c.status, c.code)
		}
	}

	if rec := beamCall(h.RegisterBeamKey, http.MethodPost, path, extTenantA, registerBody(32), nil); rec.Code != http.StatusOK {
		t.Fatalf("second key: %d", rec.Code)
	}
	rec = beamCall(h.RegisterBeamKey, http.MethodPost, path, extTenantA, registerBody(32), nil)
	if rec.Code != http.StatusConflict || extCode(t, rec) != "beam_key_limit" {
		t.Errorf("third key: %d %s, want 409 beam_key_limit", rec.Code, rec.Body.String())
	}
	// The other tenant's cap is its own.
	if rec := beamCall(h.RegisterBeamKey, http.MethodPost, path, extTenantB, registerBody(32), nil); rec.Code != http.StatusOK {
		t.Errorf("tenant B first key: %d", rec.Code)
	}

	past := time.Now().Add(-72 * time.Hour)
	fs.billing = &store.UserBilling{Status: "suspended", SuspendedAt: &past}
	rec = beamCall(h.RegisterBeamKey, http.MethodPost, path, extTenantB, registerBody(32), nil)
	if rec.Code != http.StatusForbidden || extCode(t, rec) != "account_suspended" {
		t.Errorf("suspended owner: %d %s, want 403 account_suspended", rec.Code, rec.Body.String())
	}
}

func TestRevokeBeamKey(t *testing.T) {
	h, fs, rdb := newBeamKeyHandler(t)
	ctx := context.Background()
	kidA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1"
	fs.keys = []models.BeamIssuerKey{{ID: kidA, OwnerID: extTenantA, PublicKey: make([]byte, 32)}}
	rdb.Set(ctx, services.BeamIssuerKeyPrefix+kidA, "published", 0)
	path := "/api/external/gateway/beam/keys/"

	rec := beamCall(h.RevokeBeamKey, http.MethodDelete, path+kidA, extTenantB, "", map[string]string{"kid": kidA})
	if rec.Code != http.StatusNotFound || extCode(t, rec) != "beam_key_not_found" {
		t.Fatalf("other owner's kid: %d %s, want 404", rec.Code, rec.Body.String())
	}
	if n, _ := rdb.Exists(ctx, services.BeamIssuerKeyPrefix+kidA).Result(); n != 1 {
		t.Fatal("a foreign revoke deleted the record")
	}
	for _, bad := range []string{"short", strings.ToUpper(kidA), "beam:issuer:*", kidA + "a"} {
		rec := beamCall(h.RevokeBeamKey, http.MethodDelete, path+bad, extTenantA, "", map[string]string{"kid": bad})
		if rec.Code != http.StatusBadRequest || extCode(t, rec) != "invalid_kid" {
			t.Errorf("kid %q: %d, want 400 invalid_kid", bad, rec.Code)
		}
	}
	rec = beamCall(h.RevokeBeamKey, http.MethodDelete, path+kidA, extTenantA, "", map[string]string{"kid": kidA})
	if rec.Code != http.StatusOK {
		t.Fatalf("own kid: %d %s", rec.Code, rec.Body.String())
	}
	if n, _ := rdb.Exists(ctx, services.BeamIssuerKeyPrefix+kidA).Result(); n != 0 {
		t.Error("revoked key's record survived")
	}
	list := beamCall(h.ListBeamKeys, http.MethodGet, "/api/external/gateway/beam/keys", extTenantA, "", nil)
	if strings.Contains(list.Body.String(), kidA) {
		t.Errorf("revoked key still listed: %s", list.Body.String())
	}
}

func seedBeams(t *testing.T, rdb *redis.Client) {
	t.Helper()
	ctx := context.Background()
	rdb.SAdd(ctx, "sys:beams", "b1", "b2", "b3", "b4")
	rdb.Set(ctx, "beam:registry:b1", `{"beam_id":"b1","ip":"1.2.3.4","private_ip":"10.0.0.1","region":"eu","public_host":"beam-eu.example.com","service_port":"25570","client_port":"25571"}`, 0)
	rdb.Set(ctx, "beam:cert:fingerprint:b1", "ab12", 0)
	rdb.Set(ctx, "beam:registry:b2", `{"beam_id":"b2","ip":"5.6.7.8","private_ip":"10.0.0.2","region":"us","service_port":"25570","client_port":"25571"}`, 0)
	// Only a private address: never handed out.
	rdb.Set(ctx, "beam:registry:b3", `{"beam_id":"b3","private_ip":"10.0.0.3","region":"eu","service_port":"25570","client_port":"25571"}`, 0)
	// b4: expired registry, left out.
}

func TestListBeamRelaysHidesPrivateIPs(t *testing.T) {
	h, _, rdb := newBeamKeyHandler(t)
	seedBeams(t, rdb)
	for _, c := range []struct {
		query string
		want  []externalBeamRelay
	}{
		{"", []externalBeamRelay{{"eu", "beam-eu.example.com", "25571"}, {"us", "5.6.7.8", "25571"}}},
		{"?region=US", []externalBeamRelay{{"us", "5.6.7.8", "25571"}}},
		{"?region=ap", []externalBeamRelay{}},
	} {
		rec := beamCall(h.ListBeamRelays, http.MethodGet, "/api/external/gateway/beam/relays"+c.query, extTenantA, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%q: %d", c.query, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "10.0.0.") {
			t.Errorf("%q: private address in %s", c.query, rec.Body.String())
		}
		var got struct{ Items []externalBeamRelay }
		json.Unmarshal(rec.Body.Bytes(), &got)
		if len(got.Items) != len(c.want) {
			t.Fatalf("%q: %+v, want %+v", c.query, got.Items, c.want)
		}
		for i := range c.want {
			if got.Items[i] != c.want[i] {
				t.Errorf("%q: item %d %+v, want %+v", c.query, i, got.Items[i], c.want[i])
			}
		}
	}
}

// The kit's link gets the relays of its own region, public addresses and the
// pin only.
func TestLinkBeamsHidesPrivateIPsAndFiltersRegion(t *testing.T) {
	for _, c := range []struct {
		region string
		want   map[string]linkBeam
	}{
		{"eu", map[string]linkBeam{"b1": {ID: "b1", Region: "eu", PublicHost: "beam-eu.example.com", ServicePort: "25570", CertSHA256: "ab12"}}},
		{"", map[string]linkBeam{
			"b1": {ID: "b1", Region: "eu", PublicHost: "beam-eu.example.com", ServicePort: "25570", CertSHA256: "ab12"},
			"b2": {ID: "b2", Region: "us", PublicHost: "5.6.7.8", ServicePort: "25570"},
		}},
	} {
		h, _, key, _ := routeOnlyKit(t)
		seedBeams(t, h.d.Redis)
		key.Region = c.region
		rec := callAs(h.LinkBeams, http.MethodGet, "/api/warp/link/beams", "", key)
		if rec.Code != http.StatusOK {
			t.Fatalf("region %q: %d %s", c.region, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "10.0.0.") {
			t.Errorf("region %q: private address in %s", c.region, rec.Body.String())
		}
		var got struct{ Beams []linkBeam }
		json.Unmarshal(rec.Body.Bytes(), &got)
		if len(got.Beams) != len(c.want) {
			t.Fatalf("region %q: %+v, want %+v", c.region, got.Beams, c.want)
		}
		for _, b := range got.Beams {
			if c.want[b.ID] != b {
				t.Errorf("region %q: %+v, want %+v", c.region, b, c.want[b.ID])
			}
		}
	}
}
