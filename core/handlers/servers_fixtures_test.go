package handlers

import (
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-core/store"
)

// Copies of handlers/servers test fixtures that handlers tests still use.

// newServerPowerRedis returns a real *redis.Client backed by miniredis, used
// both for the install-cooldown TTL key and (wrapped in a QueueService) for
// the node command stream.
func newServerPowerRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()})
}

// serverPowerFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only the methods ServerPowerHandler and
// byonNodeServerCapReached touch are overridden. Any other call would panic -
// these tests never make one.
type serverPowerFakeStore struct {
	store.Store

	server    *models.Server
	serverErr error

	invite    *models.ServerInvite
	inviteErr error

	billing    *store.UserBilling
	billingErr error

	node    *models.Node
	nodeErr error

	settings map[string]string

	countServersByNode    int
	countServersByNodeErr error

	// serverGrant + serverRole back the authz.Resolver's delegation path (a
	// non-owner holding a per-server capability grant): set both to give
	// serverGrant.UserID the caps listed on serverRole.Capabilities.
	serverGrant *store.ServerGrant
	serverRole  *store.ServerRole

	updateDesiredStateCalls []serverPowerDesiredStateCall
	updateStatusCalls       []serverPowerStatusCall

	auditEnabled   bool
	insertedAudits []*models.ServerAuditEvent

	setRconNeedsRestartCalls []bool
}

// The following satisfy authz.Store (beyond GetServerByID, already overridden
// above) so h.state.Authz.Resolve does not panic on the nil embedded
// store.Store. None of these tests exercise panel roles or account grants, so
// they report "not found" unless a test explicitly sets serverGrant/serverRole.
func (f *serverPowerFakeStore) GetServerByUUID(uuid string) (*models.Server, error) {
	return nil, errors.New("not found")
}

func (f *serverPowerFakeStore) GetPanelRole(id int) (*store.PanelRole, error) {
	return nil, errors.New("not found")
}

func (f *serverPowerFakeStore) GetServerRole(id int) (*store.ServerRole, error) {
	if f.serverRole != nil && f.serverRole.ID == id {
		return f.serverRole, nil
	}
	return nil, errors.New("not found")
}

func (f *serverPowerFakeStore) GetUserPanelAuthz(userID string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}

func (f *serverPowerFakeStore) GetServerGrant(serverID int, userID string) (*store.ServerGrant, error) {
	if f.serverGrant != nil && f.serverGrant.UserID == userID {
		return f.serverGrant, nil
	}
	return nil, errors.New("not found")
}

func (f *serverPowerFakeStore) GetAccountGrant(ownerUserID, userID string) (*store.ServerGrant, error) {
	return nil, errors.New("not found")
}

func (f *serverPowerFakeStore) GetServerByID(id int) (*models.Server, error) {
	return f.server, f.serverErr
}

func (f *serverPowerFakeStore) GetInvite(serverID int, userID string) (*models.ServerInvite, error) {
	return f.invite, f.inviteErr
}

func (f *serverPowerFakeStore) GetUserBilling(userID string) (*store.UserBilling, error) {
	if f.billingErr != nil {
		return nil, f.billingErr
	}
	if f.billing != nil {
		return f.billing, nil
	}
	return &store.UserBilling{UserID: userID, Status: "active"}, nil
}

func (f *serverPowerFakeStore) GetNodeByID(id int) (*models.Node, error) {
	return f.node, f.nodeErr
}

func (f *serverPowerFakeStore) UpdateServerDesiredState(id int, desiredState string) error {
	f.updateDesiredStateCalls = append(f.updateDesiredStateCalls, serverPowerDesiredStateCall{id, desiredState})
	return nil
}

func (f *serverPowerFakeStore) UpdateServerStatus(id int, status string) error {
	f.updateStatusCalls = append(f.updateStatusCalls, serverPowerStatusCall{id, status})
	return nil
}

func (f *serverPowerFakeStore) SetServerRconNeedsRestart(serverID int, needsRestart bool) error {
	f.setRconNeedsRestartCalls = append(f.setRconNeedsRestartCalls, needsRestart)
	return nil
}

// GetServerAuditState reports audit disabled by default (LogServerAudit then
// no-ops); set auditEnabled to exercise the audit-insert path.
func (f *serverPowerFakeStore) GetServerAuditState(serverID int) (bool, bool, int, error) {
	return f.auditEnabled, false, 0, nil
}

func (f *serverPowerFakeStore) InsertServerAudit(ev *models.ServerAuditEvent) error {
	f.insertedAudits = append(f.insertedAudits, ev)
	return nil
}

func (f *serverPowerFakeStore) GetSetting(key string) (string, error) {
	return f.settings[key], nil
}

func (f *serverPowerFakeStore) CountServersByNode(nodeID int) (int, error) {
	return f.countServersByNode, f.countServersByNodeErr
}

type serverPowerDesiredStateCall struct {
	id    int
	state string
}

type serverPowerStatusCall struct {
	id     int
	status string
}
