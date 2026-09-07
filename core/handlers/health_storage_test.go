package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/storage"
)

// These cover core storage's appearance in the two health endpoints, and the
// three decisions that shape it: a storage outage is degraded rather than down,
// it never gates /healthz, and the unauthenticated body carries no cause.

// probeFailProvider fails every write with a transport error and does nothing
// else. It exists to trip an S3Resilience from outside the storage package,
// which has no exported way to set the state directly. WriteFile is the one
// operation that reports and returns rather than retrying, so tripping through
// it takes no time at all.
type probeFailProvider struct{ err error }

func (p *probeFailProvider) WriteFile(context.Context, string, io.Reader) error { return p.err }
func (p *probeFailProvider) ListFiles(context.Context, string) ([]storage.FileInfo, error) {
	return nil, p.err
}
func (p *probeFailProvider) GetFile(context.Context, string) (io.ReadCloser, error) {
	return nil, p.err
}
func (p *probeFailProvider) DeletePath(context.Context, string) error          { return p.err }
func (p *probeFailProvider) CreateDir(context.Context, string) error           { return p.err }
func (p *probeFailProvider) CopyToLocal(context.Context, string, string) error { return p.err }
func (p *probeFailProvider) DownloadURL(context.Context, string, time.Duration) (string, error) {
	return "", p.err
}

func (p *probeFailProvider) UploadURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

// reconnectingS3 returns an S3Resilience already in the reconnecting state,
// carrying an error that names a host so the leak tests have something to look
// for.
func reconnectingS3(t *testing.T) *storage.S3Resilience {
	t.Helper()
	res := storage.NewS3Resilience()
	inner := &probeFailProvider{err: &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: &net.AddrError{Err: "connection refused", Addr: "objects.internal.example:9000"},
	}}
	prov := storage.NewS3ResilientProvider(inner, res)
	_ = prov.WriteFile(context.Background(), "k", strings.NewReader("x"))
	if reconnecting, _, _ := res.State(); !reconnecting {
		t.Fatal("setup: the resilience wrapper did not enter reconnecting on a transport failure")
	}
	return res
}

func TestStorageComponent_ReportsThePerBackendState(t *testing.T) {
	tests := []struct {
		name       string
		values     map[string]string
		gate       func(*testing.T) *storage.Gate
		s3         func(*testing.T) *storage.S3Resilience
		wantStatus string
		wantReason string // substring, empty means do not check
	}{
		{
			name:       "no backend configured is disabled, not a fault",
			values:     map[string]string{},
			wantStatus: "disabled",
		},
		{
			name: "host path with no evidence of a problem",
			values: map[string]string{
				keyCoreStorageBackend: "path",
				keyCoreStoragePath:    "/mnt/shared",
			},
			wantStatus: "up",
		},
		{
			name: "host path not answering",
			values: map[string]string{
				keyCoreStorageBackend: "path",
				keyCoreStoragePath:    "/mnt/shared",
			},
			gate:       unhealthyGate,
			wantStatus: "degraded",
			// The admin endpoint is capability-gated, so unlike the SSE payload
			// it is allowed to name the path and the errno.
			wantReason: "/mnt/shared",
		},
		{
			name: "s3 with no reported failure",
			values: map[string]string{
				keyCoreStorageBackend:  "s3",
				keyCoreStorageS3Bucket: "my-bucket",
			},
			wantStatus: "up",
		},
		{
			name: "s3 reconnecting",
			values: map[string]string{
				keyCoreStorageBackend:  "s3",
				keyCoreStorageS3Bucket: "my-bucket",
			},
			s3:         reconnectingS3,
			wantStatus: "degraded",
			wantReason: "objects.internal.example:9000",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &AppState{Store: &coreStorageFakeStore{values: tc.values}}
			if tc.gate != nil {
				s.StorageGate = tc.gate(t)
			}
			if tc.s3 != nil {
				s.StorageS3 = tc.s3(t)
			}

			comp := (&HealthHandler{state: s}).storageComponent()

			if comp.Key != "storage" {
				t.Errorf("Key = %q, want %q", comp.Key, "storage")
			}
			if comp.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q (reason: %q)", comp.Status, tc.wantStatus, comp.Reason)
			}
			if tc.wantReason != "" && !strings.Contains(comp.Reason, tc.wantReason) {
				t.Errorf("Reason = %q, want it to contain %q", comp.Reason, tc.wantReason)
			}
		})
	}
}

// TestStorageComponent_IsNeverDown pins the decision rather than the wording.
// "down" is reserved for the dependencies Core cannot run without: overallStatus
// maps a down database or redis to a down PLATFORM, and storage does not belong
// in that set. With storage unreachable the panel still serves, the API still
// answers and running servers keep running; only file features fail.
func TestStorageComponent_IsNeverDown(t *testing.T) {
	backends := []struct {
		name   string
		values map[string]string
		gate   *storage.Gate
		s3     *storage.S3Resilience
	}{
		{
			name:   "host path wedged",
			values: map[string]string{keyCoreStorageBackend: "path", keyCoreStoragePath: "/mnt/shared"},
			gate:   unhealthyGate(t),
		},
		{
			name:   "s3 unreachable",
			values: map[string]string{keyCoreStorageBackend: "s3", keyCoreStorageS3Bucket: "b"},
			s3:     reconnectingS3(t),
		},
	}

	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			s := &AppState{Store: &coreStorageFakeStore{values: b.values}, StorageGate: b.gate, StorageS3: b.s3}
			comp := (&HealthHandler{state: s}).storageComponent()

			if comp.Status == "down" {
				t.Fatalf("storage reported %q; a storage outage must degrade the platform, not declare it down", comp.Status)
			}
			if comp.Status != "degraded" {
				t.Fatalf("Status = %q, want %q", comp.Status, "degraded")
			}
			if got := overallStatus([]healthComponent{comp}); got != "degraded" {
				t.Fatalf("overallStatus = %q, want %q", got, "degraded")
			}
		})
	}
}

