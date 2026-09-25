package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

type enrollRevokeStore struct {
	store.Store
	removed bool
	gotID   string
	gotUser string
}

func (f *enrollRevokeStore) DeleteNodeEnrollToken(id, userID string) (bool, error) {
	f.gotID, f.gotUser = id, userID
	return f.removed, nil
}
func (f *enrollRevokeStore) GetSetting(key string) (string, error) {
	// byonActive reads the feature flags; "true" keeps the handler reachable.
	return "true", nil
}
func (f *enrollRevokeStore) GetUserByID(id string) (*models.User, error) {
	return &models.User{ID: id, Username: "someone"}, nil
}

// Revoking an enroll token answered "success" to anyone, for any id.
//
// The delete has always been scoped by user id, so a stranger's id never
// matched and nothing was ever removed across accounts - but the answer said
// otherwise. Measured on production: an unrelated account revoked an admin's
// token, got 200 success, and the token was still there afterwards. In a
// credential-revocation path that is the wrong way to be wrong: the one person
// who must not be told "it is gone" is the one whose token is still live.
func TestRevokingAnEnrollTokenSaysWhatHappened(t *testing.T) {
	call := func(removed bool) *httptest.ResponseRecorder {
		st := &enrollRevokeStore{removed: removed}
		// FeatureFlags reads the same fake store, whose GetSetting answers
		// "true", so the BYON gate in front of the handler is open.
		h := &NodeEnrollHandler{state: &AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st)}}
		r := httptest.NewRequest(http.MethodDelete, "/api/nodes/enroll-token/abc", nil)
		ctx := context.WithValue(r.Context(), "userID", "u1")
		ctx = context.WithValue(ctx, "isAdmin", true)
		r = mux.SetURLVars(r.WithContext(ctx), map[string]string{"id": "abc"})
		rec := httptest.NewRecorder()
		h.RevokeToken(rec, r)
		return rec
	}

	if rec := call(true); rec.Code != http.StatusOK {
		t.Fatalf("a token that WAS removed answered %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// "No such token" and "not yours" are deliberately the same answer: telling
	// a stranger which ids exist is a different kind of mistake.
	if rec := call(false); rec.Code != http.StatusNotFound {
		t.Fatalf("nothing removed answered %d, want 404: %s", rec.Code, rec.Body.String())
	}
}
