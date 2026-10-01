package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

// auditingKeyStore records what the server audit trail was told.
type auditingKeyStore struct {
	*apiKeysAuthFakeStore
	audits []models.ServerAuditEvent
}

func (f *auditingKeyStore) GetServerAuditState(int) (bool, bool, int, error) {
	return true, false, 0, nil
}
func (f *auditingKeyStore) InsertServerAudit(e *models.ServerAuditEvent) error {
	f.audits = append(f.audits, *e)
	return nil
}

func reachedInner(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

// Any positive rate used to be taken, so a key could switch its own per-key
// limit off.
func TestAPIKeyRateHasACeiling(t *testing.T) {
	fs := &apiKeysAuthFakeStore{}
	h := newAPIKeysAuthHandler(fs)
	rec := httptest.NewRecorder()
	h.Create(rec, createAPIKeyReq("u1", "alice", false, map[string]interface{}{
		"name": "k", "permissions": []string{"usage.read"}, "ratePerMin": 1000000,
	}))
	if rec.Code != http.StatusBadRequest || len(fs.createCalls) != 0 {
		t.Fatalf("status %d, created %d; want 400 and no key", rec.Code, len(fs.createCalls))
	}
}

// The operator's list of what a user key may carry was checked at mint only;
// a capability taken off it kept working on every key already minted.
func TestAKeyCapabilityRemovedFromTheListStopsWorking(t *testing.T) {
	owner := &models.User{ID: "owner-1", Username: "owner"}
	fs := externalFixture([]string{"console.send"}, []string{"uuid-a"}, owner, map[string]*models.Server{
		"uuid-a": {ID: 10, UUID: "uuid-a", OwnerID: "owner-1"},
	})
	fs.settings = map[string]string{"apikeys_user_allowed_caps": "console.read"}
	h := newAPIKeysAuthHandler(fs)
	rec := httptest.NewRecorder()
	h.APIKeyServerRoute("console.send")(reachedInner)(rec, externalRequest("POST", "/api/external/servers/uuid-a/console/command",
		map[string]string{"uuid": "uuid-a"}, `{"command":"list"}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403 for a capability the operator no longer allows", rec.Code)
	}
}

// A console command, an RCON call or a backup trigger by key left no record on
// the server's audit trail, which the same action in the panel does.
func TestAKeyActionIsOnTheServerAuditTrail(t *testing.T) {
	owner := &models.User{ID: "owner-1", Username: "owner"}
	base := externalFixture([]string{"console.send"}, []string{"uuid-a"}, owner, map[string]*models.Server{
		"uuid-a": {ID: 10, UUID: "uuid-a", OwnerID: "owner-1"},
	})
	fs := &auditingKeyStore{apiKeysAuthFakeStore: base}
	h := newAPIKeysAuthHandler(base)
	h.state.Store = fs
	rec := httptest.NewRecorder()
	h.APIKeyServerRoute("console.send")(reachedInner)(rec, externalRequest("POST", "/api/external/servers/uuid-a/console/command",
		map[string]string{"uuid": "uuid-a"}, `{"command":"list"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if len(fs.audits) != 1 || fs.audits[0].ServerID != 10 || fs.audits[0].EventType != "console.send" {
		t.Fatalf("audit rows %+v, want one console.send on server 10", fs.audits)
	}
}

// The demo account is read-only, and AuthMiddleware - where that is enforced -
// never sees a key request.
func TestADemoAccountKeyCannotWrite(t *testing.T) {
	owner := &models.User{ID: "owner-1", Username: "owner"}
	fs := externalFixture([]string{"console.send"}, []string{"uuid-a"}, owner, map[string]*models.Server{
		"uuid-a": {ID: 10, UUID: "uuid-a", OwnerID: "owner-1"},
	})
	fs.settings = map[string]string{demoAccountUUIDSetting: "owner-1"}
	h := newAPIKeysAuthHandler(fs)
	h.state.StoreEnabled = true
	rec := httptest.NewRecorder()
	h.APIKeyServerRoute("console.send")(reachedInner)(rec, externalRequest("POST", "/api/external/servers/uuid-a/console/command",
		map[string]string{"uuid": "uuid-a"}, `{"command":"stop"}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403 for the demo account's key", rec.Code)
	}
}

// The panel asks an operator to confirm a bypass of a tenant guard; a key has
// nobody to ask, so an operator's key does not carry the bypass.
func TestAnOperatorKeyDoesNotOverrideTenantGuards(t *testing.T) {
	r := httptest.NewRequest("POST", "/x", nil)
	r = r.WithContext(context.WithValue(r.Context(), "isAdmin", true))
	if !operatorOverride(r) {
		t.Fatal("an operator in a panel session lost the override")
	}
	r = r.WithContext(context.WithValue(r.Context(), apiKeyCtxKey{}, &apiKeyCtx{key: &models.APIKey{ID: 1}}))
	if operatorOverride(r) {
		t.Fatal("an operator's API key carried the override")
	}
}

type resetRevokeStore struct {
	store.Store
	revoked []int
}

func (f *resetRevokeStore) GetSetting(string) (string, error) { return "", nil }
func (f *resetRevokeStore) GetUserByPasswordResetToken(string) (*models.User, error) {
	return &models.User{ID: "u1", Username: "alice"}, nil
}
func (f *resetRevokeStore) ResetPasswordWithToken(string, string, string) (bool, error) {
	return true, nil
}
func (f *resetRevokeStore) ListAPIKeysByUser(string) ([]models.APIKey, error) {
	return []models.APIKey{{ID: 3}, {ID: 4, RevokedAt: func() *time.Time { t := time.Now(); return &t }()}}, nil
}
func (f *resetRevokeStore) RevokeAPIKey(id int, _ string) error {
	f.revoked = append(f.revoked, id)
	return nil
}
func (f *resetRevokeStore) InsertAuditIdentity(*models.AuditEventIdentity) error { return nil }

// Recovering an account by mail ended its sessions and left the API keys
// working - a key minted by whoever had the account was the way back in.
func TestAPasswordResetByLinkRevokesTheAccountsKeys(t *testing.T) {
	fs := &resetRevokeStore{}
	h := NewPasswordResetHandler(&AppState{Store: fs})
	rec := httptest.NewRecorder()
	h.ResetPassword(rec, httptest.NewRequest("POST", "/api/auth/reset-password",
		strings.NewReader(`{"token":"abcdefghijklmnopqrstuvwxyz","password":"a-long-new-password"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: status %d: %s", rec.Code, rec.Body.String())
	}
	if len(fs.revoked) != 1 || fs.revoked[0] != 3 {
		t.Fatalf("revoked %v, want the one live key", fs.revoked)
	}
}
