package packs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// packAccessStore: pack 1 is bound to Modrinth project "proj", pack 2 is an
// unbound (legacy) pack. Both were created by "owner".
type packAccessStore struct {
	store.Store
	members map[string]*models.ModrinthMembership // user -> cached membership in "proj"
	stored  []models.ModrinthMembership
}

func (f *packAccessStore) GetPack(id int) (*models.Pack, error) {
	if id == 2 {
		return &models.Pack{ID: 2, OwnerID: "owner"}, nil
	}
	return &models.Pack{ID: id, OwnerID: "owner", ModrinthProjectID: "proj"}, nil
}
func (f *packAccessStore) GetModrinthMembership(user, project string) (*models.ModrinthMembership, error) {
	return f.members[user], nil
}
func (f *packAccessStore) UpsertModrinthMembership(m *models.ModrinthMembership) error {
	f.stored = append(f.stored, *m)
	return nil
}

func member(perms int64) *models.ModrinthMembership {
	now := time.Now()
	return &models.ModrinthMembership{State: stateMember, Permissions: perms, CheckedAt: now, ConfirmedAt: &now}
}

// lapsed is a rejected token whose last real "member" answer is ago old.
func lapsed(ago time.Duration) *models.ModrinthMembership {
	confirmed := time.Now().Add(-ago)
	return &models.ModrinthMembership{State: stateTokenInvalid, Permissions: services.ModrinthPermAll, CheckedAt: time.Now(), ConfirmedAt: &confirmed}
}

// Who may do what with a pack bound to Modrinth: exactly what the Modrinth
// team grants - including for the user who created it here - an admin may
// read, a rejected token keeps read only, anyone else nothing.
func TestPackAccessFollowsTheModrinthTeam(t *testing.T) {
	fs := &packAccessStore{members: map[string]*models.ModrinthMembership{
		"uploader":   member(services.ModrinthPermUploadVersion),
		"deleter":    member(services.ModrinthPermDeleteVersion),
		"pageEditor": member(services.ModrinthPermEditBody),
		"manager":    member(services.ModrinthPermAll),
		"viewer":     member(0),
		"lapsed":     lapsed(time.Hour),
		"expired":    lapsed(services.TokenInvalidGrace + time.Minute),
		"removed":    {State: stateNone, CheckedAt: time.Now()},
	}}
	h := NewPacksHandler(&handlers.AppState{Store: fs})
	cases := []struct {
		user  string
		admin bool
		want  [5]bool // read, edit, delete, page, deletePack
	}{
		{"manager", false, [5]bool{true, true, true, true, true}},
		{"uploader", false, [5]bool{true, true, false, false, false}},
		{"deleter", false, [5]bool{true, false, true, false, false}},
		{"pageEditor", false, [5]bool{true, false, false, true, false}},
		{"viewer", false, [5]bool{true, false, false, false, false}},
		{"lapsed", false, [5]bool{true, false, false, false, false}},
		// A rejected token reads for a day after the last real membership,
		// not forever: a draft answers 404 to a removed member too.
		{"expired", false, [5]bool{}},
		{"removed", false, [5]bool{}},
		// The creator has no shortcut: the team removed them, so they are out.
		{"owner", false, [5]bool{}},
		{"admin", true, [5]bool{true, false, false, false, false}},
		{"stranger", false, [5]bool{}},
		{"", false, [5]bool{}},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		ctx := context.WithValue(r.Context(), "userID", c.user)
		ctx = context.WithValue(ctx, "isAdmin", c.admin)
		r = r.WithContext(ctx)
		for i, need := range []packNeed{packRead, packEdit, packDelete, packEditPage, packDeletePack} {
			if _, got := h.packAccess(r, 1, need); got != c.want[i] {
				t.Errorf("%s need %d: got %v, want %v", c.user, need, got, c.want[i])
			}
		}
	}
}

