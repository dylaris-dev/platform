package database

import (
	"testing"

	"dylaris-core/store"
)

// The fleet the routing migration recreates carries each server's effective
// cpuset; against a real Postgres because the query names the columns.
func TestIntegrationActiveServersCarryTheEffectiveCpuset(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)
	f := newFixture(t, st)
	if _, err := db.Exec(`UPDATE servers SET status = 'online', cpu_pinning_mode = 'manual', cpuset = '2-3' WHERE id = $1`, f.server.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE nodes SET cpuset_cpus = '0-7' WHERE id = $1`, f.node.ID); err != nil {
		t.Fatal(err)
	}
	servers, err := st.GetAllActiveServers()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range servers {
		if s.UUID == f.server.UUID {
			if s.Cpuset != "2-3" {
				t.Fatalf("cpuset %q, want the pinned 2-3", s.Cpuset)
			}
			return
		}
	}
	t.Fatal("the fixture server is not in the active fleet")
}
