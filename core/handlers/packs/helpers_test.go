package packs

import (
	"os"
	"regexp"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"

	"dylaris-core/models"
	"dylaris-core/store"
)

// Copies of the handlers package's test helpers that the pack tests used while
// they lived there. Test files cannot be imported across packages, so the few
// that were shared are repeated here rather than exported from production code.

// testReauthPassword is the acting administrator's password in the account
// delete fixture; see handlers/reauth_test_helpers_test.go.
const testReauthPassword = "correct-horse-battery-staple"

var testReauthHash = func() string {
	h, err := bcrypt.GenerateFromPassword([]byte(testReauthPassword), bcrypt.MinCost)
	if err != nil {
		panic(err)
	}
	return string(h)
}()

// userDeleteWarpStore answers the account delete path with an account that has
// nothing else to tear down; see handlers/user_delete_warp_test.go.
type userDeleteWarpStore struct {
	store.Store
}

func (f *userDeleteWarpStore) GetSetting(string) (string, error) { return "", nil }
func (f *userDeleteWarpStore) GetUserByID(id string) (*models.User, error) {
	return &models.User{ID: id, Username: "victim", Password: testReauthHash}, nil
}
func (f *userDeleteWarpStore) CountServersByOwner(string) (int, error) { return 0, nil }
func (f *userDeleteWarpStore) ListAllWarpAPIKeysByOwner(string) ([]store.WarpAPIKey, error) {
	return nil, nil
}
func (f *userDeleteWarpStore) ListWarpAPIKeysByOwner(string) ([]store.WarpAPIKey, error) {
	return nil, nil
}
func (f *userDeleteWarpStore) RevokeWarpAPIKeyByNodeID(string) error          { return nil }
func (f *userDeleteWarpStore) ListNodesByOwner(string) ([]models.Node, error) { return nil, nil }
func (f *userDeleteWarpStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) {
	return nil, nil
}
func (f *userDeleteWarpStore) DeleteUser(string) error                              { return nil }
func (f *userDeleteWarpStore) InsertAuditIdentity(*models.AuditEventIdentity) error { return nil }
func (f *userDeleteWarpStore) CountAdmins() (int, error)                            { return 5, nil }

// funcBody returns the source of one function, for the tests that pin how a
// handler is written; see handlers/nodes_live_fields_test.go.
func funcBody(t *testing.T, file, fn string) string {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	re := regexp.MustCompile(`(?ms)^func (?:\([^)]*\) )?` + regexp.QuoteMeta(fn) + `\(.*?^}`)
	body := re.FindString(string(src))
	if body == "" {
		t.Fatalf("%s not found in %s", fn, file)
	}
	return body
}

// newServerPowerRedis returns a real client backed by miniredis.
func newServerPowerRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()})
}
