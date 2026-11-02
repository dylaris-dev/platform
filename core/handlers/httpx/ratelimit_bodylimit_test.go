package httpx

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLimitBody_CapsWhatAnAnonymousCallerCanMakeCoreAllocate is the point of
// the wrapper.
//
// The IP rate limiter bounds how MANY requests an unauthenticated caller may
// send and says nothing about how BIG one may be. Every public handler decodes
// with json.NewDecoder(r.Body).Decode(&req), and the decoder allocates whatever
// a string field actually contains - so a single request carrying a huge value
// was enough to exhaust Core, with no credential and no second request.
func TestLimitBody_CapsWhatAnAnonymousCallerCanMakeCoreAllocate(t *testing.T) {
	const cap = 1024

	var readErr error
	var readN int
	h := LimitBody(cap, func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		readN, readErr = len(b), err
	})

	// A body an order of magnitude over the cap.
	body := strings.Repeat("A", cap*10)
	rw := httptest.NewRecorder()
	h(rw, httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body)))

	if readErr == nil {
		t.Fatalf("the handler read %d bytes with no error; the body was not capped", readN)
	}
	if readN > cap {
		t.Errorf("the handler got %d bytes through a %d-byte cap", readN, cap)
	}
}

// TestLimitBody_OversizedJSONBecomesA400 covers what a real public handler
// does with a capped body: the existing Decode fails, and the existing
// "Invalid JSON" branch answers. No handler needed changing for this.
func TestLimitBody_OversizedJSONBecomesA400(t *testing.T) {
	const cap = 512

	// Same shape as every public auth handler.
	h := LimitBody(cap, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string `json:"username"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	huge := `{"username":"` + strings.Repeat("A", cap*4) + `"}`
	rw := httptest.NewRecorder()
	h(rw, httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(huge)))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an oversized credential body", rw.Code)
	}
}

// TestLimitBody_LeavesRealisticPayloadsAlone is the control. The cap has to be
// invisible to every legitimate credential payload, or it becomes an outage
// instead of a guard.
func TestLimitBody_LeavesRealisticPayloadsAlone(t *testing.T) {
	got := ""
	h := LimitBody(CredentialBodyLimit, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
			TOTP     string `json:"totp"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
		got = req.Username
		w.WriteHeader(http.StatusOK)
	})

	// Deliberately fat for a login: a 200-character password and a long
	// username are still four orders of magnitude under the cap.
	body := `{"username":"` + strings.Repeat("u", 256) + `","password":"` +
		strings.Repeat("p", 200) + `","totp":"123456"}`
	rw := httptest.NewRecorder()
	h(rw, httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body)))

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; a normal login must not be capped (body: %s)", rw.Code, rw.Body.String())
	}
	if len(got) != 256 {
		t.Errorf("username came through as %d chars, want 256", len(got))
	}
}

// TestLimitBody_NilBodyIsSafe: a GET routed through the wrapper must not panic.
func TestLimitBody_NilBodyIsSafe(t *testing.T) {
	called := false
	h := LimitBody(CredentialBodyLimit, func(w http.ResponseWriter, r *http.Request) { called = true })
	req := httptest.NewRequest(http.MethodGet, "/api/auth/login", nil)
	req.Body = nil
	h(httptest.NewRecorder(), req)
	if !called {
		t.Error("handler was not reached for a request with no body")
	}
}
