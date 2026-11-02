package guard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/storage"
)

// TestCoreStorageUnavailableResponse pins the two 503s apart. They ask the
// operator for opposite things: one to go and configure storage, the other to
// look at a mount while leaving a correct config alone.
func TestCoreStorageUnavailableResponse(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantError    string
		wantContains string
	}{
		{
			name:         "not configured",
			err:          errors.New("core storage: path is required for the filesystem backend"),
			wantError:    "core_storage_unavailable",
			wantContains: "not configured",
		},
		{
			name:         "no error at all still reads as not configured",
			err:          nil,
			wantError:    "core_storage_unavailable",
			wantContains: "not configured",
		},
		{
			name:         "unreachable",
			err:          fmt.Errorf("%w: stat /mnt/shared: input/output error", storage.ErrBackendUnreachable),
			wantError:    "core_storage_unreachable",
			wantContains: "UNREACHABLE",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rw := httptest.NewRecorder()
			CoreStorageUnavailableResponse(rw, tt.err)

			if rw.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want %d", rw.Code, http.StatusServiceUnavailable)
			}
			// A 404 here would tell the operator the file is gone when the
			// truth is that Core cannot see the mount.
			if rw.Code == http.StatusNotFound || rw.Code == http.StatusInternalServerError {
				t.Fatal("an unreachable backend must not be reported as a missing file or a generic failure")
			}
			// The router-level gate's shape must stay distinguishable from this one.
			if h := rw.Header().Get("X-Feature-Disabled"); h != "" {
				t.Errorf("X-Feature-Disabled = %q; that header belongs to featureDisabledResponse alone", h)
			}

			var body struct {
				Success bool   `json:"success"`
				Error   string `json:"error"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rw.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body %q: %v", rw.Body.String(), err)
			}
			if body.Success {
				t.Error("success = true on a 503")
			}
			if body.Error != tt.wantError {
				t.Errorf("error = %q, want %q", body.Error, tt.wantError)
			}
			if !strings.Contains(body.Message, tt.wantContains) {
				t.Errorf("message = %q, want it to contain %q", body.Message, tt.wantContains)
			}
		})
	}
}
