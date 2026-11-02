package handlertest

import (
	"errors"

	"dylaris-core/models"
	"dylaris-core/store"
)

// OwnedNode is node 7, online, owned by VisOwner: a customer's machine.
func OwnedNode() *models.Node {
	owner := VisOwner
	return &models.Node{ID: 7, Status: "online", OwnerID: &owner}
}

// PlatformNode is node 7, online, with no owner.
func PlatformNode() *models.Node {
	return &models.Node{ID: 7, Status: "online"}
}

// ForeignStore answers the node, the server on it and the BYON flag. A
// SettingErr or NodeErr is a database fault, which must count as foreign.
// Every other store method panics on the nil embedded store.
type ForeignStore struct {
	store.Store
	Node       *models.Node
	NodeErr    error
	Byon       string
	SettingErr error
}

func (f *ForeignStore) GetNodeByID(id int) (*models.Node, error) {
	if f.NodeErr != nil {
		return nil, f.NodeErr
	}
	return f.Node, nil
}

func (f *ForeignStore) GetSetting(key string) (string, error) {
	if key == "feature_byon_enabled" {
		return f.Byon, f.SettingErr
	}
	return "", nil
}

func (f *ForeignStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, UUID: "srv-1", OwnerID: VisOwner, NodeID: f.Node.ID}, nil
}

func (f *ForeignStore) GetServerByUUID(uuid string) (*models.Server, error) {
	return &models.Server{ID: 3, UUID: uuid, OwnerID: VisOwner, NodeID: f.Node.ID}, nil
}

func (f *ForeignStore) GetUserByID(id string) (*models.User, error) {
	return &models.User{ID: id}, nil
}

func (f *ForeignStore) GetPanelRole(int) (*store.PanelRole, error) { return nil, nil }

func (f *ForeignStore) GetServerRole(int) (*store.ServerRole, error) { return nil, nil }

func (f *ForeignStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}

func (f *ForeignStore) GetServerGrant(int, string) (*store.ServerGrant, error) {
	return nil, errors.New("no grant")
}

func (f *ForeignStore) GetAccountGrant(string, string) (*store.ServerGrant, error) {
	return nil, errors.New("no grant")
}

// SettingsStore is a read/write settings map: SetSetting and the audited
// SetSettingBy (actor dropped) write into KV, so a save-then-get round trip
// can be asserted, and a handler writing through either fails its assertion
// rather than panicking on the nil embedded store.
type SettingsStore struct {
	store.Store
	KV map[string]string
}

// NewSettingsStore returns a SettingsStore with an empty map.
func NewSettingsStore() *SettingsStore {
	return &SettingsStore{KV: map[string]string{}}
}

func (f *SettingsStore) GetSetting(key string) (string, error) { return f.KV[key], nil }

func (f *SettingsStore) SetSettingBy(key, value, _ string) error {
	return f.SetSetting(key, value)
}

func (f *SettingsStore) SetSetting(key, value string) error {
	f.KV[key] = value
	return nil
}
