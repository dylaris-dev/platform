package servers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/authz"
	nodegrpc "dylaris-core/grpc"
	"dylaris-core/handlers"
	"dylaris-core/handlers/handlertest"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/corestorage"
	"dylaris-core/store"
)

// Copies of handlers test fixtures this package's tests use; each keeps its name.

// foreignState builds the state the way main does: the resolver asks the same
// predicate the handlers ask.
func foreignState(node *models.Node) *handlers.AppState {
	st := &handlertest.ForeignStore{Node: node, Byon: "true"}
	s := withCoreStorage(&handlers.AppState{Store: st, Authz: authz.NewResolver(st), FeatureFlags: services.NewFeatureFlags(st)})
	s.Authz.SetForeignNode(s.NodeOwnedByOther)
	return s
}

func newPlacementHandler(t *testing.T, nodes []models.Node) *PlacementHandler {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	fs := &placementFakeStore{nodes: nodes, settings: map[string]string{"routing_mode": "gateway"}}
	return NewPlacementHandler(withCoreStorage(&handlers.AppState{Store: fs, Redis: rdb}))
}

func ownedNode(id int, name string, owner *string) models.Node {
	return models.Node{
		ID: id, Name: name, Token: name, Status: "online", OwnerID: owner,
		TotalRAMMB: 65536, RAMOvercommitRatio: 1.0, TotalCPU: 16, CPUOvercommitRatio: 1.0,
	}
}

// seedHeartbeat writes the discovery key the node writes, so the version comes
// from the same place the What's New view reads it from rather than from a
// second notion of "how old is this node".
func seedHeartbeat(t *testing.T, m *miniredis.Miniredis, token, version string) {
	t.Helper()
	hb := services.NodeHeartbeat{ID: token, ReleaseVersion: version}
	raw, err := json.Marshal(hb)
	if err != nil {
		t.Fatal(err)
	}
	m.Set("dylaris:discovery:"+token, string(raw))
}

// tabListAuthzStore is the resolver's store port, faked: grants per server id and
// per owner, nothing else. Every server is owned by "owner-1".
type tabListAuthzStore struct {
	grants  map[int]*store.ServerGrant
	account *store.ServerGrant
	failOn  int
}

func (f tabListAuthzStore) GetServerByID(id int) (*models.Server, error) {
	if id == f.failOn {
		return nil, errors.New("connection refused")
	}
	return &models.Server{ID: id, OwnerID: "owner-1"}, nil
}

func (f tabListAuthzStore) GetServerByUUID(string) (*models.Server, error) { return nil, sql.ErrNoRows }

func (f tabListAuthzStore) GetPanelRole(int) (*store.PanelRole, error) { return nil, sql.ErrNoRows }

func (f tabListAuthzStore) GetServerRole(int) (*store.ServerRole, error) { return nil, sql.ErrNoRows }

func (f tabListAuthzStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}

func (f tabListAuthzStore) GetServerGrant(serverID int, _ string) (*store.ServerGrant, error) {
	if g, ok := f.grants[serverID]; ok {
		return g, nil
	}
	return nil, sql.ErrNoRows
}

func (f tabListAuthzStore) GetAccountGrant(string, string) (*store.ServerGrant, error) {
	if f.account == nil {
		return nil, sql.ErrNoRows
	}
	return f.account, nil
}

type twoNodeFakeStore struct {
	handlertest.ForeignStore
	target *models.Node
}

// An empty target: the per-node server cap does not refuse (TransferServer).
func (f *twoNodeFakeStore) CountServersByNode(int) (int, error) { return 0, nil }

func (f *twoNodeFakeStore) GetNodeByID(id int) (*models.Node, error) {
	if f.target != nil && id == f.target.ID {
		return f.target, nil
	}
	return f.Node, nil
}

func (f *twoNodeFakeStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, UUID: "srv-1", OwnerID: handlertest.VisOwner, NodeID: f.Node.ID}, nil
}

// The transfer asks whether the owner's account is cut off; it is not here.
func (f *twoNodeFakeStore) GetUserBilling(id string) (*store.UserBilling, error) {
	return &store.UserBilling{UserID: id, Status: "active"}, nil
}

func voiceNodeState(t *testing.T, fs store.Store, path, fixture string) (*handlers.AppState, *miniredis.Miniredis, *handlertest.VoiceNode) {
	t.Helper()
	reg := nodegrpc.NewRegistry()
	node := handlertest.NewVoiceNode(t, reg, path, fixture)
	mr := miniredis.RunT(t)
	return withCoreStorage(&handlers.AppState{Store: fs, GRPCRegistry: reg, Redis: redis.NewClient(&redis.Options{Addr: mr.Addr()})}), mr, node
}

func voiceRaceState(t *testing.T, fs *voiceRaceStore) (*handlers.AppState, *miniredis.Miniredis) {
	t.Helper()
	st, mr, _ := voiceNodeState(t, fs, "config/voicechat/voicechat-server.properties", "svc_before.properties")
	return st, mr
}

type voiceRaceStore struct {
	store.Store
	row     store.VoicePort  // what the sync reads
	current *store.VoicePort // what the table holds by the time it writes; nil = deleted
	updates int
	allocs  int
	mod     string
}

func (f *voiceRaceStore) GetVoicePort(int) (*store.VoicePort, error) {
	r := f.row
	return &r, nil
}

func (f *voiceRaceStore) UpdateVoicePortMod(serverID int, region string, port int, mod string, internalPort int, host string) (bool, error) {
	f.updates++
	f.mod = mod
	c := f.current
	return c != nil && c.ServerID == serverID && c.Region == region && c.Port == port, nil
}

