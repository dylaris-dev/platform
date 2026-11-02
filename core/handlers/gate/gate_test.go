package gate

import (
	"context"
	"dylaris-core/handlers/handlertest"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/corestorage"
	"net/http"
	"net/http/httptest"
	"testing"
)

// modpackGateFakeStore adds the user lookup RequireUserCanCreateModpacks needs
// on top of a settings map.
type modpackGateFakeStore struct {
	*handlertest.SettingsStore
	user *models.User
}

func (f *modpackGateFakeStore) GetUserByID(string) (*models.User, error) { return f.user, nil }

// adminCtx carries the identity the auth middleware would have set.
func adminCtx(userID string, isAdmin bool) context.Context {
	ctx := context.WithValue(context.Background(), "userID", userID)
	return context.WithValue(ctx, "isAdmin", isAdmin)
}

func TestRequireCoreStorageConfigured(t *testing.T) {
	called := false
	inner := func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(http.StatusOK) }

	t.Run("blocks when unconfigured", func(t *testing.T) {
		called = false
		fs := &handlertest.SettingsStore{KV: map[string]string{}}
		st := Gates{Store: fs, CoreStorage: &corestorage.Manager{Store: fs}}
		rw := httptest.NewRecorder()
		st.RequireCoreStorageConfigured(inner)(rw, httptest.NewRequest(http.MethodPost, "/x", nil))
		if called {
			t.Fatal("inner ran despite unconfigured storage")
		}
		if rw.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rw.Code)
		}
	})

	t.Run("passes when configured (path)", func(t *testing.T) {
		called = false
		fs := &handlertest.SettingsStore{KV: map[string]string{
			corestorage.KeyBackend:     "path",
			corestorage.KeyPath:        "/mnt/shared",
			corestorage.KeyPathConfirm: "true",
		}}
		st := Gates{Store: fs, CoreStorage: &corestorage.Manager{Store: fs}}
		rw := httptest.NewRecorder()
		st.RequireCoreStorageConfigured(inner)(rw, httptest.NewRequest(http.MethodPost, "/x", nil))
		if !called {
			t.Fatalf("inner did not run; status = %d body %s", rw.Code, rw.Body.String())
		}
	})
}

// TestRequireBYONEnabled covers the BYON route gate: passthrough when the
// platform-wide feature_byon_enabled flag is on, blocked (503 feature_disabled)
// when it is off or unset. Settings come from handlertest.SettingsStore.
func TestRequireBYONEnabled(t *testing.T) {
	cases := []struct {
		name       string
		flag       string // stored feature_byon_enabled ("" = unset -> default off)
		wantCalled bool
		wantCode   int
	}{
		{"enabled passes through", "true", true, http.StatusOK},
		{"disabled blocks", "false", false, http.StatusServiceUnavailable},
		{"unset blocks (default off)", "", false, http.StatusServiceUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kv := map[string]string{}
			if c.flag != "" {
				kv["feature_byon_enabled"] = c.flag
			}
			fs := &handlertest.SettingsStore{KV: kv}
			st := Gates{Store: fs, FeatureFlags: services.NewFeatureFlags(fs)}

			called := false
			inner := func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(http.StatusOK) }
			rw := httptest.NewRecorder()
			st.RequireBYONEnabled(inner)(rw, httptest.NewRequest(http.MethodGet, "/admin/usage", nil))

			if called != c.wantCalled {
				t.Fatalf("inner called = %v, want %v", called, c.wantCalled)
			}
			if rw.Code != c.wantCode {
				t.Fatalf("status = %d, want %d (%s)", rw.Code, c.wantCode, rw.Body.String())
			}
			if !c.wantCalled && rw.Header().Get("X-Feature-Disabled") != FeatureBYON {
				t.Fatalf("X-Feature-Disabled = %q, want %q", rw.Header().Get("X-Feature-Disabled"), FeatureBYON)
			}
		})
	}
}

// TestRequireUserCanCreateModpacks_FlagIsACeiling is the security-relevant half
// of the split. The per-user column is an exception mechanism, not an override:
// a row set to true by hand (and therefore marked manual, so the bulk apply
// leaves it alone) must STILL lose authoring when the admin closes it platform
// wide. Checking only the column would leave those users authoring after the
// switch went off, which is the opposite of what closing it means.
func TestRequireUserCanCreateModpacks_FlagIsACeiling(t *testing.T) {
	cases := []struct {
		name        string
		authoring   string
		isAdmin     bool
		userCan     bool
		userManual  bool
		wantCalled  bool
		wantCode    int
		wantFeature string
	}{
		{"authoring on and user allowed", "true", false, true, false, true, http.StatusOK, ""},
		{"authoring on but user revoked", "true", false, false, true, false, http.StatusServiceUnavailable, "modpacks_user"},
		{"authoring off blocks a hand-allowed user", "false", false, true, true, false, http.StatusServiceUnavailable, FeatureModpackAuthoring},
		{"authoring unset blocks (default off)", "", false, true, false, false, http.StatusServiceUnavailable, FeatureModpackAuthoring},
		{"admin bypasses both", "false", true, false, false, true, http.StatusOK, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kv := map[string]string{}
			if c.authoring != "" {
				kv["feature_modpack_authoring_enabled"] = c.authoring
			}
			fake := &modpackGateFakeStore{
				SettingsStore: &handlertest.SettingsStore{KV: kv},
				user: &models.User{
					ID: "u1", CanCreateModpacks: c.userCan, CanCreateModpacksManual: c.userManual,
				},
			}
			st := Gates{Store: fake, FeatureFlags: services.NewFeatureFlags(fake)}

			called := false
			inner := func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(http.StatusOK) }
			req := httptest.NewRequest(http.MethodPost, "/api/me/packs", nil).WithContext(adminCtx("u1", c.isAdmin))
			rw := httptest.NewRecorder()
			st.RequireUserCanCreateModpacks(inner)(rw, req)

			if called != c.wantCalled {
				t.Fatalf("inner called = %v, want %v (%s)", called, c.wantCalled, rw.Body.String())
			}
			if rw.Code != c.wantCode {
				t.Fatalf("status = %d, want %d (%s)", rw.Code, c.wantCode, rw.Body.String())
			}
			if c.wantFeature != "" && rw.Header().Get("X-Feature-Disabled") != c.wantFeature {
				t.Errorf("X-Feature-Disabled = %q, want %q - the two refusals must stay distinguishable",
					rw.Header().Get("X-Feature-Disabled"), c.wantFeature)
			}
		})
	}
}

func TestRequireTemplatesAudience(t *testing.T) {
	tests := []struct {
		audience string
		admin    bool
		want     int
	}{
		{"off", true, 200}, {"off", false, 403},
		{"admin", true, 200}, {"admin", false, 403},
		{"all", true, 200}, {"all", false, 200},
	}
	for _, tc := range tests {
		st := &handlertest.SettingsStore{KV: map[string]string{services.TemplatesSettingKey: tc.audience}}
		state := Gates{Store: st, FeatureFlags: services.NewFeatureFlags(st)}
		h := state.RequireTemplatesAudience(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
		req := httptest.NewRequest("GET", "/api/templates", nil)
		req = req.WithContext(context.WithValue(req.Context(), "isAdmin", tc.admin))
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != tc.want {
			t.Errorf("audience=%s admin=%v: got %d, want %d", tc.audience, tc.admin, rec.Code, tc.want)
		}
	}
}
