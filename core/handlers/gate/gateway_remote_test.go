package gate

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/services"
)

func TestRequireLocalGateway(t *testing.T) {
	mode := func(remote, known bool) func() (bool, bool) {
		return func() (bool, bool) { return remote, known }
	}
	tests := []struct {
		name     string
		mode     func() (bool, bool)
		wantNext bool
		status   int
		code     string
	}{
		{"nil mode is local", nil, true, http.StatusOK, ""},
		{"local", mode(false, true), true, http.StatusOK, ""},
		{"remote", mode(true, true), false, http.StatusConflict, "gateway_remote"},
		{"unknown", mode(false, false), false, http.StatusServiceUnavailable, "gateway_provider_unknown"},
		{"unknown with a stale remote answer", mode(true, false), false, http.StatusServiceUnavailable, "gateway_provider_unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			h := Gates{GatewayRemote: tt.mode}.RequireLocalGateway(func(w http.ResponseWriter, r *http.Request) {
				called = true
			})
			rec := httptest.NewRecorder()
			h(rec, httptest.NewRequest("GET", "/x", nil))
			if called != tt.wantNext || rec.Code != tt.status {
				t.Fatalf("next called=%v status=%d, want %v %d (body %s)", called, rec.Code, tt.wantNext, tt.status, rec.Body)
			}
			if tt.code == "" {
				return
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["code"] != tt.code || body["success"] != false || body["error"] == "" {
				t.Fatalf("body %v, want code %q", body, tt.code)
			}
		})
	}
}

func TestGatewayModeError(t *testing.T) {
	tests := []struct {
		err     error
		handled bool
		status  int
		code    string
	}{
		{nil, false, 0, ""},
		{errors.New("other"), false, 0, ""},
		{fmt.Errorf("create: %w", services.ErrGatewayRemote), true, http.StatusConflict, "gateway_remote"},
		{services.ErrGatewayProviderUnknown, true, http.StatusServiceUnavailable, "gateway_provider_unknown"},
		{fmt.Errorf("route: %w", services.ErrGatewayChangePending), true, http.StatusConflict, "change_pending"},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		if got := GatewayModeError(rec, tt.err); got != tt.handled {
			t.Fatalf("%v: handled=%v, want %v", tt.err, got, tt.handled)
		}
		if !tt.handled {
			if rec.Body.Len() != 0 {
				t.Fatalf("%v: unhandled error wrote %q", tt.err, rec.Body)
			}
			continue
		}
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != tt.status || body["code"] != tt.code {
			t.Fatalf("%v: %d %v, want %d %s", tt.err, rec.Code, body, tt.status, tt.code)
		}
	}
}
