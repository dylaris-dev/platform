package mods

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers"
)

// TestModpackSettings_ConnectionIDRoundTrips: the modpack settings PUT persists
// modpack_storage_connection_id, GET echoes it, and a 0 clears it.
func TestModpackSettings_ConnectionIDRoundTrips(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	h := newModpackSettingsTestHandler(fs)

	// PUT with a selected connection.
	body, _ := json.Marshal(modpackSettings{Provider: "s3", ConnectionID: 7})
	rw := httptest.NewRecorder()
	h.Set(rw, httptest.NewRequest(http.MethodPut, "/api/admin/settings/modpacks", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("Set status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.kv[handlers.KeyModpackStorageConnectionID] != "7" {
		t.Fatalf("connection id not persisted, got %q", fs.kv[handlers.KeyModpackStorageConnectionID])
	}

	// GET echoes it back.
	rw = httptest.NewRecorder()
	h.Get(rw, httptest.NewRequest(http.MethodGet, "/api/admin/settings/modpacks", nil))
	var got struct {
		Settings modpackSettings `json:"settings"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if got.Settings.ConnectionID != 7 {
		t.Errorf("GET connectionId = %d, want 7", got.Settings.ConnectionID)
	}

	// PUT with 0 clears it.
	body, _ = json.Marshal(modpackSettings{Provider: "s3", ConnectionID: 0})
	rw = httptest.NewRecorder()
	h.Set(rw, httptest.NewRequest(http.MethodPut, "/api/admin/settings/modpacks", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("clear Set status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if fs.kv[handlers.KeyModpackStorageConnectionID] != "" {
		t.Errorf("connection id 0 did not clear the setting, got %q", fs.kv[handlers.KeyModpackStorageConnectionID])
	}
}