// An unbound (legacy) pack is its creator's alone.
func TestUnboundPackBelongsToItsCreator(t *testing.T) {
	h := NewPacksHandler(&handlers.AppState{Store: &packAccessStore{members: map[string]*models.ModrinthMembership{"manager": member(services.ModrinthPermAll)}}})
	for _, c := range []struct {
		user  string
		admin bool
		read  bool
		edit  bool
	}{{"owner", false, true, true}, {"manager", false, false, false}, {"admin", true, true, false}} {
		r := httptest.NewRequest("GET", "/", nil)
		r = r.WithContext(context.WithValue(context.WithValue(r.Context(), "userID", c.user), "isAdmin", c.admin))
		if _, got := h.packAccess(r, 2, packRead); got != c.read {
			t.Errorf("%s read: %v", c.user, got)
		}
		if _, got := h.packAccess(r, 2, packEdit); got != c.edit {
			t.Errorf("%s edit: %v", c.user, got)
		}
	}
}

// A cached answer older than five minutes is asked again; a user with no
// usable token then keeps read access only (greyed out until they renew).
func TestStaleMembershipWithoutTokenFallsBackToReadOnly(t *testing.T) {
	stale := member(services.ModrinthPermAll)
	stale.CheckedAt = time.Now().Add(-10 * time.Minute)
	fs := &packAccessStore{members: map[string]*models.ModrinthMembership{"manager": stale}}
	h := NewPacksHandler(&handlers.AppState{Store: fs})
	r := httptest.NewRequest("GET", "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), "userID", "manager"))
	if _, ok := h.packAccess(r, 1, packEdit); ok {
		t.Fatal("a stale membership without a token still allowed editing")
	}
	if len(fs.stored) == 0 || fs.stored[len(fs.stored)-1].State != stateTokenInvalid {
		t.Fatalf("stored = %+v, want token_invalid", fs.stored)
	}
}

// What Modrinth answers, mapped to what the user keeps here. The two that are
// easy to get wrong: a token lacking PROJECT_READ is listed in the team without
// rights (Modrinth reads it as anonymous), and a draft answers such a token 404.
// Neither may silently turn a member into "no access".
func TestModrinthAnswersMapToMembership(t *testing.T) {
	cached := member(services.ModrinthPermUploadVersion)
	cases := []struct {
		name    string
		user    int    // status of GET /v2/user
		members string // body of GET /v3/project/proj/members, "" = 404
		cached  *models.ModrinthMembership
		state   string
		perms   int64
	}{
		{"member", 200, `[{"user":{"id":"mu"},"accepted":true,"permissions":3}]`, nil, stateMember, 3},
		{"listed without rights", 200, `[{"user":{"id":"mu"},"accepted":true,"permissions":null}]`, nil, stateTokenInvalid, 0},
		{"listed without rights keeps cache", 200, `[{"user":{"id":"mu"},"accepted":true,"permissions":null}]`, cached, stateTokenInvalid, services.ModrinthPermUploadVersion},
		{"draft hidden, was member", 200, "", cached, stateTokenInvalid, services.ModrinthPermUploadVersion},
		{"draft hidden, never known", 200, "", nil, stateNone, 0},
		{"token rejected, was member", 401, "", cached, stateTokenInvalid, services.ModrinthPermUploadVersion},
		{"token rejected, never known", 401, "", nil, stateNone, 0},
	}
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	for _, c := range cases {
		http.DefaultTransport = modrinthAddTransport(func(r *http.Request) (*http.Response, error) {
			status, body := 404, `{}`
			switch r.URL.Path {
			case "/v2/user":
				status, body = c.user, `{"id":"mu"}`
			case "/v3/project/proj/members":
				if c.members != "" {
					status, body = 200, c.members
				}
			case "/v3/project/proj":
				status, body = 200, `{"organization":null}`
			}
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		h := NewPacksHandler(&handlers.AppState{Store: &packAccessStore{}})
		h.SetPATLoader(&publishBindingPAT{})
		m, err := h.askModrinthMembership(context.Background(), "u", "proj", c.cached)
		if err != nil || m.State != c.state || m.Permissions != c.perms {
			t.Errorf("%s: got %+v, %v; want %s/%d", c.name, m, err, c.state, c.perms)
			continue
		}
		// Only Modrinth naming the user a member may move the confirmation;
		// a lapsed answer carries the old one, or the grace never ends.
		if m.State == stateTokenInvalid && c.cached != nil && c.members == "" && m.ConfirmedAt != c.cached.ConfirmedAt {
			t.Errorf("%s: a lapsed answer moved the confirmation", c.name)
		}
	}
}
