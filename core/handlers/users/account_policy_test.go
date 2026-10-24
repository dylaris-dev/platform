package users

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers"

	"dylaris-core/store"
)

// accountPolicyFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only the methods AccountPolicyHandler touches
// are overridden. Any other call would panic - these tests never make one.
type accountPolicyFakeStore struct {
	store.Store
	allowChange  bool
	cooldownDays int
	getErr       error
	setErr       error
	setCalls     []setPolicyCall
}

type setPolicyCall struct {
	allowChange  bool
	cooldownDays int
}

func (f *accountPolicyFakeStore) GetUserAccountPolicy() (bool, int, error) {
	return f.allowChange, f.cooldownDays, f.getErr
}

func (f *accountPolicyFakeStore) SetUserAccountPolicy(allowChange bool, cooldownDays int) error {
	f.setCalls = append(f.setCalls, setPolicyCall{allowChange, cooldownDays})
	return f.setErr
}

func acctPolicyReq(method string, body []byte, isAdmin bool) *http.Request {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, "/api/admin/settings/users", bytes.NewReader(body))
	} else {
		r = httptest.NewRequest(method, "/api/admin/settings/users", nil)
	}
	return r.WithContext(context.WithValue(r.Context(), "isAdmin", isAdmin))
}

// Phase 4 Task 17: settings.read is now enforced at the route chokepoint
// (RequireCap wrapping in routes.go), not in-handler, so the old pure
// non-admin-forbidden case moved to routes_authz_test.go
// (TestCap_PlatformSettingsPanel), which runs through the real resolver.

func TestAccountPolicyGet_ReturnsStorePolicy(t *testing.T) {
	fs := &accountPolicyFakeStore{allowChange: true, cooldownDays: 30}
	h := NewAccountPolicyHandler(&handlers.AppState{Store: fs})
	rec := httptest.NewRecorder()

	h.Get(rec, acctPolicyReq("GET", nil, true))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Success bool `json:"success"`
		Policy  struct {
			AllowNameChange        bool `json:"allowNameChange"`
			NameChangeCooldownDays int  `json:"nameChangeCooldownDays"`
		} `json:"policy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Success || !out.Policy.AllowNameChange || out.Policy.NameChangeCooldownDays != 30 {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

// Phase 4 Task 17: settings.write is now enforced at the route chokepoint
// (RequireCap wrapping in routes.go), not in-handler, so the old pure
// non-admin-forbidden case moved to routes_authz_test.go
// (TestCap_PlatformSettingsPanel), which runs through the real resolver.

func TestAccountPolicySet_InvalidJSON(t *testing.T) {
	fs := &accountPolicyFakeStore{}
	h := NewAccountPolicyHandler(&handlers.AppState{Store: fs})
	rec := httptest.NewRecorder()

	h.Set(rec, acctPolicyReq("PUT", []byte("not json"), true))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestAccountPolicySet_CooldownRange covers the decision boundary of the
// 0-3650 day validation window.
func TestAccountPolicySet_CooldownRange(t *testing.T) {
	cases := []struct {
		name       string
		days       int
		wantStatus int
		wantCalled bool
	}{
		{"negative is rejected", -1, http.StatusBadRequest, false},
		{"zero is the lower boundary and allowed", 0, http.StatusOK, true},
		{"3650 is the upper boundary and allowed", 3650, http.StatusOK, true},
		{"3651 is just over the upper boundary and rejected", 3651, http.StatusBadRequest, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &accountPolicyFakeStore{}
			h := NewAccountPolicyHandler(&handlers.AppState{Store: fs})
			body, _ := json.Marshal(map[string]interface{}{"allowNameChange": true, "nameChangeCooldownDays": tc.days})
			rec := httptest.NewRecorder()

			h.Set(rec, acctPolicyReq("PUT", body, true))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if got := len(fs.setCalls) == 1; got != tc.wantCalled {
				t.Fatalf("setCalls = %+v, wantCalled %v", fs.setCalls, tc.wantCalled)
			}
		})
	}
}

func TestAccountPolicySet_Success(t *testing.T) {
	fs := &accountPolicyFakeStore{}
	h := NewAccountPolicyHandler(&handlers.AppState{Store: fs})
	body, _ := json.Marshal(map[string]interface{}{"allowNameChange": false, "nameChangeCooldownDays": 14})
	rec := httptest.NewRecorder()

	h.Set(rec, acctPolicyReq("PUT", body, true))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.setCalls) != 1 {
		t.Fatalf("expected 1 store call, got %d", len(fs.setCalls))
	}
	if call := fs.setCalls[0]; call.allowChange || call.cooldownDays != 14 {
		t.Fatalf("call = %+v, want allowChange=false cooldownDays=14", call)
	}
}
