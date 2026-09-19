package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"

	"github.com/gorilla/mux"
)

// pairingStore adds what the owner's reset and admit touch.
type pairingStore struct {
	selfNodeStore
	attempt    *models.NodeJoinAttempt
	resetLogin int
	rejected   int
	armedFor   string
}

func (f *pairingStore) ResetNodeLogin(id int) error { f.resetLogin = id; return nil }
func (f *pairingStore) RejectNodePublicKey(id int) (bool, error) {
	f.rejected = id
	return true, nil
}
func (f *pairingStore) GetNodeJoinAttempt(string) (*models.NodeJoinAttempt, error) {
	return f.attempt, nil
}
func (f *pairingStore) ApproveNodeJoinAttemptForKey(_, fp, _ string) (bool, error) {
	f.armedFor = fp
	return true, nil
}
func (f *pairingStore) InsertAuditIdentity(*models.AuditEventIdentity) error { return nil }

func pairingPost(target, userID, body string) *http.Request {
	r := httptest.NewRequest("POST", target, strings.NewReader(body))
	r = mux.SetURLVars(r, map[string]string{"id": "7"})
	//nolint:staticcheck // the handlers read a plain string key; matching them is the point
	return r.WithContext(context.WithValue(r.Context(), "userID", userID))
}

// The owner resets their own machine. A stranger cannot: before this, nobody
// but the fleet operator could, and they were refused on a customer's machine.
func TestResetMyNodePairingIsTheOwnersOnly(t *testing.T) {
	fs := &pairingStore{selfNodeStore: selfNodeStore{node: ownedBy("me")}}
	h := &NodeHandler{state: &AppState{Store: fs}}

	w := httptest.NewRecorder()
	h.ResetMyNodePairing(w, pairingPost("/api/me/nodes/7/reset-pairing", "stranger", ""))
	if w.Code != 404 || fs.resetLogin != 0 {
		t.Fatalf("a stranger got %d and reset=%d, want 404 and nothing reset", w.Code, fs.resetLogin)
	}

	w = httptest.NewRecorder()
	h.ResetMyNodePairing(w, pairingPost("/api/me/nodes/7/reset-pairing", "me", ""))
	if w.Code != 200 || fs.resetLogin != 7 {
		t.Fatalf("the owner got %d and reset=%d, want 200 and node 7 reset", w.Code, fs.resetLogin)
	}
}

// Admit takes the fingerprint the owner read off their own machine's log, as a
// person copies it. It does not depend on what is knocking right now: anyone who
// knows the machine's id could keep that row showing their own key, and the
// owner could then never admit their machine.
func TestAdmitMyNodeTakesTheFingerprintFromTheOwner(t *testing.T) {
	full := strings.Repeat("ab", 32)
	cases := []struct {
		name      string
		body      string
		wantCode  int
		wantArmed string
	}{
		{"nothing named", `{}`, 400, ""},
		{"too short to be a key", `{"fingerprint":"abcd-ef01"}`, 400, ""},
		{"not hex", `{"fingerprint":"zzzz-zzzz-zzzz-zzzz"}`, 400, ""},
		{"the logged prefix, dashes and case as printed", `{"fingerprint":" ABCD-EF01-2345-6789 "}`, 200, "abcdef0123456789"},
		{"the full fingerprint", `{"fingerprint":"` + full + `"}`, 200, full},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &pairingStore{selfNodeStore: selfNodeStore{node: ownedBy("me")}}
			h := &NodeHandler{state: &AppState{Store: fs}}
			w := httptest.NewRecorder()
			h.AdmitMyNode(w, pairingPost("/api/me/nodes/7/admit", "me", c.body))
			if w.Code != c.wantCode || fs.armedFor != c.wantArmed {
				t.Fatalf("code=%d armed=%q, want %d %q (%s)", w.Code, fs.armedFor, c.wantCode, c.wantArmed, w.Body.String())
			}
			if c.wantCode != 200 && fs.rejected != 0 {
				t.Error("the machine's login was revoked for an admission that never armed")
			}
		})
	}

	t.Run("a stranger", func(t *testing.T) {
		fs := &pairingStore{selfNodeStore: selfNodeStore{node: ownedBy("me")}}
		h := &NodeHandler{state: &AppState{Store: fs}}
		w := httptest.NewRecorder()
		h.AdmitMyNode(w, pairingPost("/api/me/nodes/7/admit", "stranger", `{"fingerprint":"abcd-ef01-2345-6789"}`))
		if w.Code != 404 || fs.armedFor != "" {
			t.Fatalf("a stranger got %d and armed %q", w.Code, fs.armedFor)
		}
	})
}
