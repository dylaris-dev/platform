package settings

import (
	"bytes"
	"dylaris-core/handlers/handlertest"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSaveBeamSettings_PersistsUploadLimits guards the two new upload-limit
// settings: SaveBeamSettings must persist them under the beam.* DB keys the
// node-side Redis publish mirrors, and LoadBeamSettings must round-trip them.
// Redis is nil here (the publish block is guarded), so this covers only the
// durable DB path; the live Redis publish is exercised on a real instance.
func TestSaveBeamSettings_PersistsUploadLimits(t *testing.T) {
	fs := handlertest.NewSettingsStore()
	h := &SettingsHandler{d: Deps{Store: fs}}

	maxUpload := int64(5) << 30    // 5 GiB
	dailyUpload := int64(20) << 30 // 20 GiB

	body, _ := json.Marshal(BeamSettings{
		MaxUploadBytes:   &maxUpload,
		DailyUploadBytes: &dailyUpload,
	})
	rw := httptest.NewRecorder()
	h.SaveBeamSettings(rw, httptest.NewRequest(http.MethodPost, "/api/settings/beam", bytes.NewReader(body)))

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}

	if got := fs.KV["beam.max_upload_bytes"]; got != "5368709120" {
		t.Errorf("stored beam.max_upload_bytes = %q, want %q", got, "5368709120")
	}
	if got := fs.KV["beam.daily_upload_bytes"]; got != "21474836480" {
		t.Errorf("stored beam.daily_upload_bytes = %q, want %q", got, "21474836480")
	}

	loaded := h.LoadBeamSettings()
	if loaded.MaxUploadBytes == nil || *loaded.MaxUploadBytes != maxUpload {
		t.Errorf("loaded MaxUploadBytes = %v, want %d", loaded.MaxUploadBytes, maxUpload)
	}
	if loaded.DailyUploadBytes == nil || *loaded.DailyUploadBytes != dailyUpload {
		t.Errorf("loaded DailyUploadBytes = %v, want %d", loaded.DailyUploadBytes, dailyUpload)
	}
}

// A cap of 0 is the operator saying uploads are not allowed. It has to SURVIVE
// the round-trip as a cap: it used to be indistinguishable from "no limit",
// which is the inversion the platform limit convention removes.
func TestSaveBeamSettings_ZeroIsACapOfNone(t *testing.T) {
	fs := handlertest.NewSettingsStore()
	h := &SettingsHandler{d: Deps{Store: fs}}

	zero := int64(0)
	body, _ := json.Marshal(BeamSettings{MaxUploadBytes: &zero, DailyUploadBytes: &zero})
	rw := httptest.NewRecorder()
	h.SaveBeamSettings(rw, httptest.NewRequest(http.MethodPost, "/api/settings/beam", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if got := fs.KV["beam.max_upload_bytes"]; got != "0" {
		t.Errorf("stored beam.max_upload_bytes = %q, want %q", got, "0")
	}
	loaded := h.LoadBeamSettings()
	if loaded.MaxUploadBytes == nil || *loaded.MaxUploadBytes != 0 {
		t.Errorf("loaded MaxUploadBytes = %v, want a cap of 0", loaded.MaxUploadBytes)
	}
}

// An unset limit stays UNSET through a round-trip, so the feature is inert until
// an admin sets a value. It is stored as the "unlimited" sentinel rather than as
// an empty string, because empty means "never saved" and would be re-read as
// whatever default ships - silently replacing the operator's choice.
func TestSaveBeamSettings_UploadLimitsDefaultToNoCap(t *testing.T) {
	fs := handlertest.NewSettingsStore()
	h := &SettingsHandler{d: Deps{Store: fs}}

	body, _ := json.Marshal(BeamSettings{})
	rw := httptest.NewRecorder()
	h.SaveBeamSettings(rw, httptest.NewRequest(http.MethodPost, "/api/settings/beam", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}

	loaded := h.LoadBeamSettings()
	if loaded.MaxUploadBytes != nil || loaded.DailyUploadBytes != nil {
		t.Errorf("defaults = (%v, %v), want no cap on both", loaded.MaxUploadBytes, loaded.DailyUploadBytes)
	}
}
