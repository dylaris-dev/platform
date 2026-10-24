package storageapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"

	"dylaris-core/handlers"

	"dylaris-core/services"
	"dylaris-core/storage"
)

// storageConnBody is the endpoint's shape. Both backend keys must always be
// present, so a panel never has to tell "ok" apart from "not reported".
type storageConnBody struct {
	Path *struct {
		State string  `json:"state"`
		Since *string `json:"since"`
	} `json:"path"`
	S3 *struct {
		State string  `json:"state"`
		Since *string `json:"since"`
	} `json:"s3"`
}

func TestStorageConnectionHandler_GetConnection(t *testing.T) {
	tests := []struct {
		name      string
		status    func() *services.StorageStatus
		wantPath  string
		wantS3    string
		pathSince bool
	}{
		{
			name: "healthy backends report ok with a null since",
			status: func() *services.StorageStatus {
				return services.NewStorageStatus(nil, storage.NewGate(), storage.NewS3Resilience())
			},
			wantPath: "ok",
			wantS3:   "ok",
		},
		{
			name: "a tripped host-path gate reports unavailable",
			status: func() *services.StorageStatus {
				gate := storage.NewGate()
				s := services.NewStorageStatus(nil, gate, storage.NewS3Resilience())
				s.Attach()
				gate.ReportFailure(fmt.Errorf("mount is gone: %w", syscall.EIO))
				return s
			},
			wantPath:  "unavailable",
			wantS3:    "ok",
			pathSince: true,
		},
		{
			name: "no StorageStatus at all still answers both keys",
			// Only ever a test AppState; the nil-safety is what lets the handler
			// call Snapshot unconditionally.
			status:   func() *services.StorageStatus { return nil },
			wantPath: "ok",
			wantS3:   "ok",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewStorageConnectionHandler(&handlers.AppState{StorageStatus: tc.status()})
			rr := httptest.NewRecorder()
			h.GetConnection(rr, httptest.NewRequest(http.MethodGet, "/api/storage/connection", nil))

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rr.Code)
			}

			var raw map[string]json.RawMessage
			if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
				t.Fatalf("unmarshal: %v (body %s)", err, rr.Body.String())
			}
			for _, key := range []string{"path", "s3"} {
				if _, ok := raw[key]; !ok {
					t.Errorf("body is missing the %q key: %s", key, rr.Body.String())
				}
			}

			var body storageConnBody
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal into shape: %v", err)
			}
			if body.Path == nil || body.S3 == nil {
				t.Fatalf("both backends must be objects, got %s", rr.Body.String())
			}
			if body.Path.State != tc.wantPath {
				t.Errorf("path.state = %q, want %q", body.Path.State, tc.wantPath)
			}
			if body.S3.State != tc.wantS3 {
				t.Errorf("s3.state = %q, want %q", body.S3.State, tc.wantS3)
			}
			if tc.pathSince && (body.Path.Since == nil || *body.Path.Since == "") {
				t.Errorf("path.since = %v, want a timestamp", body.Path.Since)
			}
			if !tc.pathSince && body.Path.Since != nil {
				t.Errorf("path.since = %q, want null for an ok state", *body.Path.Since)
			}
		})
	}
}
