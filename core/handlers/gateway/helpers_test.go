package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/handlers"
	"dylaris-core/handlers/handlertest"
	"dylaris-core/services"
	"dylaris-core/store"
)

// Copies of handlers test fixtures the gateway tests use. Test files cannot be
// imported across packages; each keeps its original name so the source is easy
// to find with a search.

func decodeCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out.Code
}

// newEntGateState builds a BYON-on state. storeEnabled is what separates a
// hosted install (entitlement required) from a self-host one (everything
// allowed), and getting it wrong in either direction is the whole risk here.
func newEntGateState(billing *store.UserBilling, storeEnabled bool) *handlers.AppState {
	fs := &entGateStore{
		settings: map[string]string{"feature_byon_enabled": "true"},
		billing:  billing,
	}
	return &handlers.AppState{
		Store:        fs,
		FeatureFlags: services.NewFeatureFlags(fs),
		StoreEnabled: storeEnabled,
		// Deliberately blank: an unreachable storefront must not turn the refusal
		// into a "you are not connected" claim we cannot support. See
		// storeLinkedBestEffort.
		StoreURL: "",
	}
}

// entGateStore serves the two lookups the gate makes.
type entGateStore struct {
	store.Store
	settings map[string]string
	billing  *store.UserBilling
}

func (f *entGateStore) GetSetting(key string) (string, error) { return f.settings[key], nil }

func (f *entGateStore) GetUserBilling(string) (*store.UserBilling, error) {
	return f.billing, nil
}

func (f *entGateStore) CountNodesByOwner(string) (int, error) { return 0, nil }

func (f *entGateStore) CountPendingNodeEnrollTokens(string) (int, error) { return 0, nil }

// Both pending kinds, because services.NodeSlotsUsed asks for both: a tenant can
// reach a node through an enroll token OR a warp key, and a fake that answers
// only the one this file is about would let the mint gate be tested against a
// number the production gate never sees.
func (f *entGateStore) CountNodeWarpKeysByOwner(string) (int, error) { return 0, nil }

func (f *entGateStore) CreateNodeEnrollToken(string, string, string, *time.Time, string) error {
	return nil
}

func ptrI64(i int64) *int64 { return &i }

func voiceNodeState(t *testing.T, fs store.Store, path, fixture string) (*handlers.AppState, *miniredis.Miniredis, *handlertest.VoiceNode) {
	t.Helper()
	reg := nodegrpc.NewRegistry()
	node := handlertest.NewVoiceNode(t, reg, path, fixture)
	mr := miniredis.RunT(t)
	return &handlers.AppState{Store: fs, GRPCRegistry: reg, Redis: redis.NewClient(&redis.Options{Addr: mr.Addr()})}, mr, node
}
