package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

func teamMember(id string, accepted, owner bool, perms *int64) ModrinthTeamMember {
	var m ModrinthTeamMember
	m.User.ID, m.Accepted, m.IsOwner, m.Permissions = id, accepted, owner, perms
	return m
}

// labrinth shows a member their permissions only when it read the token as
// that member; a token lacking the read scope is read as anonymous and gets
// the team without permissions. That must not pass as "member with no rights".
func TestMemberPermissionsTellsAnonymousViewFromNoRights(t *testing.T) {
	upload := ModrinthPermUploadVersion
	zero := int64(0)
	cases := []struct {
		name   string
		team   []ModrinthTeamMember
		perms  int64
		member bool
		err    error
	}{
		{"member", []ModrinthTeamMember{teamMember("u", true, false, &upload)}, upload, true, nil},
		{"member without rights", []ModrinthTeamMember{teamMember("u", true, false, &zero)}, 0, true, nil},
		{"owner", []ModrinthTeamMember{teamMember("u", true, true, nil)}, ModrinthPermAll, true, nil},
		{"anonymous view", []ModrinthTeamMember{teamMember("o", true, true, nil), teamMember("u", true, false, nil)}, 0, false, ErrModrinthScopeMissing},
		{"pending invite", []ModrinthTeamMember{teamMember("u", false, false, &upload)}, 0, false, nil},
		{"not in team", []ModrinthTeamMember{teamMember("o", true, true, nil)}, 0, false, nil},
	}
	for _, c := range cases {
		perms, member, err := memberPermissions(c.team, "u")
		if perms != c.perms || member != c.member || !errors.Is(err, c.err) {
			t.Errorf("%s: got (%d, %v, %v), want (%d, %v, %v)", c.name, perms, member, err, c.perms, c.member, c.err)
		}
	}
	if !ModrinthTokenRejected(ErrModrinthScopeMissing) {
		t.Error("a missing read scope must ask the user to renew the token")
	}
}

type reachStore struct {
	store.Store
	m *models.ModrinthMembership
}

func (s *reachStore) GetModrinthMembership(string, string) (*models.ModrinthMembership, error) {
	return s.m, nil
}
func (s *reachStore) UpsertModrinthMembership(m *models.ModrinthMembership) error {
	s.m = m
	return nil
}

// Installs and alerts resolve like the pack handlers: a cached "member" older
// than the stale limit is asked again, and without a usable token it no longer
// installs; a lapsed token reads (alerts) only within its grace.
func TestPackReachableByNeverTrustsAnOldAnswer(t *testing.T) {
	SetPackPATLoader(nil)
	ago := func(d time.Duration) *time.Time { at := time.Now().Add(-d); return &at }
	pack := &models.Pack{ID: 1, OwnerID: "creator", ModrinthProjectID: "proj"}
	cases := []struct {
		name            string
		m               *models.ModrinthMembership
		install, alerts bool
	}{
		{"fresh member", &models.ModrinthMembership{State: MembershipMember, CheckedAt: time.Now(), ConfirmedAt: ago(0)}, true, true},
		{"member cached weeks ago", &models.ModrinthMembership{State: MembershipMember, CheckedAt: *ago(21 * 24 * time.Hour), ConfirmedAt: ago(21 * 24 * time.Hour)}, false, false},
		{"lapsed within grace", &models.ModrinthMembership{State: MembershipLapsed, CheckedAt: time.Now(), ConfirmedAt: ago(time.Hour)}, false, true},
		{"lapsed past grace", &models.ModrinthMembership{State: MembershipLapsed, CheckedAt: time.Now(), ConfirmedAt: ago(TokenInvalidGrace + time.Minute)}, false, false},
		{"nothing cached", nil, false, false},
	}
	for _, c := range cases {
		for _, verified := range []bool{true, false} {
			var m *models.ModrinthMembership
			if c.m != nil {
				copied := *c.m
				m = &copied
			}
			got, _ := PackReachableBy(context.Background(), &reachStore{m: m}, pack, "u", verified)
			want := c.alerts
			if verified {
				want = c.install
			}
			if got != want {
				t.Errorf("%s verified=%v: got %v, want %v", c.name, verified, got, want)
			}
		}
	}
	// An unbound pack stays its creator's.
	if ok, _ := PackReachableBy(context.Background(), &reachStore{}, &models.Pack{OwnerID: "creator"}, "creator", true); !ok {
		t.Error("an unbound pack refused its creator")
	}
}
