package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// nodeAdmissionFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only the methods SetAdmission and AddCIDR touch
// are overridden. Any other call would panic - these tests never make one.
type nodeAdmissionFakeStore struct {
	store.Store

	setSettingCalls []setSettingCall
	setSettingErr   error

	addCIDRCalls []addCIDRCall
	addCIDRErr   error
}

type setSettingCall struct {
	key   string
	value string
}

type addCIDRCall struct {
	cidr  string
	label string
}

func (f *nodeAdmissionFakeStore) SetSetting(key, value string) error {
	f.setSettingCalls = append(f.setSettingCalls, setSettingCall{key, value})
	return f.setSettingErr
}

func (f *nodeAdmissionFakeStore) AddAdmissionCIDR(cidr, label string) error {
	f.addCIDRCalls = append(f.addCIDRCalls, addCIDRCall{cidr, label})
	return f.addCIDRErr
}

func (f *nodeAdmissionFakeStore) InsertAuditIdentity(*models.AuditEventIdentity) error { return nil }

func nodeAdmissionReq(method, path string, isAdmin bool, body map[string]interface{}) *http.Request {
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	return r.WithContext(context.WithValue(r.Context(), "isAdmin", isAdmin))
}

// --- SetAdmission ---

// Phase 4 Task 13: nodes.write is now enforced at the route chokepoint
// (RequireCap wrapping in routes.go), not in-handler, so the old pure
// non-admin-forbidden case moved to routes_authz_test.go
// (TestCap_NodeAdmissionPanel), which runs through the real resolver.

// TestSetAdmission_ValidationAndPersistence pins the validJoin x validIP
// enum guard (node_admission.go:77-78) and the exact two SetSetting calls a
// valid combination persists.
func TestSetAdmission_ValidationAndPersistence(t *testing.T) {
	cases := []struct {
		name       string
		joinMode   string
		ipMode     string
		wantStatus int
		wantCalled bool
	}{
		{"disabled + allow is valid", "disabled", "allow", http.StatusOK, true},
		{"open + allow is valid", "open", "allow", http.StatusOK, true},
		{"one-shot + deny is valid", "one-shot", "deny", http.StatusOK, true},
		{"invalid joinMode rejected", "bogus", "allow", http.StatusBadRequest, false},
		{"invalid ipMode rejected", "open", "bogus", http.StatusBadRequest, false},
		{"empty joinMode rejected", "", "allow", http.StatusBadRequest, false},
		{"empty ipMode rejected", "open", "", http.StatusBadRequest, false},
		// case-sensitivity: the valid-set map keys are lowercase-exact, so an
		// otherwise-valid value in the wrong case is rejected, not normalized.
		{"joinMode is case-sensitive - uppercase rejected", "OPEN", "allow", http.StatusBadRequest, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &nodeAdmissionFakeStore{}
			h := NewNodeAdmissionHandler(&AppState{Store: fs})
			rec := httptest.NewRecorder()

			h.SetAdmission(rec, nodeAdmissionReq("PUT", "/api/admin/settings/node-admission", true, map[string]interface{}{
				"joinMode": c.joinMode, "ipMode": c.ipMode,
			}))

			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.wantStatus, rec.Body.String())
			}
			if !c.wantCalled {
				if len(fs.setSettingCalls) != 0 {
					t.Fatalf("expected no SetSetting calls, got %+v", fs.setSettingCalls)
				}
				return
			}
			if len(fs.setSettingCalls) != 2 {
				t.Fatalf("expected exactly 2 SetSetting calls, got %+v", fs.setSettingCalls)
			}
			want := []setSettingCall{
				{"node_join_mode", c.joinMode},
				{"node_admission_ip_mode", c.ipMode},
			}
			if fs.setSettingCalls[0] != want[0] || fs.setSettingCalls[1] != want[1] {
				t.Fatalf("setSettingCalls = %+v, want %+v", fs.setSettingCalls, want)
			}
		})
	}
}

