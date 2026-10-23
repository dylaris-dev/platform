package database

import (
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// The server list carries the CPU pinning and auto-move a server really has:
// the panel's resources dialog prefills from a list row, and a row that read
// "shared" made every save of RAM or CPU reset the pinning.
func TestIntegrationServerListCarriesPinningAndAutoMove(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)
	f := newFixture(t, st)
	if _, err := db.Exec(`UPDATE servers SET cpu_pinning_mode = 'manual', cpuset = '2-3', auto_move = true WHERE id = $1`, f.server.ID); err != nil {
		t.Fatal(err)
	}
	find := func(name string, list []models.Server, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, s := range list {
			if s.ID == f.server.ID {
				if s.CPUPinningMode != "manual" || s.Cpuset != "2-3" || !s.AutoMove {
					t.Errorf("%s: pinning %q cpuset %q autoMove %v", name, s.CPUPinningMode, s.Cpuset, s.AutoMove)
				}
				return
			}
		}
		t.Errorf("%s: fixture server missing", name)
	}
	owner, err := st.ListServersForUser(f.server.OwnerID, false)
	find("owner list", owner, err)
	admin, err := st.ListServersForUser(f.server.OwnerID, true)
	find("admin list", admin, err)
	all, err := st.ListAllServers()
	find("fleet", all, err)
	// The per-core view decides "are these cores this server's alone" from
	// this read; a row without mode and cpuset reads as running everywhere.
	pins, err := st.ListServerPinningByNode(f.server.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, s := range pins {
		if s.ID == f.server.ID {
			seen = true
			if s.CPUPinningMode != "manual" || s.Cpuset != "2-3" {
				t.Errorf("pinning by node: mode %q cpuset %q, want manual 2-3", s.CPUPinningMode, s.Cpuset)
			}
		}
	}
	if !seen {
		t.Error("pinning by node: fixture server missing")
	}
}
