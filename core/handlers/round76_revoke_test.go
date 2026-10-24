package handlers

import (
	"database/sql"
	"strconv"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

type demoListStore struct {
	store.Store
	settings map[string]string
	// deleteInLock deletes the server between the handler's first load and
	// the locked edit, as a delete on the other replica would.
	deleteInLock bool
	deleted      bool
}

func (f *demoListStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *demoListStore) SetSetting(k, v string) error        { f.settings[k] = v; return nil }
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

// The demo list holds UUIDs and a UUID can come back (CreateServer accepts one,
// adopting a leftover folder reuses it), so a deleted server left on the list
// made the next server with that UUID readable by every signed-in account.
func TestDeletedServersLeaveTheDemoList(t *testing.T) {
	fs := &demoListStore{settings: map[string]string{demoServerUUIDsSetting: `["keep","gone"]`}}
	s := &AppState{Store: fs, StoreEnabled: true}
	dropDemoServers(s, []string{"gone"})
	if isDemoServer(s, "gone") || !isDemoServer(s, "keep") {
		t.Fatalf("demo list after delete: %s", fs.settings[demoServerUUIDsSetting])
	}
}

func (f *demoListStore) GetServerByID(id int) (*models.Server, error) {
	if f.deleted {
		return nil, sql.ErrNoRows
	}
	return &models.Server{ID: id, UUID: "srv-" + strconv.Itoa(id), NodeID: 1}, nil
}
func (f *demoListStore) GetNodeByID(id int) (*models.Node, error) { return &models.Node{ID: id}, nil }
