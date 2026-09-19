package handlers

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/store"
)

type keyLookupStore struct {
	store.Store
	err error
}

func (f *keyLookupStore) GetWarpAPIKeyByHash(string) (*store.WarpAPIKey, error) { return nil, f.err }

// A key that does not exist is a 401; a database that could not answer is not.
// The link reads 401 as "revoked" and exits at boot, so a lookup failure
// reported as 401 stopped a customer's link over a blip on our side.
func TestWarpKeyMiddlewareTellsAnUnknownKeyFromAFault(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want int
	}{
		{"unknown key", sql.ErrNoRows, http.StatusUnauthorized},
		{"database fault", errors.New("connection refused"), http.StatusServiceUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := &WarpHandler{state: &AppState{Store: &keyLookupStore{err: c.err}}}
			req := httptest.NewRequest(http.MethodPost, "/api/warp/link/heartbeat", nil)
			req.Header.Set("Authorization", "Bearer link-abc")
			rec := httptest.NewRecorder()
			h.WarpAPIKeyMiddleware(func(http.ResponseWriter, *http.Request) {
				t.Fatal("the handler ran without a key")
			})(rec, req)
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d", rec.Code, c.want)
			}
		})
	}
}
