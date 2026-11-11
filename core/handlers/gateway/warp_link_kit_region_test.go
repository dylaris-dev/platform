package gateway

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

type kitMintStore struct {
	store.Store
	settings map[string]string
	created  []store.WarpAPIKey
}

func (f *kitMintStore) GetSetting(k string) (string, error)               { return f.settings[k], nil }
func (f *kitMintStore) GetUserBilling(string) (*store.UserBilling, error) { return nil, nil }
func (f *kitMintStore) CreateWarpAPIKey(k store.WarpAPIKey) (int, error) {
	f.created = append(f.created, k)
	return len(f.created), nil
}

func mintKit(t *testing.T, body string) (*httptest.ResponseRecorder, *kitMintStore) {
	t.Helper()
	rdb := newLinkRouteRedis(t)
	rdb.Set(context.Background(), "edge:registry:e1", `{"edge_id":"e1","region":"eu","status":"online"}`, 0)
	rdb.Set(context.Background(), "edge:registry:e2", `{"edge_id":"e2","region":"us","status":"offline"}`, 0)
	fs := &kitMintStore{settings: map[string]string{"routing_mode": "gateway", "feature_byon_enabled": "true"}}
	h := NewWarpHandler(Deps{
		Store:        fs,
		FeatureFlags: services.NewFeatureFlags(fs),
		Gateway:      services.NewRedisGateway(rdb, fs, "cs"),
		Redis:        rdb,
	}, nil)
	r := httptest.NewRequest(http.MethodPost, "/api/warp/link-kits", bytes.NewBufferString(body))
	r = r.WithContext(context.WithValue(r.Context(), "userID", "alice"))
	rec := httptest.NewRecorder()
	h.MintLinkKit(rec, r)
	return rec, fs
}

func TestMintLinkKitStoresRegion(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"region":" EU "}`, "eu"},
		{`{}`, ""},
	} {
		rec, fs := mintKit(t, tc.body)
		if rec.Code != http.StatusOK || len(fs.created) != 1 || fs.created[0].Region != tc.want {
			t.Fatalf("%s: status %d, kits %+v, want region %q: %s", tc.body, rec.Code, fs.created, tc.want, rec.Body.String())
		}
	}
}

func TestMintLinkKitRejectsUnknownRegion(t *testing.T) {
	// "us" is registered but offline, so it is as unknown as "mars".
	for _, region := range []string{"mars", "us"} {
		rec, fs := mintKit(t, `{"region":"`+region+`"}`)
		if rec.Code != http.StatusBadRequest || len(fs.created) != 0 {
			t.Fatalf("%s: status %d, kits %+v, want 400 and no kit: %s", region, rec.Code, fs.created, rec.Body.String())
		}
		if code := externalErrorCode(rec.Code, rec.Body.String()); code != "unknown_region" {
			t.Fatalf("%s: external code = %q", region, code)
		}
	}
}

// No beam issuer keys: the publish after a kit or billing change is a no-op here.
func (*kitMintStore) ListBeamIssuerKeysByOwner(string) ([]models.BeamIssuerKey, error) {
	return nil, nil
}
