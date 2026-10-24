package billing

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// Copies of the handlers package's test helpers and fakes that the billing
// tests used while they lived there. Test files cannot be imported across
// packages; each says where the original is.

// staffID, visAdmin, staffRequest: handlers/staff_bounds_test.go and
// handlers/node_visibility_test.go.
const (
	staffID  = "33333333-3333-3333-3333-333333333333"
	visAdmin = "22222222-2222-2222-2222-222222222222"
)

func staffRequest(method string, vars map[string]string, body string) *http.Request {
	r := httptest.NewRequest(method, "/x", bytes.NewBufferString(body))
	ctx := context.WithValue(r.Context(), "isAdmin", false)
	ctx = context.WithValue(ctx, "userID", staffID)
	ctx = context.WithValue(ctx, "username", "staff")
	return mux.SetURLVars(r.WithContext(ctx), vars)
}

// termsStore: handlers/staff_bounds_test.go.
type termsStore struct {
	store.Store
	target *models.User
}

func (f *termsStore) GetUserByID(string) (*models.User, error) { return f.target, nil }
func (f *termsStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{Grant: []string{"plans.write"}}, nil
}
func (f *termsStore) GetPanelRole(int) (*store.PanelRole, error) { return nil, nil }

// voicePoolStore moved here from handlers/voice_stats_test.go; billing is its
// only user now.
type voicePoolStore struct {
	store.Store
	cells []store.RegionUsage
}

func (f *voicePoolStore) GetTrafficUsageRegions(string, time.Time) ([]store.RegionUsage, error) {
	return f.cells, nil
}
func (f *voicePoolStore) GetTrafficLimit(scope, region, kind string) (*models.TrafficLimit, error) {
	if scope != services.TrafficScopeDefault {
		return nil, nil
	}
	gb := int64(100)
	return &models.TrafficLimit{IncludedGB: &gb}, nil
}
func (f *voicePoolStore) GetUserDataBillingEnabled(string) (bool, error) { return false, nil }
func (f *voicePoolStore) GetSetting(string) (string, error)              { return "", nil }

// coreStorageHTTPFakeStore: handlers/core_storage_http_test.go, a settings map.
type coreStorageHTTPFakeStore struct {
	store.Store
	kv map[string]string
}

func newCoreStorageHTTPFakeStore() *coreStorageHTTPFakeStore {
	return &coreStorageHTTPFakeStore{kv: map[string]string{}}
}
func (f *coreStorageHTTPFakeStore) GetSetting(key string) (string, error) { return f.kv[key], nil }
func (f *coreStorageHTTPFakeStore) SetSettingBy(key, value, _ string) error {
	return f.SetSetting(key, value)
}
func (f *coreStorageHTTPFakeStore) SetSetting(key, value string) error {
	f.kv[key] = value
	return nil
}

// newServerPowerRedis returns a real client backed by miniredis.
func newServerPowerRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()})
}
