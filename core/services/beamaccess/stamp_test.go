package beamaccess

import (
	"slices"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// reachStore answers ListServersForUser the way the Postgres store does: the
// admin list carries the platform's own nodes, the grant-shaped list carries
// what the user owns or was granted, on any node.
type reachStore struct {
	store.Store
	admin      bool
	adminList  []string
	grantsList []string
}

func (f *reachStore) GetUserByID(id string) (*models.User, error) {
	return &models.User{ID: id, IsAdmin: f.admin}, nil
}

func (f *reachStore) ListServersForUser(_ string, isAdmin bool) ([]models.Server, error) {
	list := f.grantsList
	if isAdmin {
		list = f.adminList
	}
	out := make([]models.Server, 0, len(list))
	for _, u := range list {
		out = append(out, models.Server{UUID: u})
	}
	return out, nil
}

func TestReachOfCoversEveryServerTheTicketResolverOpens(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	cases := []struct {
		name string
		st   *reachStore
		want []string
	}{
		{
			// The resolver drops an admin's short-circuit on a customer's node and
			// falls through to the grants, so an account-wide grant there opens a
			// ticket that the admin list never names.
			name: "admin with an account-wide grant on a customer node",
			st: &reachStore{admin: true,
				adminList:  []string{"platform", "own"},
				grantsList: []string{"own", "customer-account-wide"}},
			want: []string{"customer-account-wide", "own", "platform"},
		},
		{
			name: "admin with a proxy-inherited grant on a customer node",
			st: &reachStore{admin: true,
				adminList:  []string{"platform"},
				grantsList: []string{"customer-child"}},
			want: []string{"customer-child", "platform"},
		},
		{
			name: "non-admin reaches only the grant-shaped list",
			st: &reachStore{
				adminList:  []string{"platform"},
				grantsList: []string{"own", "invited"}},
			want: []string{"invited", "own"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ReachOf(tc.st, rdb, "u1")
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ReachOf = %v, want %v", got, tc.want)
			}
		})
	}
}
