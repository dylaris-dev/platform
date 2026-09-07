package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// solderKeyCreateStore records what reached the store. Hashes only: the
// plaintext must never be persisted, and asserting on the hash is what proves
// a pasted key is treated exactly like a minted one from there on.
type solderKeyCreateStore struct {
	store.Store
	hashes   map[string]bool
	created  []string // key_hash, in order
	failWith error
	callerID string
}

func (f *solderKeyCreateStore) GetSetting(key string) (string, error) {
	if key == "feature_modpacks_enabled" {
		return "true", nil
	}
	return "", nil
}

func (f *solderKeyCreateStore) CreateSolderKey(name, ownerID, keyHash string) (*store.SolderKey, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}
	if f.hashes[keyHash] {
		return nil, store.ErrNameTaken
	}
	if f.hashes == nil {
		f.hashes = map[string]bool{}
	}
	f.hashes[keyHash] = true
	f.created = append(f.created, keyHash)
	f.callerID = ownerID
	return &store.SolderKey{ID: len(f.created), Name: name, OwnerID: ownerID}, nil
}

func (f *solderKeyCreateStore) GetSolderKeyByHash(h string) (*store.SolderKey, error) {
	if f.hashes[h] {
		return &store.SolderKey{ID: 1, Name: "technic", OwnerID: solderTestOwner}, nil
	}
	return nil, nil
}

func (f *solderKeyCreateStore) GetUserIDBySolderHandle(handle string) (string, error) {
	if handle == solderTestHandle {
		return solderTestOwner, nil
	}
	return "", nil
}

func newSolderKeyHandler(st *solderKeyCreateStore) *SolderHandler {
	return &SolderHandler{state: &AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st)}}
}

func postKey(t *testing.T, h *SolderHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/solder/keys", bytes.NewReader([]byte(body)))
	h.CreateKey(rec, req)
	return rec
}

// The Technic Platform ISSUES the key and expects Solder to accept it; it then
// calls GET /solder/api/verify/{key} with that value. Core could only ever mint
// its own, which Technic has never heard of, so linking was impossible and
// failed with a bare 403 from a URL that was otherwise correct.
//
// MEASURED before this: technicpack.net asked this install to verify a 32-hex
// key that nothing here could have created.
func TestCreateSolderKeyAcceptsAPastedPlatformKey(t *testing.T) {
	st := &solderKeyCreateStore{}
	h := newSolderKeyHandler(st)

	const platformKey = "0290a8199086df46be66a94fb7b87453" // the shape Technic sent
	rec := postKey(t, h, `{"name":"technic","key":"`+platformKey+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// Stored under the hash of the key Technic will present, which is the whole
	// point: verify hashes the incoming value and looks it up.
	if len(st.created) != 1 || st.created[0] != solderKeyHash(platformKey) {
		t.Fatalf("stored %v, want the hash of the pasted key", st.created)
	}

	// And the verify endpoint now answers for it, which is the actual handshake.
	vrec := httptest.NewRecorder()
	h.VerifyKey(vrec, keyRequest(platformKey))
	if vrec.Code != http.StatusOK {
		t.Errorf("verify status = %d, want 200 - the pasted key must be the one Technic can verify", vrec.Code)
	}

	// A pasted key is not echoed back. The operator already has it, and the
	// response is one more place it would exist for no gain.
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if _, ok := out["plaintext"]; ok {
		t.Error("the response echoed a pasted key back")
	}
}

// The generated path is the launcher key and must keep working unchanged: 64
// hex characters, shown exactly once because this is the only moment it exists
// in the clear.
func TestCreateSolderKeyStillMintsWhenNoneIsGiven(t *testing.T) {
	st := &solderKeyCreateStore{}
	rec := postKey(t, newSolderKeyHandler(st), `{"name":"launcher"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Plaintext string `json:"plaintext"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Plaintext) != 64 {
		t.Errorf("plaintext is %d characters, want 64", len(out.Plaintext))
	}
	if len(st.created) != 1 || st.created[0] != solderKeyHash(out.Plaintext) {
		t.Error("the stored hash is not the hash of the key that was returned")
	}
}

func TestCreateSolderKeyRefusals(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		want       int
	}{
		// Whitespace is the give-away for a paste that picked up a newline, and
		// it would be hashed into a key Technic can never match.
		{name: "a key with a space", body: `{"name":"x","key":"abc def abc def abc"}`, want: http.StatusBadRequest},
		{name: "too short to be a key", body: `{"name":"x","key":"short"}`, want: http.StatusBadRequest},
		{name: "over the column width", body: `{"name":"x","key":"` + repeat("a", 129) + `"}`, want: http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := postKey(t, newSolderKeyHandler(&solderKeyCreateStore{}), tt.body)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}

// key_hash is UNIQUE, and now that a person can choose the value, a collision is
// something they can cause by hand - re-adding the same Technic key. It has to
// answer rather than 500, and without the driver's message.
func TestCreateSolderKeyRejectsADuplicate(t *testing.T) {
	st := &solderKeyCreateStore{}
	h := newSolderKeyHandler(st)
	const k = "0290a8199086df46be66a94fb7b87453"

	if rec := postKey(t, h, `{"name":"a","key":"`+k+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("first add: %d %s", rec.Code, rec.Body.String())
	}
	rec := postKey(t, h, `{"name":"b","key":"`+k+`"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second add: status = %d, want 409", rec.Code)
	}
	var out map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["message"] == "" {
		t.Error("no message; the operator cannot tell this from a server fault")
	}
	if len(st.created) != 1 {
		t.Errorf("stored %d keys, want 1", len(st.created))
	}
}

const (
	solderTestOwner  = "aaaaaaaa-1111-4111-8111-111111111111"
	solderTestHandle = "bartis"
)

// keyRequest builds the verify request the Technic Platform makes, against the
// account's own Solder address.
func keyRequest(key string) *http.Request {
	return mux.SetURLVars(
		httptest.NewRequest(http.MethodGet, "/solder/u/"+solderTestHandle+"/api/verify/"+key, nil),
		map[string]string{"handle": solderTestHandle, "key": key})
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n*len(s))
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
