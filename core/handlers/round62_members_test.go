package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

const round62InviteBody = `{"username":"guest","permissions":{"console":true}}`

// permissions_mode off means nobody but a panel admin delegates access. The
// legacy member routes ignored it while /api/grants enforced it.
func TestMemberRoutesHonourPermissionsModeOff(t *testing.T) {
	st := &inviteInputStore{mode: authz.ModeOff}
	rec := httptest.NewRecorder()
	(&MemberHandler{state: &AppState{Store: st}}).InviteMember(rec, inviteReq(round62InviteBody))
	if rec.Code != http.StatusForbidden || st.created {
		t.Fatalf("invite with mode off: %d created=%v", rec.Code, st.created)
	}

	admin := &inviteInputStore{mode: authz.ModeOff}
	req := inviteReq(round62InviteBody)
	req = req.WithContext(context.WithValue(req.Context(), "isAdmin", true))
	rec = httptest.NewRecorder()
	(&MemberHandler{state: &AppState{Store: admin}}).InviteMember(rec, req)
	if !admin.created {
		t.Fatalf("an admin was refused with mode off: %d %s", rec.Code, rec.Body.String())
	}

	// The bypass is the admin flag alone, not owning the server.
	stranger := httptest.NewRequest("POST", "/api/servers/1/members", nil)
	ctx := context.WithValue(stranger.Context(), "userID", "admin-9")
	stranger = stranger.WithContext(context.WithValue(ctx, "isAdmin", true))
	if refuseDelegationWhenOff(httptest.NewRecorder(), stranger, admin) {
		t.Fatal("an admin on someone else's server was refused with mode off")
	}

	simple := &inviteInputStore{}
	rec = httptest.NewRecorder()
	(&MemberHandler{state: &AppState{Store: simple}}).InviteMember(rec, inviteReq(round62InviteBody))
	if !simple.created {
		t.Fatalf("invite with the default mode refused: %d %s", rec.Code, rec.Body.String())
	}

	ps := &patchPermsStore{mode: authz.ModeOff}
	rec = httptest.NewRecorder()
	(&MemberHandler{state: &AppState{Store: ps}}).UpdateMemberPermissions(rec, patchReq(`{"permissions":{"console":true}}`))
	if rec.Code != http.StatusForbidden || ps.updated {
		t.Fatalf("patch with mode off: %d updated=%v", rec.Code, ps.updated)
	}
}

// inviteTargetStore answers GetUserByUsername with a chosen account.
type inviteTargetStore struct {
	inviteInputStore
	target models.User
}

func (f *inviteTargetStore) GetUserByUsername(string) (*models.User, error) {
	u := f.target
	return &u, nil
}

// Nobody invites themselves, and the owner is recognised by id, not by name.
func TestInviteRefusesYourselfAndTheOwner(t *testing.T) {
	for name, target := range map[string]models.User{
		"yourself":  {ID: "member-7", Username: "member"},
		"the owner": {ID: "owner-1", Username: "renamed-owner"},
	} {
		st := &inviteTargetStore{target: target}
		req := inviteReq(round62InviteBody)
		req = req.WithContext(context.WithValue(req.Context(), "userID", "member-7"))
		rec := httptest.NewRecorder()
		(&MemberHandler{state: &AppState{Store: st}}).InviteMember(rec, req)
		if rec.Code != http.StatusBadRequest || st.created {
			t.Errorf("%s: %d created=%v", name, rec.Code, st.created)
		}
	}
}

// inheritStore serves a child server linked to proxy 9 and one inheriting
// member on that proxy.
type inheritStore struct {
	store.Store
	proxyOwner string
}

func (f *inheritStore) GetServerByID(id int) (*models.Server, error) {
	if id == 9 {
		return &models.Server{ID: 9, OwnerID: f.proxyOwner}, nil
	}
	proxy := 9
	return &models.Server{ID: id, OwnerID: "owner-a", ProxyID: &proxy}, nil
}
func (f *inheritStore) ListInvitesByServer(int) ([]models.ServerInvite, error) {
	return []models.ServerInvite{{UserID: "m-1", Username: "member", Permissions: models.TabPermissions{Inherit: true}}}, nil
}
func (f *inheritStore) GetInvite(int, string) (*models.ServerInvite, error) {
	return nil, sql.ErrNoRows
}

// Inherited members only come from a proxy of the same owner.
func TestInheritedMembersStayWithinOneOwner(t *testing.T) {
	list := func(proxyOwner string) int {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/servers/5/members/inherited", nil)
		req = mux.SetURLVars(req, map[string]string{"id": "5"})
		rec := httptest.NewRecorder()
		(&MemberHandler{state: &AppState{Store: &inheritStore{proxyOwner: proxyOwner}}}).GetInheritedMembers(rec, req)
		var out struct{ Members []map[string]any }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		return len(out.Members)
	}
	if n := list("owner-a"); n != 1 {
		t.Fatalf("same owner: %d members, want 1", n)
	}
	if n := list("owner-b"); n != 0 {
		t.Fatalf("another owner's proxy listed %d members", n)
	}
}

// A member roster carries no email addresses.
func TestServerInviteHasNoEmail(t *testing.T) {
	b, _ := json.Marshal(models.ServerInvite{})
	if strings.Contains(strings.ToLower(string(b)), "email") {
		t.Fatalf("roster entry exposes email: %s", b)
	}
}
