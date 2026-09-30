package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/store"
)

type serverSettingsFakeStore struct {
	store.Store
	settings map[string]string
}

func (f *serverSettingsFakeStore) GetSetting(key string) (string, error) { return f.settings[key], nil }
func (f *serverSettingsFakeStore) SetSetting(key, value string) error {
	f.settings[key] = value
	return nil
}

func saveServerSettings(t *testing.T, fs *serverSettingsFakeStore, body string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/settings/servers", bytes.NewReader([]byte(body)))
	NewSettingsHandler(&AppState{Store: fs}).SaveServerSettings(rec, req)
	return rec.Code
}

// The scheduled-task cap travels the same convention as the sub-server cap:
// never saved means the product default, 0 is a real "none", null is no cap -
// and a save of one field must not lose the other.
func TestServerSettingsCarryTheScheduledTaskLimit(t *testing.T) {
	fs := &serverSettingsFakeStore{settings: map[string]string{}}
	h := NewSettingsHandler(&AppState{Store: fs})

	got := h.LoadServerSettings()
	if got.MaxScheduledTasks == nil || *got.MaxScheduledTasks != 25 {
		t.Fatalf("never saved: MaxScheduledTasks = %v, want the default 25", got.MaxScheduledTasks)
	}

	if code := saveServerSettings(t, fs, `{"maxSubServers":3,"maxScheduledTasks":0}`); code != http.StatusOK {
		t.Fatalf("save status = %d", code)
	}
	got = h.LoadServerSettings()
	if got.MaxScheduledTasks == nil || *got.MaxScheduledTasks != 0 {
		t.Errorf("saved 0: MaxScheduledTasks = %v, want 0 (none), not the default and not unlimited", got.MaxScheduledTasks)
	}
	if got.MaxSubServers == nil || *got.MaxSubServers != 3 {
		t.Errorf("MaxSubServers = %v, want 3", got.MaxSubServers)
	}

	if code := saveServerSettings(t, fs, `{"maxSubServers":3,"maxScheduledTasks":null}`); code != http.StatusOK {
		t.Fatalf("save status = %d", code)
	}
	if got = h.LoadServerSettings(); got.MaxScheduledTasks != nil {
		t.Errorf("saved null: MaxScheduledTasks = %d, want no cap", *got.MaxScheduledTasks)
	}

	if code := saveServerSettings(t, fs, `{"maxSubServers":3,"maxScheduledTasks":-1}`); code != http.StatusBadRequest {
		t.Errorf("a negative cap was accepted: status %d", code)
	}

	var resp struct {
		Settings ServerSettings `json:"settings"`
	}
	rec := httptest.NewRecorder()
	h.GetServerSettings(rec, httptest.NewRequest(http.MethodGet, "/api/settings/servers", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Settings.MaxSubServers == nil || *resp.Settings.MaxSubServers != 3 {
		t.Errorf("GET lost maxSubServers: %v", resp.Settings.MaxSubServers)
	}
}
