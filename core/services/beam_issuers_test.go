package services

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-core/store"
)

const (
	beamKidA1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1"
	beamKidA2 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa2"
	beamKidB1 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb1"
)

// beamIssuerFakeStore answers per owner, the way the real queries filter.
type beamIssuerFakeStore struct {
	store.Store
	keys     []models.BeamIssuerKey
	kits     []store.WarpAPIKey
	billing  map[string]*store.UserBilling
	managed  map[string]bool
	settings map[string]string
	keysErr  error
}

func (f *beamIssuerFakeStore) ListBeamIssuerKeysByOwner(owner string) ([]models.BeamIssuerKey, error) {
	if f.keysErr != nil {
		return nil, f.keysErr
	}
	var out []models.BeamIssuerKey
	for _, k := range f.keys {
		if k.OwnerID == owner {
			out = append(out, k)
		}
	}
	return out, nil
}

func (f *beamIssuerFakeStore) ListBeamIssuerOwners() ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, k := range f.keys {
		if !seen[k.OwnerID] {
			seen[k.OwnerID] = true
			out = append(out, k.OwnerID)
		}
	}
	return out, nil
}

func (f *beamIssuerFakeStore) ListBeamIssuerKids() ([]string, error) {
	var out []string
	for _, k := range f.keys {
		out = append(out, k.ID)
	}
	return out, nil
}

func (f *beamIssuerFakeStore) GetUserBilling(owner string) (*store.UserBilling, error) {
	return f.billing[owner], nil
}

func (f *beamIssuerFakeStore) GetTrafficLimit(scope, region, kind string) (*models.TrafficLimit, error) {
	owner := strings.TrimPrefix(scope, "user:")
	if region == TrafficRegionAny && kind == TrafficKindEdge && f.managed[owner] {
		return &models.TrafficLimit{Scope: scope, Region: region, Kind: kind}, nil
	}
	return nil, nil
}

func (f *beamIssuerFakeStore) ListWarpAPIKeysByOwner(owner string) ([]store.WarpAPIKey, error) {
	var out []store.WarpAPIKey
	for _, k := range f.kits {
		if k.OwnerID == owner && k.RevokedAt == nil {
			out = append(out, k)
		}
	}
	return out, nil
}

func (f *beamIssuerFakeStore) GetSetting(k string) (string, error) { return f.settings[k], nil }

func beamIssuerFixture(t *testing.T) (*beamIssuerFakeStore, *redis.Client) {
	t.Helper()
	now := time.Now()
	pub := func(b byte) []byte { p := make([]byte, 32); p[0] = b; return p }
	fs := &beamIssuerFakeStore{
		keys: []models.BeamIssuerKey{
			{ID: beamKidA1, OwnerID: "owner-a", PublicKey: pub(1)},
			{ID: beamKidA2, OwnerID: "owner-a", PublicKey: pub(2), RevokedAt: &now},
			{ID: beamKidB1, OwnerID: "owner-b", PublicKey: pub(3)},
		},
		kits: []store.WarpAPIKey{
			{NodeID: "link-a2", OwnerID: "owner-a"},
			{NodeID: "link-a1", OwnerID: "owner-a"},
			{NodeID: "node-a", OwnerID: "owner-a"}, // a BYON node key, not a kit
			{NodeID: "link-b1", OwnerID: "owner-b"},
		},
		billing:  map[string]*store.UserBilling{},
		managed:  map[string]bool{"owner-a": true, "owner-b": true},
		settings: map[string]string{},
	}
	mr := miniredis.RunT(t)
	return fs, redis.NewClient(&redis.Options{Addr: mr.Addr()})
}

func readIssuer(t *testing.T, rdb *redis.Client, kid string) (map[string]interface{}, bool) {
	t.Helper()
	raw, err := rdb.Get(context.Background(), BeamIssuerKeyPrefix+kid).Result()
	if err == redis.Nil {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("record %q: %v", raw, err)
	}
	return m, true
}

// An owner's record names that owner's live kits and nothing else: not the
// other tenant's kit, not a node key. A revoked key's record is deleted.
func TestPublishBeamIssuersListsOnlyOwnKits(t *testing.T) {
	fs, rdb := beamIssuerFixture(t)
	ctx := context.Background()
	rdb.Set(ctx, BeamIssuerKeyPrefix+beamKidA2, "stale", 0)
	if err := PublishBeamIssuers(ctx, fs, rdb, time.Hour, "owner-a"); err != nil {
		t.Fatal(err)
	}
	rec, ok := readIssuer(t, rdb, beamKidA1)
	if !ok {
		t.Fatal("live key of a managed owner has no record")
	}
	want := map[string]interface{}{
		"v":         float64(1),
		"owner":     "owner-a",
		"pub":       "AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"kits":      []interface{}{"link-a1", "link-a2"},
		"max_conns": float64(16),
	}
	if !reflect.DeepEqual(rec, want) {
		t.Errorf("record %v, want %v", rec, want)
	}
	if _, ok := readIssuer(t, rdb, beamKidA2); ok {
		t.Error("revoked key still has a record")
	}
	if _, ok := readIssuer(t, rdb, beamKidB1); ok {
		t.Error("publishing owner-a wrote owner-b's record")
	}
}

