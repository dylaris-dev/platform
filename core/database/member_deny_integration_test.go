package database

import (
	"testing"

	"dylaris-core/store"
)

// Replacing a member's permissions kept only the grant side of the overrides,
// so the denies the owner set through /grants vanished and the member's role
// applied in full again.
func TestIntegrationReplacingMemberPermissionsKeepsTheDenies(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)
	f := newFixture(t, st)

	member := f.user.ID
	if err := st.CreateInvite(f.server.ID, member, member, map[string]bool{"console": true}); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if _, err := db.Exec(`UPDATE server_invites SET cap_overrides = '{"grant":["console.read"],"deny":["server.delete","sftp.access"]}'::jsonb WHERE server_id = $1 AND user_id = $2`, f.server.ID, member); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInvitePermissions(f.server.ID, member, map[string]bool{"files": true}); err != nil {
		t.Fatalf("UpdateInvitePermissions: %v", err)
	}
	var deny string
	if err := db.QueryRow(`SELECT cap_overrides->'deny' FROM server_invites WHERE server_id = $1 AND user_id = $2`, f.server.ID, member).Scan(&deny); err != nil {
		t.Fatal(err)
	}
	if deny != `["server.delete", "sftp.access"]` {
		t.Fatalf("deny after the replace = %s", deny)
	}
}
