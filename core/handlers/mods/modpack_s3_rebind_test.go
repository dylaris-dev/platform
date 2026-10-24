package mods

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers"
)

func TestModpackS3SecretRebound(t *testing.T) {
	stored := map[string]string{
		"modpack_storage_s3_endpoint":   "https://s3.eu-central-1.amazonaws.com",
		"modpack_storage_s3_bucket":     "dylaris-packs",
		"modpack_storage_s3_access_key": "AKIAREAL",
		"modpack_storage_s3_secret_key": "the-real-secret",
	}
	base := modpackSettings{
		S3Endpoint:  stored["modpack_storage_s3_endpoint"],
		S3Bucket:    stored["modpack_storage_s3_bucket"],
		S3AccessKey: stored["modpack_storage_s3_access_key"],
	}
	get := func(k string) string { return stored[k] }

	cases := []struct {
		name   string
		mutate func(*modpackSettings)
		noSec  bool // drop the stored secret for this case
		want   bool
	}{
		{"unchanged identity, blank secret", func(*modpackSettings) {}, false, false},
		{"endpoint moved", func(s *modpackSettings) { s.S3Endpoint = "https://attacker.example" }, false, true},
		{"bucket moved", func(s *modpackSettings) { s.S3Bucket = "someone-elses" }, false, true},
		{"access key moved", func(s *modpackSettings) { s.S3AccessKey = "AKIAOTHER" }, false, true},
		{"a submitted secret is a rotation, always allowed", func(s *modpackSettings) {
			s.S3Endpoint = "https://attacker.example"
			s.S3SecretKey = "brand-new"
		}, false, false},
		{"whitespace-only secret is still blank", func(s *modpackSettings) {
			s.S3Endpoint = "https://attacker.example"
			s.S3SecretKey = "   "
		}, false, true},
		{"nothing stored means nothing to rebind", func(s *modpackSettings) {
			s.S3Endpoint = "https://attacker.example"
		}, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := base
			c.mutate(&req)
			g := get
			if c.noSec {
				g = func(k string) string {
					if k == "modpack_storage_s3_secret_key" {
						return ""
					}
					return stored[k]
				}
			}
			if got := modpackS3SecretRebound(req, g); got != c.want {
				t.Fatalf("modpackS3SecretRebound = %v, want %v", got, c.want)
			}
		})
	}
}

// End-to-end on the wire for the modpack surface: the refusal has to land
// before the settings writes, or the endpoint is already rebound by the time
// the caller sees the 400.
func TestModpackSettingsSet_RefusesARebindOnTheWire(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	fs.kv["modpack_storage_s3_endpoint"] = "https://s3.example"
	fs.kv["modpack_storage_s3_bucket"] = "packs"
	fs.kv["modpack_storage_s3_access_key"] = "AKIAREAL"
	fs.kv["modpack_storage_s3_secret_key"] = "the-real-secret"
	h := NewModpackSettingsHandler(&handlers.AppState{Store: fs})

	body, _ := json.Marshal(modpackSettings{
		Provider:    "s3",
		S3Endpoint:  "https://attacker.example",
		S3Bucket:    "packs",
		S3AccessKey: "AKIAREAL",
		// no S3SecretKey: the caller cannot read it, and is not supplying one
	})
	rw := httptest.NewRecorder()
	h.Set(rw, httptest.NewRequest(http.MethodPut, "/api/admin/settings/modpacks", bytes.NewReader(body)))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rw.Code, rw.Body.String())
	}
	if got := fs.kv["modpack_storage_s3_endpoint"]; got != "https://s3.example" {
		t.Fatalf("endpoint was rebound anyway: %q", got)
	}
	if got := fs.kv["modpack_storage_s3_secret_key"]; got != "the-real-secret" {
		t.Fatalf("stored secret changed: %q", got)
	}
}
