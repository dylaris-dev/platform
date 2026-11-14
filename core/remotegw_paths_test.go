package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/handlers"
	"dylaris-core/handlers/auth"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/remotegw"
)

// auditedKeyStore takes the audit row of a refused key request.
type auditedKeyStore struct{ keyedAuthzStore }

func (*auditedKeyStore) InsertAuditIdentity(*models.AuditEventIdentity) error { return nil }

// The typed client's newer calls go through the REAL router: a path that drifts
// from its registration answers the router's code-less 404, which Players
// reads as "an older provider" and shows as zero players with every other test
// green. The key holds no permission, so a registered route meets its scope
// check and answers 403; only an unregistered path answers 404.
func TestRemoteClientPathsReachTheRealRouter(t *testing.T) {
	fs := &authzFakeStore{settings: map[string]string{"routing_mode": "gateway"}}
	owner := fs.addUser("owner-id", "owner", false)
	st := &auditedKeyStore{keyedAuthzStore{authzFakeStore: fs, key: &models.APIKey{ID: 1, UserID: owner.ID, RatePerMin: 100}}}
	appState := &handlers.AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st), Authz: authz.NewResolver(st),
		GatewayRemote: func() (bool, bool) { return false, true }}
	h, _ := buildAPIRouter(appState, auth.NewAuthHandler(authDepsOf(appState), testJWTSecret), routeCfg{JWTSecret: testJWTSecret})
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		mu.Lock()
		seen[r.Method+" "+r.URL.Path] = rec.Code
		mu.Unlock()
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	t.Cleanup(srv.Close)
	c := remotegw.New(srv.URL, func() string { return "dyl_not_a_real_key" })
	ctx := context.Background()
	const d = "play.example.com"
	_, _ = c.SetVoice(ctx, d, true, 0)
	_, _ = c.SetBedrock(ctx, d, true, true)
	_, _ = c.FloodgateKey(ctx, d)
	_, _ = c.Players(ctx)
	for _, want := range []string{
		"PUT /api/external/gateway/routes/" + d + "/voice",
		"PUT /api/external/gateway/routes/" + d + "/bedrock",
		"GET /api/external/gateway/routes/" + d + "/floodgate-key",
		"GET /api/external/gateway/players",
	} {
		code, ok := seen[want]
		switch {
		case !ok:
			t.Errorf("%s: the client never sent it; sent %v", want, seen)
		case code == http.StatusNotFound:
			t.Errorf("%s answered 404: the client's path matches no registered route", want)
		case code != http.StatusForbidden:
			t.Errorf("%s answered %d, want the scope check's 403", want, code)
		}
	}
}
