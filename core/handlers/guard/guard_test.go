package guard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// The guards took *AppState before they moved here and now take the one
// dependency each reads. These pin what a caller sees, so a slip in that
// rewrite (a swapped status, a lost nil check, an override that now also
// opens the node interlock) fails by name.
func TestTheGuardsAnswerAsBeforeTheMove(t *testing.T) {
	rdb, _ := handlertest.Redis(t)
	suspended := &cutoffBillingStore{billing: &store.UserBilling{Status: "suspended"}}
	req := func(admin bool, key *models.APIKey) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		ctx := context.WithValue(r.Context(), "isAdmin", admin)
		if key != nil {
			ctx = reqctx.WithAPIKey(ctx, key, true)
		}
		return r.WithContext(ctx)
	}
	message := func(rec *httptest.ResponseRecorder) string {
		var out struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Message
	}

	srv := &models.Server{UUID: "srv-a", OwnerID: "u1", Status: "stopped"}
	moving := &models.Server{UUID: "srv-m", OwnerID: "u1", Status: "migrating"}

	for _, tc := range []struct {
		name string
		run  func(w http.ResponseWriter) bool
		code int // 0: not refused
	}{
		{"suspended owner", func(w http.ResponseWriter) bool {
			return RefuseIfSuspended(w, req(false, nil), suspended, time.Hour, srv)
		}, http.StatusForbidden},
		{"suspended owner, admin in the panel", func(w http.ResponseWriter) bool {
			return RefuseIfSuspended(w, req(true, nil), suspended, time.Hour, srv)
		}, 0},
		{"suspended owner, admin's API key", func(w http.ResponseWriter) bool {
			return RefuseIfSuspended(w, req(true, &models.APIKey{ID: 1}), suspended, time.Hour, srv)
		}, http.StatusForbidden},
		{"no store", func(w http.ResponseWriter) bool { return RefuseIfSuspended(w, req(false, nil), nil, time.Hour, srv) }, 0},
		{"no server", func(w http.ResponseWriter) bool {
			return RefuseIfSuspended(w, req(false, nil), suspended, time.Hour, nil)
		}, 0},
		{"moving", func(w http.ResponseWriter) bool { return RefuseIfServerBusy(w, req(false, nil), rdb, moving) }, http.StatusConflict},
		{"moving, admin in the panel", func(w http.ResponseWriter) bool { return RefuseIfServerBusy(w, req(true, nil), rdb, moving) }, 0},
		{"moving, no redis", func(w http.ResponseWriter) bool { return RefuseIfServerBusy(w, req(false, nil), nil, moving) }, http.StatusConflict},
		{"idle", func(w http.ResponseWriter) bool { return RefuseIfServerBusy(w, req(false, nil), rdb, srv) }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			got := tc.run(rec)
			if got != (tc.code != 0) || (tc.code != 0 && rec.Code != tc.code) {
				t.Fatalf("refused=%v code=%d, want code %d", got, rec.Code, tc.code)
			}
			if tc.code == http.StatusForbidden && message(rec) != SuspendedMessage {
				t.Fatalf("message %q", message(rec))
			}
		})
	}

	// The node's interlock is not an operator override: the node is writing
	// the directory, and an admin's setup beside it would write it too.
	if err := rdb.Set(context.Background(), services.NodeBusyKey(srv.UUID), "installing", 0).Err(); err != nil {
		t.Fatal(err)
	}
	for _, admin := range []bool{false, true} {
		rec := httptest.NewRecorder()
		if !RefuseIfServerBusy(rec, req(admin, nil), rdb, srv) || rec.Code != http.StatusConflict {
			t.Fatalf("admin=%v: node_busy did not refuse (code %d)", admin, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	if RefuseIfServerBusy(rec, req(false, nil), nil, srv) {
		t.Fatal("without Redis the interlock cannot be read and must not refuse")
	}
}
