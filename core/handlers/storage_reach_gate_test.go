package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/services/storagereach"
)

func TestRequireCoreStorageReachable_PassesWhenHealthy(t *testing.T) {
	s := &AppState{StorageReach: storagereach.NewService(storagereach.ServiceDeps{CoreID: "core-a"})}
	called := false
	h := s.RequireCoreStorageReachable(func(http.ResponseWriter, *http.Request) { called = true })

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/api/library", nil))

	if !called {
		t.Fatal("the handler was not reached on a healthy Core")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestRequireCoreStorageReachable_PassesWhenTheVerifierIsAbsent(t *testing.T) {
	// Tooling and tests build an AppState without the verifier. That must
	// behave exactly as it did before this feature existed, not deny
	// everything.
	s := &AppState{}
	called := false
	h := s.RequireCoreStorageReachable(func(http.ResponseWriter, *http.Request) { called = true })

	h(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/library", nil))

	if !called {
		t.Fatal("a nil verifier gated the request")
	}
}

func TestRequireCoreStorageReachable_503sWithTheTaxonomyReason(t *testing.T) {
	svc := storagereach.NewService(storagereach.ServiceDeps{CoreID: "core-a"})
	// Distinctive, obviously-internal detail: two of the eight gated routes
	// are open to any authenticated user with no capability check, so this
	// string must never reach the response body (see the assertion below).
	const rawDetail = "/mnt/nfs/dylaris-data: permission denied"
	svc.Status().Set(storagereach.StatusNotShared, rawDetail)
	s := &AppState{StorageReach: svc}

	called := false
	h := s.RequireCoreStorageReachable(func(http.ResponseWriter, *http.Request) { called = true })
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/api/library", nil))

	if called {
		t.Fatal("the handler ran on a Core that cannot reach shared storage")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rec.Body.String())
	}
	if body["reason"] != string(storagereach.StatusNotShared) {
		t.Errorf("reason = %v, want %q", body["reason"], storagereach.StatusNotShared)
	}
	msg, _ := body["message"].(string)
	if msg == "" {
		t.Error("message is empty; the operator is told nothing actionable")
	}
	if strings.Contains(rec.Body.String(), rawDetail) {
		t.Errorf("response body leaks the raw backend error text: %s", rec.Body.String())
	}
}

func TestRequireCoreStorageReachable_EveryStatusHasAMessage(t *testing.T) {
	// A taxonomy value with no copy would render as a blank 503 body, which
	// is the exact "silent failure" this feature exists to remove.
	for _, st := range []storagereach.Status{
		storagereach.StatusOffline,
		storagereach.StatusNoResponse,
		storagereach.StatusUnreachable,
		storagereach.StatusWriteDenied,
		storagereach.StatusNotShared,
		storagereach.StatusFingerprintMismatch,
		storagereach.StatusCrossWriteDenied,
	} {
		t.Run(string(st), func(t *testing.T) {
			if got := storageReachMessage(st); got == "" {
				t.Fatalf("storageReachMessage(%s) is empty", st)
			}
		})
	}
}