func (f *voiceRaceStore) AllocateVoicePort(store.VoicePort, int, int) (store.VoicePort, error) {
	f.allocs++
	return store.VoicePort{}, nil
}

type voiceSyncFakeStore struct {
	store.Store
	row   *store.VoicePort
	calls int
}

func (f *voiceSyncFakeStore) GetVoicePort(int) (*store.VoicePort, error) {
	f.calls++
	return f.row, nil
}

type placementFakeStore struct {
	store.Store
	nodes    []models.Node
	settings map[string]string
}

func (f *placementFakeStore) ListNodes() ([]models.Node, error) { return f.nodes, nil }

func (f *placementFakeStore) GetSetting(k string) (string, error) {
	return f.settings[k], nil
}

func (f *placementFakeStore) SumAllocatedByNode(int) (int64, float64, error) { return 0, 0, nil }

func (f *placementFakeStore) CountServersByNode(int) (int, error) { return 0, nil }

type demoListStore struct {
	store.Store
	settings map[string]string
	// deleteInLock deletes the server between the handler's first load and
	// the locked edit, as a delete on the other replica would.
	deleteInLock bool
	deleted      bool
}

func (f *demoListStore) GetSetting(k string) (string, error) { return f.settings[k], nil }

func (f *demoListStore) SetSetting(k, v string) error { f.settings[k] = v; return nil }

func (f *demoListStore) UpdateSetting(k string, fn func(string) (string, error)) error {
	if f.deleteInLock {
		f.deleted = true
	}
	v, err := fn(f.settings[k])
	if err == nil {
		f.settings[k] = v
	}
	return err
}

func (f *demoListStore) GetServerByID(id int) (*models.Server, error) {
	if f.deleted {
		return nil, sql.ErrNoRows
	}
	return &models.Server{ID: id, UUID: "srv-" + strconv.Itoa(id), NodeID: 1}, nil
}

func (f *demoListStore) GetNodeByID(id int) (*models.Node, error) { return &models.Node{ID: id}, nil }

type demoStaffStore struct {
	store.Store
	user  *models.User
	roleC []string
}

func (f *demoStaffStore) GetUserByUsername(string) (*models.User, error) { return f.user, nil }

func (f *demoStaffStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	if f.roleC == nil {
		return nil, store.CapOverrides{}, nil
	}
	id := 1
	return &id, store.CapOverrides{}, nil
}

func (f *demoStaffStore) GetPanelRole(int) (*store.PanelRole, error) {
	return &store.PanelRole{Capabilities: f.roleC}, nil
}

func (f *demoStaffStore) SetSetting(string, string) error { return nil }

// memberDeleteStore gives the caller members.delete on server 5 through a role.
type memberDeleteStore struct{ *revokeStore }

func (f *memberDeleteStore) GetServerGrant(serverID int, userID string) (*store.ServerGrant, error) {
	g, err := f.revokeStore.GetServerGrant(serverID, userID)
	if g != nil && serverID == 5 {
		one := 1
		g.ServerRoleID = &one
	}
	return g, err
}

func (f *memberDeleteStore) GetServerRole(int) (*store.ServerRole, error) {
	return &store.ServerRole{ID: 1, Capabilities: []string{"members.delete"}}, nil
}

// revokeStore: backend 5 linked to proxy 9, both owned by "owner"; "member"
// holds a narrow direct grant on 5 and an inherit grant on 9.
type revokeStore struct {
	store.Store
	deleted    bool
	proxyGrant bool
	staleRealm bool
}

func (f *revokeStore) GetServerByID(id int) (*models.Server, error) {
	if id == 9 {
		return &models.Server{ID: 9, OwnerID: "owner", ServerType: "proxy"}, nil
	}
	p := 9
	return &models.Server{ID: 5, OwnerID: "owner", ProxyID: &p}, nil
}

func (f *revokeStore) GetServerGrant(serverID int, userID string) (*store.ServerGrant, error) {
	if serverID == 9 && f.proxyGrant {
		realm := "owner"
		if f.staleRealm {
			realm = "previous-owner"
		}
		return &store.ServerGrant{UserID: userID, OwnerUserID: realm, Inherit: true}, nil
	}
	return &store.ServerGrant{UserID: userID, OwnerUserID: "owner"}, nil
}

func (f *revokeStore) GetUserByUsername(name string) (*models.User, error) {
	return &models.User{ID: "target", Username: name}, nil
}

func (f *revokeStore) GetNodeByID(id int) (*models.Node, error) { return &models.Node{ID: id}, nil }

func (f *revokeStore) DeleteInvite(int, string) error { f.deleted = true; return nil }

func (f *revokeStore) DeleteServerGrant(*int, string, string) error {
	f.deleted = true
	return nil
}

func (f *revokeStore) GetSetting(string) (string, error) { return "", nil }

func (f *revokeStore) GetAccountGrant(string, string) (*store.ServerGrant, error) {
	return nil, nil
}

func (f *revokeStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}

func (f *revokeStore) GetServerRole(int) (*store.ServerRole, error) { return nil, nil }

func (f *revokeStore) GetServerAuditState(int) (bool, bool, int, error) { return false, false, 0, nil }

// withCoreStorage gives a test state the core storage it implicitly was before
// S12 (the AppState methods ran over its own Store, Redis, Events, ...): a
// Manager over the very same objects.
func withCoreStorage(s *handlers.AppState) *handlers.AppState {
	s.CoreStorage = &corestorage.Manager{Store: s.Store, Redis: s.Redis, Events: s.Events, ClusterSecret: s.ClusterSecret, Registry: s.GRPCRegistry}
	return s
}