// healthzFakeStore adds the one method Healthz needs beyond the settings reads
// coreStorageFakeStore already covers. A DB that answers is the point: it keeps
// storage the only variable in these tests.
type healthzFakeStore struct {
	coreStorageFakeStore
}

func (f *healthzFakeStore) Ping(context.Context) error { return nil }

// newHealthzHandler wires a handler whose DB and Redis both answer, so the only
// variable left is storage.
func newHealthzHandler(t *testing.T, values map[string]string, gate *storage.Gate, s3 *storage.S3Resilience) *HealthHandler {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return &HealthHandler{state: &AppState{
		Store:       &healthzFakeStore{coreStorageFakeStore{values: values}},
		Redis:       redis.NewClient(&redis.Options{Addr: mr.Addr()}),
		StorageGate: gate,
		StorageS3:   s3,
	}}
}

// TestHealthz_StorageIsReportedButNeverGates is the load-bearing one. Docker and
// Swarm consume this endpoint to decide whether to kill and restart the
// container, and restarting Core cannot make an unreachable NAS reachable.
// Gating on storage would turn a mount blip into a restart loop that takes the
// panel and every running server's supervision down with it.
func TestHealthz_StorageIsReportedButNeverGates(t *testing.T) {
	h := newHealthzHandler(t, map[string]string{
		keyCoreStorageBackend: "path",
		keyCoreStoragePath:    "/mnt/shared",
	}, unhealthyGate(t), nil)

	rec := httptest.NewRecorder()
	h.Healthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: storage must not take the container out of rotation", rec.Code, http.StatusOK)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["status"] != "ready" {
		t.Errorf("status field = %v, want %q", body["status"], "ready")
	}
	if body["storage"] != false {
		t.Errorf("storage field = %v, want false: the state has to be visible even though it does not gate", body["storage"])
	}
}

func TestHealthz_HealthyStorageReportsTrue(t *testing.T) {
	h := newHealthzHandler(t, map[string]string{
		keyCoreStorageBackend: "path",
		keyCoreStoragePath:    "/mnt/shared",
	}, storage.NewGate(), storage.NewS3Resilience())

	rec := httptest.NewRecorder()
	h.Healthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["storage"] != true {
		t.Errorf("storage field = %v, want true", body["storage"])
	}
}

// TestHealthz_CarriesNoCause: this route is registered outside AuthMiddleware,
// so anyone who can reach Core can read the body. The coarse flag is the whole
// budget; the path, the errno, the bucket and the endpoint stay in the
// capability-gated admin endpoint.
func TestHealthz_CarriesNoCause(t *testing.T) {
	h := newHealthzHandler(t, map[string]string{
		keyCoreStorageBackend:  "s3",
		keyCoreStorageS3Bucket: "my-bucket",
	}, unhealthyGate(t), reconnectingS3(t))

	rec := httptest.NewRecorder()
	h.Healthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	body := rec.Body.String()
	for _, secret := range []string{"/mnt/shared", "objects.internal.example", "my-bucket", "connection refused"} {
		if strings.Contains(body, secret) {
			t.Errorf("unauthenticated /healthz body leaked %q; body was: %s", secret, body)
		}
	}
}

// TestSyncStorageGate_DropsStaleS3State closes the mirror of the stale-gate
// problem. An s3 state left reconnecting after a switch to a host path can
// never be cleared by anything else, because the only thing that clears it is a
// successful s3 call and there will not be another one. It would sit in the
// panel banner and both health endpoints forever.
func TestSyncStorageGate_DropsStaleS3State(t *testing.T) {
	tests := []struct {
		name             string
		values           map[string]string
		wantReconnecting bool
	}{
		{
			name: "switched to a host path",
			values: map[string]string{
				keyCoreStorageBackend: "path",
				keyCoreStoragePath:    "/mnt/shared",
			},
			wantReconnecting: false,
		},
		{
			name:             "nothing configured",
			values:           map[string]string{},
			wantReconnecting: false,
		},
		{
			name: "still s3, so a real outage survives",
			values: map[string]string{
				keyCoreStorageBackend:  "s3",
				keyCoreStorageS3Bucket: "my-bucket",
			},
			wantReconnecting: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &AppState{
				Store:       &coreStorageFakeStore{values: tc.values},
				StorageGate: storage.NewGate(),
				StorageS3:   reconnectingS3(t),
			}
			s.SyncStorageGate()

			got, _, _ := s.StorageS3.State()
			if got != tc.wantReconnecting {
				t.Fatalf("reconnecting = %v, want %v", got, tc.wantReconnecting)
			}
		})
	}
}
