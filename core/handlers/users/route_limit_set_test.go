package users

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/store"
)

type routeLimitFailStore struct {
	store.Store
}

func (routeLimitFailStore) SetGatewayRouteLimit(string, *int) error { return errors.New("db down") }
func (routeLimitFailStore) DeleteGatewayRouteLimit(string) error    { return errors.New("db down") }

// A failed write must not answer "Route limit updated": the operator would
// believe a user is stopped while the old allowance still applies.
func TestSetUserRouteLimitReportsAFailedWrite(t *testing.T) {
	for _, mode := range []string{"default", "unlimited", "custom", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			h := &UserHandler{d: Deps{Store: routeLimitFailStore{}}}
			rw := httptest.NewRecorder()
			h.SetUserRouteLimit(rw, handlertest.AdminRequest("PUT", map[string]string{"id": "u1"},
				`{"mode":"`+mode+`","maxRoutes":3}`))
			if rw.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500: %s", rw.Code, rw.Body.String())
			}
		})
	}
}
