package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// solderVerifyStore answers the single lookup VerifyKey makes. The embedded nil
// store.Store panics on anything else, which keeps this about the response
// shape.
type solderVerifyStore struct {
	store.Store
	key *store.SolderKey
}

func (f *solderVerifyStore) GetSolderKeyByHash(string) (*store.SolderKey, error) {
	return f.key, nil
}

// Modpacks on, which is what every one of these endpoints is gated by.
func (f *solderVerifyStore) GetSetting(key string) (string, error) {
	if key == "feature_modpacks_enabled" {
		return "true", nil
	}
	return "", nil
}

// The Technic Platform calls /solder/api/verify/{key} to prove an operator owns
// the Solder they entered. Upstream TechnicSolder has answered with created_at
// alongside valid and name for years; this omitted it. Whether the platform
// reads the field cannot be checked from here, which is the reason to send it:
// a field nobody reads costs nothing, one a client does read rejects a valid key.
func TestSolderVerifyKeyResponseShape(t *testing.T) {
	created := time.Date(2026, 9, 7, 14, 5, 6, 0, time.UTC)
	st := &solderVerifyStore{key: &store.SolderKey{Name: "technic", CreatedAt: created}}
	h := &SolderHandler{state: &AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st)}}

	rec := httptest.NewRecorder()
	req := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/solder/api/verify/secret", nil), map[string]string{"key": "secret"})
	h.VerifyKey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for k, want := range map[string]string{
		"valid": "Key validated.",
		"name":  "technic",
		// Local time must not leak in: the reference implementation stored and
		// emitted UTC, and a launcher comparing this to anything would compare
		// it to that.
		"created_at": "2026-09-07 14:05:06",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}

// An unknown key is a 403 with the contract's own wording, and must not leak
// that the lookup even happened.
func TestSolderVerifyKeyRejectsUnknown(t *testing.T) {
	st := &solderVerifyStore{key: nil}
	h := &SolderHandler{state: &AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st)}}
	rec := httptest.NewRecorder()
	req := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/solder/api/verify/nope", nil), map[string]string{"key": "nope"})
	h.VerifyKey(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["error"] != "Invalid key provided." {
		t.Errorf("error = %q, want the contract wording", got["error"])
	}
}
