package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/services"
)

// newModpackSettingsTestHandler wires a ModpackSettingsHandler against
// coreStorageHTTPFakeStore (core_storage_http_test.go), which records
// SetSetting writes into a map so a rejected request can be asserted to
// never reach the store. FeatureFlags/Events are wired the same way
// core_storage_gate_test.go does for FeatureSettingsHandler, since Set()
// unconditionally invalidates flags and publishes events on success.
func newModpackSettingsTestHandler(fs *coreStorageHTTPFakeStore) *ModpackSettingsHandler {
	st := &AppState{
		Store:  fs,
		Events: services.NewSystemEventsPublisher(nil),
	}
	st.FeatureFlags = services.NewFeatureFlags(st.Store)
	return NewModpackSettingsHandler(st)
}

// TestModpackSettingsHandler_Set_ProviderValidation is the regression guard
// for the dead core-storage branch: "core-storage" must be persistable, and
// an unknown value must be rejected with 400 before it ever reaches the
// store.
func TestModpackSettingsHandler_Set_ProviderValidation(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		wantCode int
	}{
		{"local is accepted", "local", http.StatusOK},
		{"s3 is accepted", "s3", http.StatusOK},
		{"core-storage is accepted", "core-storage", http.StatusOK},
		{"empty defaults to local and is accepted", "", http.StatusOK},
		{"unknown provider is rejected", "ftp", http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := newCoreStorageHTTPFakeStore()
			h := newModpackSettingsTestHandler(fs)

			body, _ := json.Marshal(modpackSettings{Provider: c.provider})
			rw := httptest.NewRecorder()
			h.Set(rw, httptest.NewRequest(http.MethodPut, "/api/admin/settings/modpacks", bytes.NewReader(body)))

			if rw.Code != c.wantCode {
				t.Fatalf("status = %d, want %d (%s)", rw.Code, c.wantCode, rw.Body.String())
			}

			if c.wantCode == http.StatusBadRequest {
				if _, ok := fs.kv["modpack_storage_provider"]; ok {
					t.Errorf("rejected provider %q reached the store, kv = %v", c.provider, fs.kv)
				}
				return
			}
			wantStored := c.provider
			if wantStored == "" {
				wantStored = "local"
			}
			if fs.kv["modpack_storage_provider"] != wantStored {
				t.Errorf("stored provider = %q, want %q", fs.kv["modpack_storage_provider"], wantStored)
			}
		})
	}
}

// TestModpackSettingsHandler_Set_RejectsCredentialsInTheS3Endpoint is the
// modpack half of the endpoint check that already guards core storage. The
// modpack endpoint is persisted verbatim in modpack_storage_s3_endpoint, echoed
// back by Get, and read by modpackBackendLabel, so a credential in its userinfo
// component reaches the settings table, the modpack settings response and the
// migration engine's label material. The cases mirror
// TestValidateCoreStorageConfig_RejectsCredentialsInTheS3Endpoint, and the
// provider column pins that the check does not depend on the provider.
func TestModpackSettingsHandler_Set_RejectsCredentialsInTheS3Endpoint(t *testing.T) {
	const secret = "supersecret"
	cases := []struct {
		name       string
		provider   string
		endpoint   string
		wantReject bool
	}{
		{"credentials in the endpoint", "s3", "https://AKIAEXAMPLE:" + secret + "@minio.internal", true},
		{"user only, no password", "s3", "https://AKIAEXAMPLE@minio.internal", true},
		// A password containing a space defeats url.Parse, which is why the
		// check fails CLOSED on "@" rather than trusting a clean parse.
		{"unparseable credentials", "s3", "https://user:pass word@minio.internal", true},
		{"schemeless credentials", "s3", "AKIAEXAMPLE:" + secret + "@minio.internal", true},
		// The endpoint is persisted whatever the provider is, so a non-s3
		// provider must not be a way around the check.
		{"credentials rejected under provider local", "local", "https://AKIAEXAMPLE:" + secret + "@minio.internal", true},
		{"credentials rejected under provider core-storage", "core-storage", "https://AKIAEXAMPLE:" + secret + "@minio.internal", true},
		// Positive controls: the ordinary shapes must still be accepted.
		{"plain https endpoint", "s3", "https://s3.example.com", false},
		{"endpoint with a port", "s3", "http://minio.internal:9000", false},
		{"empty endpoint (AWS default, region selects the host)", "s3", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := newCoreStorageHTTPFakeStore()
			h := newModpackSettingsTestHandler(fs)

			body, _ := json.Marshal(modpackSettings{Provider: c.provider, S3Endpoint: c.endpoint})
			rw := httptest.NewRecorder()
			h.Set(rw, httptest.NewRequest(http.MethodPut, "/api/admin/settings/modpacks", bytes.NewReader(body)))

			if c.wantReject {
				if rw.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400: endpoint %q persists a credential (%s)", rw.Code, c.endpoint, rw.Body.String())
				}
				if got, ok := fs.kv["modpack_storage_s3_endpoint"]; ok {
					t.Errorf("rejected endpoint reached the store as %q", got)
				}
				// Nothing at all may be written: the writes run as one
				// unguarded loop, so a rejection landing mid-loop would leave
				// the provider switched with the endpoint refused.
				if len(fs.kv) != 0 {
					t.Errorf("rejected request wrote %d setting(s), want 0: %v", len(fs.kv), fs.kv)
				}
				return
			}
			if rw.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", rw.Code, rw.Body.String())
			}
			if fs.kv["modpack_storage_s3_endpoint"] != c.endpoint {
				t.Errorf("stored endpoint = %q, want %q", fs.kv["modpack_storage_s3_endpoint"], c.endpoint)
			}
		})
	}
}

// TestModpackSettingsHandler_Set_AllowsMovingFromAnyStoredValueToValid
// guards the "never trap an operator" constraint: a row that already holds
// some other value (including a stale/unrecognized one) must still be
// updatable to any currently-valid provider, since Set only validates the
// incoming request and never conditions on what is already stored.
func TestModpackSettingsHandler_Set_AllowsMovingFromAnyStoredValueToValid(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	fs.kv["modpack_storage_provider"] = "some-legacy-value"
	h := newModpackSettingsTestHandler(fs)

	body, _ := json.Marshal(modpackSettings{Provider: "core-storage"})
	rw := httptest.NewRecorder()
	h.Set(rw, httptest.NewRequest(http.MethodPut, "/api/admin/settings/modpacks", bytes.NewReader(body)))

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.kv["modpack_storage_provider"] != "core-storage" {
		t.Errorf("stored provider = %q, want core-storage", fs.kv["modpack_storage_provider"])
	}
}
