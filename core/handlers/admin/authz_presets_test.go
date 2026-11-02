package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthzHandler_Presets(t *testing.T) {
	h := NewAuthzHandler()
	rec := httptest.NewRecorder()
	h.Presets(rec, httptest.NewRequest("GET", "/api/authz/presets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success bool `json:"success"`
		Presets []struct {
			ID           string   `json:"id"`
			Capabilities []string `json:"capabilities"`
		} `json:"presets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Success || len(resp.Presets) < 4 {
		t.Fatalf("expected >=4 presets, got %+v", resp)
	}
	for _, p := range resp.Presets {
		if p.ID == "" || len(p.Capabilities) == 0 {
			t.Fatalf("preset missing id/caps: %+v", p)
		}
	}
}