// Every way an owner stops being admitted deletes the records; inside the
// payment grace they stay, as routeOnlyKey keeps the link up.
func TestPublishDeletesOnSuspendAndNoManagedProduct(t *testing.T) {
	past := time.Now().Add(-72 * time.Hour)
	recent := time.Now().Add(-time.Minute)
	overLimit := time.Now().Add(-OverLimitGrace - time.Hour)
	for _, c := range []struct {
		name     string
		billing  *store.UserBilling
		managed  bool
		wantKept bool
	}{
		{"active managed", &store.UserBilling{Status: "active"}, true, true},
		{"no billing row, managed", nil, true, true},
		{"suspended inside the grace", &store.UserBilling{Status: "suspended", SuspendedAt: &recent}, true, true},
		{"suspended past the grace", &store.UserBilling{Status: "suspended", SuspendedAt: &past}, true, false},
		{"admin hold", &store.UserBilling{Status: "suspended", AdminHold: true, SuspendedAt: &recent}, true, false},
		{"over limit past the grace", &store.UserBilling{Status: "active", OverLimitSince: &overLimit}, true, false},
		{"no managed product", &store.UserBilling{Status: "active"}, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs, rdb := beamIssuerFixture(t)
			ctx := context.Background()
			rdb.Set(ctx, BeamIssuerKeyPrefix+beamKidA1, "previously published", 0)
			fs.billing["owner-a"] = c.billing
			fs.managed["owner-a"] = c.managed
			if err := PublishBeamIssuers(ctx, fs, rdb, 48*time.Hour, "owner-a"); err != nil {
				t.Fatal(err)
			}
			if _, ok := readIssuer(t, rdb, beamKidA1); ok != c.wantKept {
				t.Errorf("record present = %v, want %v", ok, c.wantKept)
			}
		})
	}
}

// max_conns follows the limits convention through beam.tenant_max_conns.
func TestPublishBeamIssuersMaxConns(t *testing.T) {
	for _, c := range []struct {
		setting string
		want    interface{} // nil = field absent
	}{
		{"", float64(16)},
		{"unlimited", nil},
		{"0", float64(0)},
		{"40", float64(40)},
		{"garbage", float64(16)},
	} {
		t.Run(c.setting, func(t *testing.T) {
			fs, rdb := beamIssuerFixture(t)
			fs.settings["beam.tenant_max_conns"] = c.setting
			if err := PublishBeamIssuers(context.Background(), fs, rdb, time.Hour, "owner-a"); err != nil {
				t.Fatal(err)
			}
			rec, _ := readIssuer(t, rdb, beamKidA1)
			got, present := rec["max_conns"]
			if (c.want == nil) == present || (present && got != c.want) {
				t.Errorf("max_conns = %v (present %v), want %v", got, present, c.want)
			}
		})
	}
}

// A DB fault leaves Redis alone: a blip must not delete a paying customer's
// records.
func TestPublishBeamIssuersDBFaultKeepsRecords(t *testing.T) {
	fs, rdb := beamIssuerFixture(t)
	ctx := context.Background()
	rdb.Set(ctx, BeamIssuerKeyPrefix+beamKidA1, "previously published", 0)
	fs.keysErr = errors.New("db down")
	if err := PublishBeamIssuers(ctx, fs, rdb, time.Hour, "owner-a"); err == nil {
		t.Fatal("want an error")
	}
	if v, _ := rdb.Get(ctx, BeamIssuerKeyPrefix+beamKidA1).Result(); v != "previously published" {
		t.Errorf("record = %q after a DB fault", v)
	}
}

// One reconcile pass publishes every owner and sweeps records with no row.
func TestBeamIssuerReconcilerPublishesAndSweeps(t *testing.T) {
	fs, rdb := beamIssuerFixture(t)
	ctx := context.Background()
	orphan := "cccccccccccccccccccccccccccccccc"
	rdb.Set(ctx, BeamIssuerKeyPrefix+orphan, "deleted account", 0)
	NewBeamIssuerReconciler(fs, rdb, time.Hour).RunOnce(ctx)
	for _, kid := range []string{beamKidA1, beamKidB1} {
		if _, ok := readIssuer(t, rdb, kid); !ok {
			t.Errorf("%s not published", kid)
		}
	}
	if n, _ := rdb.Exists(ctx, BeamIssuerKeyPrefix+orphan).Result(); n != 0 {
		t.Error("orphan record survived the sweep")
	}
}

func TestValidBeamKid(t *testing.T) {
	for kid, want := range map[string]bool{
		beamKidA1:                           true,
		"":                                  false,
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1":  false,
		beamKidA1 + "0":                     false,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa*":   false,
		"../../aaaaaaaaaaaaaaaaaaaaaaaaaa1": false,
	} {
		if got := ValidBeamKid(kid); got != want {
			t.Errorf("ValidBeamKid(%q) = %v, want %v", kid, got, want)
		}
	}
}
