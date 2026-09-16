package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/services"
	"dylaris-core/store"
)

// A route-only customer used to see a green shield on every kit row, whether
// the Link had ever booted or not, so every failure between their machine and
// us was invisible. The list now carries what the Link itself says.

type kitListStore struct {
	store.Store
	settings map[string]string
	keys     []store.WarpAPIKey
}

func (f *kitListStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *kitListStore) ListWarpAPIKeysByOwner(string) ([]store.WarpAPIKey, error) {
	return f.keys, nil
}
func (f *kitListStore) CountLinkKitsByOwner(string) (int, error) { return len(f.keys), nil }

// No plan and no cap: this test is about the liveness field, and a limit of nil
// is the shape EffectiveLimits answers for a tenant with no billing row.
func (f *kitListStore) GetUserBilling(string) (*store.UserBilling, error) { return nil, nil }

type kitRow struct {
	LinkID string `json:"link_id"`
	Online *bool  `json:"online"`
}

func listKits(t *testing.T, rdb *redis.Client, kitIDs ...string) []kitRow {
	t.Helper()
	fs := &kitListStore{settings: map[string]string{"routing_mode": "gateway", "feature_byon_enabled": "true"}}
	for i, id := range kitIDs {
		fs.keys = append(fs.keys, store.WarpAPIKey{ID: i + 1, Name: id, NodeID: id, OwnerID: "alice", CreatedAt: time.Now()})
	}
	state := &AppState{
		Store:        fs,
		FeatureFlags: services.NewFeatureFlags(fs),
		Gateway:      services.NewRedisGateway(rdb, fs, "cs"),
		Redis:        rdb,
	}
	h := NewWarpHandler(state, nil)
	r := httptest.NewRequest(http.MethodGet, "/api/warp/link-kits", nil)
	r = r.WithContext(context.WithValue(r.Context(), "userID", "alice"))
	rec := httptest.NewRecorder()
	h.ListLinkKits(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Kits []kitRow `json:"kits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Kits
}

func TestListLinkKits_ReportsWhichLinkIsConnected(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	gw := services.NewRedisGateway(rdb, nil, "cs")
	mr.Set("online_link:"+gw.LinkToken("link-live"), "1")

	kits := listKits(t, rdb, "link-live", "link-dead")

	got := map[string]*bool{}
	for _, k := range kits {
		got[k.LinkID] = k.Online
	}
	if got["link-live"] == nil || !*got["link-live"] {
		t.Errorf("a running link reads as %v, want true", got["link-live"])
	}
	if got["link-dead"] == nil || *got["link-dead"] {
		t.Errorf("a link that never booted reads as %v, want false", got["link-dead"])
	}
}

// The state that must never be claimed: Core could not ask, so the row says
// nothing rather than telling a customer their machine is down.
func TestListLinkKits_SaysNothingWhenItCannotAsk(t *testing.T) {
	for name, rdb := range map[string]*redis.Client{
		"no redis at all": nil,
		"a dead redis":    deadRedis(t),
	} {
		t.Run(name, func(t *testing.T) {
			for _, k := range listKits(t, rdb, "link-one") {
				if k.Online != nil {
					t.Fatalf("online = %v, want absent", *k.Online)
				}
			}
		})
	}
}

func deadRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close()
	c := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	t.Cleanup(func() { c.Close() })
	return c
}
