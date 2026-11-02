package files

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/handlers/handlertest"
	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"
	"dylaris-core/store"
)

type suspendedUploadStore struct{ quotaHTTPFakeStore }

func (suspendedUploadStore) GetUserBilling(string) (*store.UserBilling, error) {
	return &store.UserBilling{Status: "suspended"}, nil
}

// Beam refused a suspended tenant a ticket; the panel's upload took their file.
// An operator's key carries no override (round 26), so it is refused too.
func TestAnUploadToASuspendedTenantsServerIsRefused(t *testing.T) {
	rdb, _ := handlertest.Redis(t)
	st := suspendedUploadStore{quotaHTTPFakeStore{handlertest.NewSettingsStore()}}
	h := &FileHandler{d: Deps{Redis: rdb, Store: st, Authz: authz.NewResolver(st)}}
	req := newUploadRequest(t, "s1", 10)
	req = req.WithContext(reqctx.WithAPIKey(req.Context(), &models.APIKey{ID: 1}, false))
	rw := httptest.NewRecorder()
	h.UploadFileHandler(rw, req)
	if rw.Code != http.StatusForbidden || !strings.Contains(rw.Body.String(), "suspended") {
		t.Fatalf("status %d (%s), want the suspension refusal", rw.Code, rw.Body.String())
	}
}
