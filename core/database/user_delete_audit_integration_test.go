package database

import (
	"testing"

	"dylaris-core/models"
)

// Against a real Postgres, because what is being checked IS the constraint.
//
// audit_events_identity.target_user_id is a foreign key onto users. A row
// written AFTER a hard delete cannot reference the account: the insert is
// refused with 23503, and the audit writer swallows the error - so the row
// simply never appears and the deletion stays unrecorded. ON DELETE SET NULL
// blanks the target of rows that already EXIST; it does nothing for one that
// arrives afterwards.
//
// Measured on production: the first version of the admin delete audit logged
// "violates foreign key constraint audit_events_identity_target_user_id_fkey"
// and wrote nothing. Its unit test passed, because a fake store has no foreign
// keys - which is exactly why this one runs against the database.
//
// Skipped without DYLARIS_TEST_DB_HOST, like its neighbours.
func TestIntegrationDeletionAuditRowSurvivesTheDeletedUser(t *testing.T) {
	_, st := integrationDB(t)

	doomed := &models.User{Username: uniqueName("u_del_"), Password: "x", Email: uniqueName("e_del_") + "@example.test"}
	if err := st.CreateUser(doomed); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	actor := &models.User{Username: uniqueName("u_act_"), Password: "x", Email: uniqueName("e_act_") + "@example.test"}
	if err := st.CreateUser(actor); err != nil {
		t.Fatalf("CreateUser actor: %v", err)
	}
	t.Cleanup(func() { st.DeleteUser(actor.ID) })

	if err := st.DeleteUser(doomed.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	// What the handler writes: no target, the identity in the metadata.
	row := &models.AuditEventIdentity{
		EventType:   "user_hard_deleted",
		ActorUserID: &actor.ID,
		Metadata: map[string]interface{}{
			"userId":   doomed.ID,
			"username": doomed.Username,
			"email":    doomed.Email,
		},
	}
	if err := st.InsertAuditIdentity(row); err != nil {
		t.Fatalf("the deletion row was refused, so the removal goes unrecorded: %v", err)
	}

	// And it is readable afterwards, carrying enough to say what was removed.
	rows, err := st.ListAuditIdentity(nil, "user_hard_deleted", 50)
	if err != nil {
		t.Fatalf("ListAuditIdentity: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Metadata != nil && r.Metadata["userId"] == doomed.ID {
			found = true
			if r.Metadata["username"] != doomed.Username {
				t.Errorf("row username = %v, want %q", r.Metadata["username"], doomed.Username)
			}
			if r.Metadata["email"] != doomed.Email {
				t.Errorf("row email = %v; without it the row cannot identify the account", r.Metadata["email"])
			}
		}
	}
	if !found {
		t.Error("the row was inserted but cannot be read back by its metadata")
	}

	// The other half of the constraint, so the reason for all of this is pinned
	// rather than described: the SAME row WITH a target is refused.
	withTarget := &models.AuditEventIdentity{
		EventType:    "user_hard_deleted",
		ActorUserID:  &actor.ID,
		TargetUserID: &doomed.ID,
	}
	if err := st.InsertAuditIdentity(withTarget); err == nil {
		t.Error("a row referencing the deleted account was accepted; the foreign key this works around is gone")
	}
}
