package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

type suspendAuditStore struct {
	*linkRouteFakeStore
	events []string
}

func (s *suspendAuditStore) InsertAuditIdentity(ev *models.AuditEventIdentity) error {
	s.events = append(s.events, ev.EventType)
	return nil
}

func suspendReq(path, body string) *http.Request {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r = mux.SetURLVars(r, map[string]string{"domain": "play.example.com"})
	return r.WithContext(context.WithValue(r.Context(), "userID", "admin-1"))
}

func TestSuspendRouteHandler(t *testing.T) {
	st := &suspendAuditStore{linkRouteFakeStore: baseLinkRouteStore()}
	gw := &linkRouteFakeGateway{}
	h := &GatewayHandler{d: Deps{Store: st, Gateway: gw}}

	for _, body := range []string{`{}`, `{"reason":"  "}`, `{"reason":"` + strings.Repeat("x", 501) + `"}`, `nope`} {
		rec := httptest.NewRecorder()
		h.SuspendRoute(rec, suspendReq("/x", body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %.20q = %d, want 400", body, rec.Code)
		}
	}
	if len(gw.suspended) != 0 {
		t.Fatalf("a rejected request still suspended: %v", gw.suspended)
	}

	rec := httptest.NewRecorder()
	h.SuspendRoute(rec, suspendReq("/x", `{"reason":" abuse "}`))
	if rec.Code != 200 || len(gw.suspended) != 1 || gw.suspended[0] != "play.example.com|abuse" {
		t.Fatalf("suspend = %d %s calls=%v", rec.Code, rec.Body.String(), gw.suspended)
	}
	rec = httptest.NewRecorder()
	h.UnsuspendRoute(rec, suspendReq("/x", ``))
	if rec.Code != 200 || gw.suspended[1] != "lift|play.example.com" {
		t.Fatalf("unsuspend = %d %s", rec.Code, rec.Body.String())
	}
	if len(st.events) != 2 || st.events[0] != "gateway_route_suspended" || st.events[1] != "gateway_route_unsuspended" {
		t.Fatalf("audit = %v", st.events)
	}

	for err, want := range map[error]int{
		services.ErrRouteNotStored:    http.StatusNotFound,
		services.ErrRouteNotCoreOwned: http.StatusConflict,
	} {
		gw.suspendErr = err
		rec = httptest.NewRecorder()
		h.SuspendRoute(rec, suspendReq("/x", `{"reason":"r"}`))
		if rec.Code != want {
			t.Errorf("%v = %d, want %d", err, rec.Code, want)
		}
	}
}

func TestSuspendedRouteIsListedForItsOwnerAndAdmin(t *testing.T) {
	now := time.Now()
	st := baseLinkRouteStore()
	st.coreRoutes = []store.CoreLinkRoute{{Domain: "play.example.com", OwnerID: linkRouteUserID, LinkToken: "t",
		TargetHost: "127.0.0.1", TargetPort: 25565, SuspendedAt: &now, SuspendedReason: "abuse"}}
	h := newLinkRouteHandler(st, &linkRouteFakeGateway{}, newLinkRouteRedis(t))

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	h.ListLinkRoutes(rec, r.WithContext(context.WithValue(r.Context(), "userID", linkRouteUserID)))
	var got []struct {
		Domain          string `json:"domain"`
		Suspended       bool   `json:"suspended"`
		SuspendedReason string `json:"suspendedReason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, rec.Body.String())
	}
	if len(got) != 1 || !got[0].Suspended || got[0].SuspendedReason != "abuse" {
		t.Fatalf("owner list = %s", rec.Body.String())
	}
}

func TestSuspendedRouteMapsToItsExternalCode(t *testing.T) {
	if got := externalErrorCode(http.StatusConflict, services.ErrRouteSuspended.Error()); got != "route_suspended" {
		t.Fatalf("code = %q, want route_suspended", got)
	}
}

func (f *linkRouteFakeStore) ListLinkRouteVoice() ([]store.LinkRouteVoice, error) { return nil, nil }
func (f *linkRouteFakeStore) GetVoicePermission(string) (*bool, error)            { return nil, nil }
func (f *linkRouteFakeStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}

func deleteReq(admin bool) *http.Request {
	r := httptest.NewRequest("DELETE", "/x", nil)
	r = mux.SetURLVars(r, map[string]string{"domain": "play.example.com"})
	ctx := context.WithValue(r.Context(), "userID", linkRouteUserID)
	return r.WithContext(context.WithValue(ctx, "isAdmin", admin))
}

func TestOwnerCannotDeleteASuspendedRoute(t *testing.T) {
	now := time.Now()
	st := baseLinkRouteStore()
	st.coreRoutes = []store.CoreLinkRoute{{Domain: "play.example.com", OwnerID: linkRouteUserID, SuspendedAt: &now}}
	gw := &linkRouteFakeGateway{}
	h := newLinkRouteHandler(st, gw, newLinkRouteRedis(t))

	rec := httptest.NewRecorder()
	h.DeleteLinkRoute(rec, deleteReq(false))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "suspended by an administrator") {
		t.Fatalf("owner delete = %d %s, want 409 with the suspension message", rec.Code, rec.Body.String())
	}
	if len(gw.deleted) != 0 {
		t.Fatal("the route was deleted")
	}
	if got := externalErrorCode(rec.Code, rec.Body.String()); got != "route_suspended" {
		t.Fatalf("external code = %q", got)
	}

	rec = httptest.NewRecorder()
	h.d.Redis = nil // skips the best-effort Bedrock republish, which needs a full store
	h.DeleteLinkRoute(rec, deleteReq(true))
	if rec.Code != http.StatusOK || len(gw.deleted) != 1 {
		t.Fatalf("admin delete = %d, deleted=%v", rec.Code, gw.deleted)
	}
}

func (f *linkRouteFakeStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) {
	return f.coreRoutes, nil
}

func TestExternalDeleteOfASuspendedRouteIsRefused(t *testing.T) {
	f := newExtFixture(t)
	now := time.Now()
	f.st.routes[0].SuspendedAt = &now
	rec := httptest.NewRecorder()
	f.h.DeleteRoute(rec, extReq("DELETE", "/", extTenantA, false, map[string]string{"domain": "a.example.com"}, nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if out := decodeExt(t, rec); out["code"] != "route_suspended" {
		t.Fatalf("code = %v: %s", out["code"], rec.Body.String())
	}
	if len(f.gw.deleted) != 0 || f.st.routes[0].Domain != "a.example.com" {
		t.Fatalf("the route was deleted: %v", f.gw.deleted)
	}
}
