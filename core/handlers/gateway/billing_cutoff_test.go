package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// A cut-off owner kept claiming addresses on our domains, which nobody else
// could take while they held them. Refused before anything is queued.
func TestASuspendedOwnerCannotClaimAnAddress(t *testing.T) {
	for _, tc := range []struct {
		status     string
		wantStatus int
	}{{"suspended", http.StatusForbidden}, {"active", http.StatusCreated}} {
		t.Run(tc.status, func(t *testing.T) {
			rdb := newLinkRouteRedis(t)
			fs := &suspendedRouteStore{status: tc.status, linkRouteFakeStore: &linkRouteFakeStore{settings: map[string]string{
				services.HosterDomainsSettingKey: `[{"domain":"example.com","validation":"dns"}]`,
			}, servers: map[string][]models.Server{linkRouteUserID: {{ID: 1, UUID: "srv-1"}}}}}
			gw := &linkRouteFakeGateway{}
			h := &GatewayHandler{state: &handlers.AppState{Store: fs, Gateway: gw, Redis: rdb, FeatureFlags: services.NewFeatureFlags(routeOnlyOnFlags{})}}
			rec := httptest.NewRecorder()
			h.CreateServerRoute(rec, serverRouteReq(linkRouteUserID, false, map[string]interface{}{
				"domain": "new.example.com", "targetPort": 25565,
			}))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if created := len(gw.createRouteServerCalls) > 0; created != (tc.wantStatus == http.StatusCreated) {
				t.Fatalf("created = %v", created)
			}
		})
	}
}

type suspendedRouteStore struct {
	*linkRouteFakeStore
	status string
}

func (f *suspendedRouteStore) GetUserBilling(userID string) (*store.UserBilling, error) {
	return &store.UserBilling{UserID: userID, Status: f.status}, nil
}
