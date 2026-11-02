package settings

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPermissionsModeHandler_GetMode(t *testing.T) {
	fs := &modeFakeStore{value: "advanced"}
	h := NewPermissionsModeHandler(Deps{Store: fs})
	rec := httptest.NewRecorder()
	h.GetMode(rec, httptest.NewRequest("GET", "/api/authz/mode", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success bool   `json:"success"`
		Mode    string `json:"mode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Success || resp.Mode != "advanced" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestPermissionsModeHandler_SetModeValid(t *testing.T) {
	fs := &modeFakeStore{}
	h := NewPermissionsModeHandler(Deps{Store: fs})
	rec := httptest.NewRecorder()
	h.SetMode(rec, httptest.NewRequest("PUT", "/api/admin/settings/permissions-mode",
		bytes.NewBufferString(`{"mode":"off"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if fs.setKey != "permissions_mode" || fs.setVal != "off" {
		t.Fatalf("persisted (%q=%q), want permissions_mode=off", fs.setKey, fs.setVal)
	}
}

func TestPermissionsModeHandler_SetModeRejectsBogus(t *testing.T) {
	fs := &modeFakeStore{}
	h := NewPermissionsModeHandler(Deps{Store: fs})
	rec := httptest.NewRecorder()
	h.SetMode(rec, httptest.NewRequest("PUT", "/api/admin/settings/permissions-mode",
		bytes.NewBufferString(`{"mode":"bogus"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if fs.setKey != "" {
		t.Fatalf("bogus mode must not persist, got %q=%q", fs.setKey, fs.setVal)
	}
}
