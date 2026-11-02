package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/services/corestorage"
	"dylaris-core/storage"
)

// These cover the Redis component reporting WHY it failed rather than only
// that it did, and the decision that follows from it: a failure an operator has
// to fix must not fail the container healthcheck, because the orchestrator
// answers a failed check by restarting a container that cannot fix itself.
//
// NOPERM is not exercised here. miniredis has no ACL command permissions, so it
// cannot produce one; the classifier covers it in database/rediserror_test.go
// against the verbatim reply from a live valkey.

// redisAuthFailing returns a client whose every command is rejected for bad
// credentials. The reply prefixes miniredis emits were checked against a live
// valkey 8 and match (valkey adds "or user is disabled." to WRONGPASS, which
// prefix matching does not care about).
func redisAuthFailing(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	mr.RequireUserAuth("dylaris", "correct-password")
	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(), Username: "dylaris", Password: "wrong-password", MaxRetries: -1,
	})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// redisUnreachable returns a client pointed at an address that was a live
// miniredis a moment ago and is now closed.
func redisUnreachable(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	addr := mr.Addr()
	mr.Close()
	client := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func redisHealthy(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestRedisComponent_ReportsWhyItFailed is the point of the classifier. Every
// failure used to render the single line "Connection failed", which is plainly
// false when the connection is fine and the server answered WRONGPASS - and it
// sent an operator looking at the network instead of at their credentials.
func TestRedisComponent_ReportsWhyItFailed(t *testing.T) {
	tests := []struct {
		name       string
		client     func(*testing.T) *redis.Client
		wantStatus string
		wantCause  string
		wantUp     bool
	}{
		{name: "healthy", client: redisHealthy, wantStatus: "up", wantCause: "", wantUp: true},
		{name: "server is not reachable", client: redisUnreachable, wantStatus: "down", wantCause: "unreachable"},
		{name: "credentials rejected", client: redisAuthFailing, wantStatus: "down", wantCause: "auth"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &HealthHandler{state: &AppState{Redis: tt.client(t)}}

			up := false
			comp := h.redisComponent(context.Background(), &up)

			if comp.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", comp.Status, tt.wantStatus)
			}
			if comp.Cause != tt.wantCause {
				t.Errorf("cause = %q, want %q", comp.Cause, tt.wantCause)
			}
			if up != tt.wantUp {
				t.Errorf("up flag = %v, want %v", up, tt.wantUp)
			}
			if comp.Detail == "" {
				t.Error("detail is empty, want a summary of the failure class")
			}
			if tt.wantCause != "" && comp.Reason == "" {
				t.Error("reason is empty, want the server's own message so an operator can act on it")
			}
		})
	}
}

// TestRedisComponent_DistinguishesAuthFromUnreachable states the property the
// two classes exist for as its own assertion: if their summaries ever collapse
// back to one string, the component stops telling an operator which of two
// completely different fixes applies.
func TestRedisComponent_DistinguishesAuthFromUnreachable(t *testing.T) {
	unreachable := (&HealthHandler{state: &AppState{Redis: redisUnreachable(t)}}).
		redisComponent(context.Background(), new(bool))
	auth := (&HealthHandler{state: &AppState{Redis: redisAuthFailing(t)}}).
		redisComponent(context.Background(), new(bool))

	if unreachable.Detail == auth.Detail {
		t.Fatalf("both failures render the same detail %q", unreachable.Detail)
	}
	if unreachable.Cause == auth.Cause {
		t.Fatalf("both failures render the same cause %q", unreachable.Cause)
	}
}

// newRedisHealthz builds a Healthz-capable handler with healthy storage, so the
// only thing that can move the status code is Redis.
func newRedisHealthz(t *testing.T, client *redis.Client) *HealthHandler {
	t.Helper()
	fs := &healthzFakeStore{coreStorageFakeStore{values: map[string]string{}}}
	return &HealthHandler{state: &AppState{
		Store:       fs,
		Redis:       client,
		CoreStorage: &corestorage.Manager{Store: fs, Redis: client, Gate: storage.NewGate(), S3: storage.NewS3Resilience()},
	}}
}

// TestHealthz_AuthFailureIsReportedButDoesNotGate is the restart-loop fix and
// the load-bearing test of this change. Docker and Swarm answer a failed check
// by killing and restarting the container, and a restart cannot repair a
// rejected credential or a missing ACL grant. Gating on it would restart-loop
// Core over a configuration mistake, taking down the panel an operator needs in
// order to correct it.
func TestHealthz_AuthFailureIsReportedButDoesNotGate(t *testing.T) {
	h := newRedisHealthz(t, redisAuthFailing(t))

	rec := httptest.NewRecorder()
	h.Healthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: a credential failure must not take the container out of rotation", rec.Code, http.StatusOK)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["redis"] != false {
		t.Errorf("redis field = %v, want false: the state has to be visible even though it does not gate", body["redis"])
	}
	if body["status"] != "degraded" {
		t.Errorf("status field = %v, want %q: reporting \"ready\" while Redis rejects every command is a lie", body["status"], "degraded")
	}
}

// TestHealthz_UnreachableRedisStillGates is the other half. This class CAN
// clear on its own and a reschedule may land somewhere it is reachable, so the
// orchestrator is still asked to act.
func TestHealthz_UnreachableRedisStillGates(t *testing.T) {
	h := newRedisHealthz(t, redisUnreachable(t))

	rec := httptest.NewRecorder()
	h.Healthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: an unreachable Redis must still fail readiness", rec.Code, http.StatusServiceUnavailable)
	}
}

// TestHealthz_CarriesNoRedisCause keeps the unauthenticated body coarse. A real
// NOPERM reply names the ACL USERNAME ("NOPERM User dylaris has no permissions
// to run the 'ping' command"), so passing the server's message through here
// would hand an account name to anyone who can reach Core. The classified cause
// and the raw reply belong to the admin endpoint, which requires settings.read.
func TestHealthz_CarriesNoRedisCause(t *testing.T) {
	h := newRedisHealthz(t, redisAuthFailing(t))

	rec := httptest.NewRecorder()
	h.Healthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	body := rec.Body.String()
	for _, leak := range []string{"WRONGPASS", "NOPERM", "NOAUTH", "dylaris", "wrong-password", "auth", "cause", "reason"} {
		if strings.Contains(body, leak) {
			t.Errorf("body contains %q, want no failure detail on an unauthenticated route: %s", leak, body)
		}
	}
}
