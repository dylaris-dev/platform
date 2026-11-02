package packs

import (
	"dylaris-core/handlers"
	"dylaris-core/handlers/handlertest"
	"dylaris-core/models"
	"dylaris-core/services/corestorage"
	"dylaris-core/store"
)

// Copies of the handlers package's test helpers that the pack tests used while
// they lived there. Test files cannot be imported across packages, so the few
// that were shared are repeated here rather than exported from production code.

// userDeleteWarpStore answers the account delete path with an account that has
// nothing else to tear down; see handlers/user_delete_warp_test.go.
type userDeleteWarpStore struct {
	store.Store
}

func (f *userDeleteWarpStore) GetSetting(string) (string, error) { return "", nil }
func (f *userDeleteWarpStore) GetUserByID(id string) (*models.User, error) {
	return &models.User{ID: id, Username: "victim", Password: handlertest.ReauthHash}, nil
}
func (f *userDeleteWarpStore) CountServersByOwner(string) (int, error)      { return 0, nil }
func (f *userDeleteWarpStore) ListTemplateIDsByOwner(string) ([]int, error) { return nil, nil }
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

// withCoreStorage gives a test state the core storage it implicitly was before
// S12 (the AppState methods ran over its own Store, Redis, Events, ...): a
// Manager over the very same objects.
func withCoreStorage(s *handlers.AppState) *handlers.AppState {
	s.CoreStorage = &corestorage.Manager{Store: s.Store, Redis: s.Redis, Events: s.Events, ClusterSecret: s.ClusterSecret, Registry: s.GRPCRegistry}
	return s
}