func TestSetAdmission_InvalidJSON(t *testing.T) {
	fs := &nodeAdmissionFakeStore{}
	h := NewNodeAdmissionHandler(&AppState{Store: fs})
	r := httptest.NewRequest("PUT", "/api/admin/settings/node-admission", bytes.NewReader([]byte("not json")))
	r = r.WithContext(context.WithValue(r.Context(), "isAdmin", true))
	rec := httptest.NewRecorder()

	h.SetAdmission(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// --- AddCIDR ---

// Phase 4 Task 13: nodes.write is now enforced at the route chokepoint
// (RequireCap wrapping in routes.go), not in-handler, so the old pure
// non-admin-forbidden case moved to routes_authz_test.go
// (TestCap_NodeAdmissionCIDRsPanel), which runs through the real resolver.

func TestAddCIDR_MalformedRejected(t *testing.T) {
	fs := &nodeAdmissionFakeStore{}
	h := NewNodeAdmissionHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()

	h.AddCIDR(rec, nodeAdmissionReq("POST", "/api/admin/settings/node-admission/cidrs", true, map[string]interface{}{
		"cidr": "not-a-cidr", "label": "office",
	}))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if len(fs.addCIDRCalls) != 0 {
		t.Fatalf("expected no AddAdmissionCIDR calls, got %+v", fs.addCIDRCalls)
	}
}

// TestAddCIDR_NormalizesToNetworkAddress pins the exact normalization: the
// handler stores net.ParseCIDR's returned *net.IPNet.String() (the network
// address with host bits zeroed), NOT the raw input string. "10.0.0.5/24"
// must be persisted (and echoed back) as "10.0.0.0/24".
func TestAddCIDR_NormalizesToNetworkAddress(t *testing.T) {
	fs := &nodeAdmissionFakeStore{}
	h := NewNodeAdmissionHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()

	h.AddCIDR(rec, nodeAdmissionReq("POST", "/api/admin/settings/node-admission/cidrs", true, map[string]interface{}{
		"cidr": "10.0.0.5/24", "label": "  office  ",
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fs.addCIDRCalls) != 1 {
		t.Fatalf("expected 1 AddAdmissionCIDR call, got %+v", fs.addCIDRCalls)
	}
	call := fs.addCIDRCalls[0]
	if call.cidr != "10.0.0.0/24" {
		t.Fatalf("persisted cidr = %q, want normalized 10.0.0.0/24", call.cidr)
	}
	if call.label != "office" {
		t.Fatalf("persisted label = %q, want trimmed 'office'", call.label)
	}

	var resp struct {
		Success bool   `json:"success"`
		CIDR    string `json:"cidr"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success || resp.CIDR != "10.0.0.0/24" {
		t.Fatalf("response = %+v, want success=true cidr=10.0.0.0/24", resp)
	}
}

// --- RollSecret ---

// rollSecretFakeStore records every act RollSecret performs, in order, so a
// test can say both WHAT happened and that the secret went before the
// admission was armed.
type rollSecretFakeStore struct {
	store.Store

	node       *models.Node
	lastAuthIP string

	ops      []string
	armToken string
	armIP    string
	armBy    string
	audits   []string
}

func (f *rollSecretFakeStore) GetNodeByID(id int) (*models.Node, error) {
	if f.node == nil || f.node.ID != id {
		return nil, sql.ErrNoRows
	}
	return f.node, nil
}

func (f *rollSecretFakeStore) GetNodeLastAuthPeerIP(int) (string, error) { return f.lastAuthIP, nil }

func (f *rollSecretFakeStore) SetNodeSecretEnc(id int, enc string) error {
	f.ops = append(f.ops, "clear-secret:"+strconv.Itoa(id)+":"+enc)
	return nil
}

func (f *rollSecretFakeStore) ArmNodeJoinApproval(token, fromIP, by string) (bool, error) {
	f.ops = append(f.ops, "arm")
	f.armToken, f.armIP, f.armBy = token, fromIP, by
	return fromIP != "", nil
}

func (f *rollSecretFakeStore) InsertAuditIdentity(e *models.AuditEventIdentity) error {
	f.audits = append(f.audits, e.EventType)
	return nil
}

func rollSecretReq(id string) *http.Request {
	r := httptest.NewRequest("POST", "/api/admin/nodes/"+id+"/roll-secret", nil)
	r = mux.SetURLVars(r, map[string]string{"id": id})
	return r.WithContext(context.WithValue(r.Context(), "userID", "admin-1"))
}

// Without an address the admission would have to be unbound, and an admission
// for any source is the one thing this must never arm. Clearing the secret
// anyway would silently turn the action into Reset pairing, so NOTHING changes.
func TestRollSecret_NoRecordedAddressChangesNothing(t *testing.T) {
	fs := &rollSecretFakeStore{node: &models.Node{ID: 5, Token: "node-abc"}}
	h := NewNodeAdmissionHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()

	h.RollSecret(rec, rollSecretReq("5"))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if len(fs.ops) != 0 {
		t.Fatalf("the node was touched although nothing could be armed: %v", fs.ops)
	}
	if len(fs.audits) != 0 {
		t.Errorf("an audit event was written for a roll that did not happen: %v", fs.audits)
	}
	// The operator is told the way that DOES work.
	if !strings.Contains(rec.Body.String(), "Reset pairing") {
		t.Errorf("the 409 does not say what to do instead: %s", rec.Body.String())
	}
}

func TestRollSecret_ArmsTheAdmissionAtTheLastAuthAddress(t *testing.T) {
	fs := &rollSecretFakeStore{node: &models.Node{ID: 5, Token: "node-abc"}, lastAuthIP: "203.0.113.7"}
	h := NewNodeAdmissionHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()

	h.RollSecret(rec, rollSecretReq("5"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// The secret must be cleared BEFORE the admission is armed: a node whose
	// secret Core still holds is challenged and never consumes an admission.
	want := []string{"clear-secret:5:", "arm"}
	if strings.Join(fs.ops, ",") != strings.Join(want, ",") {
		t.Fatalf("ops = %v, want %v", fs.ops, want)
	}
	if fs.armToken != "node-abc" || fs.armIP != "203.0.113.7" || fs.armBy != "admin-1" {
		t.Errorf("armed (%q, %q, %q), want the node's token, its last auth address and the caller",
			fs.armToken, fs.armIP, fs.armBy)
	}
	if len(fs.audits) != 1 || fs.audits[0] != "node.secret_rolled" {
		t.Errorf("audits = %v, want exactly node.secret_rolled", fs.audits)
	}
}

func TestRollSecret_UnknownNode(t *testing.T) {
	fs := &rollSecretFakeStore{node: &models.Node{ID: 5, Token: "node-abc"}, lastAuthIP: "203.0.113.7"}
	h := NewNodeAdmissionHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()

	h.RollSecret(rec, rollSecretReq("6"))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if len(fs.ops) != 0 {
		t.Fatalf("an unknown id touched something: %v", fs.ops)
	}
}

func TestAddCIDR_InvalidJSON(t *testing.T) {
	fs := &nodeAdmissionFakeStore{}
	h := NewNodeAdmissionHandler(&AppState{Store: fs})
	r := httptest.NewRequest("POST", "/api/admin/settings/node-admission/cidrs", bytes.NewReader([]byte("not json")))
	r = r.WithContext(context.WithValue(r.Context(), "isAdmin", true))
	rec := httptest.NewRecorder()

	h.AddCIDR(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}
