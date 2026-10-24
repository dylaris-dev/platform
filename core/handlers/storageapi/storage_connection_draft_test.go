package storageapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/handlers"

	"dylaris-core/models"
)

// The rule these decide: which credential an UNSAVED connection test is allowed
// to run with.
//
// Testing before saving is the order everyone tries, and until now the only
// probe took an {id}, so a connection had to be committed before anyone could
// find out whether it worked. The bodied version is one careful step: it takes
// an operator-supplied endpoint, and `settings.write` is a delegatable panel
// capability whose holder can never READ a stored secret.
//
// So a bodied test that borrowed the saved secret for a submitted endpoint would
// be a credential-rebinding oracle - point it at a host you control and receive
// validly signed requests carrying the operator's identity. SigV4 signs with the
// secret rather than sending it, so the secret itself does not leak; what leaks
// is the ability to use it.

func draftBody(t *testing.T, id int, cfg handlers.StorageConnectionConfig, accessKey, secret string) *bytes.Reader {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"id":              id,
		"provider":        "s3",
		"config":          json.RawMessage(raw),
		"accessKey":       accessKey,
		"secretAccessKey": secret,
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return bytes.NewReader(body)
}

func storedConnection(cfg handlers.StorageConnectionConfig, accessKey string) *models.StorageConnection {
	raw, _ := json.Marshal(cfg)
	return &models.StorageConnection{
		ID:              7,
		Provider:        "s3",
		Config:          raw,
		AccessKey:       accessKey,
		SecretAccessKey: "the-stored-secret",
		SecretSet:       true,
	}
}

// The whole reason the guard exists. Same connection id, a different endpoint,
// no secret supplied: the stored credential must not travel there.
func TestDraftTest_RefusesToPointTheStoredSecretElsewhere(t *testing.T) {
	stored := handlers.StorageConnectionConfig{Endpoint: "https://fsn1.example.com", Bucket: "backups"}
	fs := &connFakeStore{existing: storedConnection(stored, "AKIA")}
	h := NewStorageConnectionsHandler(&handlers.AppState{Store: fs})

	attacker := handlers.StorageConnectionConfig{Endpoint: "https://collector.attacker.example", Bucket: "backups"}
	rw := httptest.NewRecorder()
	h.TestDraftConnection(rw, httptest.NewRequest(http.MethodPost, "/api/storage-connections/test",
		draftBody(t, 7, attacker, "AKIA", "")))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "re-enter the secret") {
		t.Errorf("the refusal did not say what to do about it: %s", rw.Body.String())
	}
}

// A changed access key is the same rebind by another route: it would pair a new
// key with the old secret.
func TestDraftTest_RefusesAChangedAccessKeyWithoutASecret(t *testing.T) {
	cfg := handlers.StorageConnectionConfig{Endpoint: "https://fsn1.example.com", Bucket: "backups"}
	fs := &connFakeStore{existing: storedConnection(cfg, "AKIA-OLD")}
	h := NewStorageConnectionsHandler(&handlers.AppState{Store: fs})

	rw := httptest.NewRecorder()
	h.TestDraftConnection(rw, httptest.NewRequest(http.MethodPost, "/api/storage-connections/test",
		draftBody(t, 7, cfg, "AKIA-NEW", "")))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rw.Code, rw.Body.String())
	}
}

// A brand new connection has nothing to borrow, and an empty secret would
// otherwise reach the probe and fail with a signature error that says nothing
// about the actual mistake.
func TestDraftTest_NewConnectionMustCarryItsOwnSecret(t *testing.T) {
	fs := &connFakeStore{}
	h := NewStorageConnectionsHandler(&handlers.AppState{Store: fs})

	cfg := handlers.StorageConnectionConfig{Endpoint: "https://fsn1.example.com", Bucket: "b"}
	rw := httptest.NewRecorder()
	h.TestDraftConnection(rw, httptest.NewRequest(http.MethodPost, "/api/storage-connections/test",
		draftBody(t, 0, cfg, "AKIA", "")))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "secret access key") {
		t.Errorf("the refusal did not name the missing field: %s", rw.Body.String())
	}
}

// The endpoint reaches a dialer, so it gets the same fail-closed check a save
// gets. A credential-bearing URL is the case that check exists for.
func TestDraftTest_RejectsACredentialBearingEndpoint(t *testing.T) {
	fs := &connFakeStore{}
	h := NewStorageConnectionsHandler(&handlers.AppState{Store: fs})

	cfg := handlers.StorageConnectionConfig{Endpoint: "https://user:pass@fsn1.example.com", Bucket: "b"}
	rw := httptest.NewRecorder()
	h.TestDraftConnection(rw, httptest.NewRequest(http.MethodPost, "/api/storage-connections/test",
		draftBody(t, 0, cfg, "AKIA", "sk")))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rw.Code, rw.Body.String())
	}
}

func TestDraftTest_RejectsAnUnsupportedProvider(t *testing.T) {
	fs := &connFakeStore{}
	h := NewStorageConnectionsHandler(&handlers.AppState{Store: fs})

	body, _ := json.Marshal(map[string]any{"provider": "ftp"})
	rw := httptest.NewRecorder()
	h.TestDraftConnection(rw, httptest.NewRequest(http.MethodPost, "/api/storage-connections/test", bytes.NewReader(body)))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rw.Code, rw.Body.String())
	}
}

// Unchanged endpoint, bucket and access key means the stored secret is being
// used exactly where it already is, which is the case the whole feature is for:
// re-testing a saved connection from inside the edit dialog without retyping a
// credential the form never shows.
//
// Provider construction then fails on the empty bucket - which is the point: it
// got past the guard to reach it. A reachable endpoint is not needed to prove
// that, and dialling one would put a network timeout in a unit test.
func TestDraftTest_AllowsTheStoredSecretWhereItAlreadyPoints(t *testing.T) {
	cfg := handlers.StorageConnectionConfig{Endpoint: "https://fsn1.example.com"}
	fs := &connFakeStore{existing: storedConnection(cfg, "AKIA")}
	h := NewStorageConnectionsHandler(&handlers.AppState{Store: fs})

	rw := httptest.NewRecorder()
	h.TestDraftConnection(rw, httptest.NewRequest(http.MethodPost, "/api/storage-connections/test",
		draftBody(t, 7, cfg, "AKIA", "")))

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with a verdict: %s", rw.Code, rw.Body.String())
	}
	var out struct {
		Success bool   `json:"success"`
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Success {
		t.Errorf("success = false; the request worked even though the probe did not")
	}
	if out.OK {
		t.Errorf("the probe reported OK against an unreachable endpoint")
	}
	if out.Message == "" {
		t.Errorf("a failed probe said nothing about why")
	}
}
