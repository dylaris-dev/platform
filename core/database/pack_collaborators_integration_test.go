package database

import (
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// Invites merge (an all-packs invite plus a per-pack one), are replaced not
// duplicated, are scoped to their owner for changes, and the history of a
// deleted pack survives. Skipped without DYLARIS_TEST_DB_HOST.
func TestIntegrationPackCollaborators(t *testing.T) {
	_, st := integrationDB(t)
	a := newFixture(t, st) // a.user owns the packs
	b := newFixture(t, st) // b.user is invited

	p1 := &models.Pack{OwnerID: a.user.ID, InternalName: "One", InternalSlug: "one", ModrinthVisibility: "unlisted"}
	id1, err := st.CreatePack(p1)
	if err != nil {
		t.Fatal(err)
	}
	p2 := &models.Pack{OwnerID: a.user.ID, InternalName: "Two", InternalSlug: "two", ModrinthVisibility: "unlisted"}
	id2, err := st.CreatePack(p2)
	if err != nil {
		t.Fatal(err)
	}

	r, _ := st.PackRightsFor(a.user.ID, b.user.ID, id1)
	if r.Found {
		t.Fatal("rights without an invite")
	}
	// Edit on every pack, delete on pack 1 only.
	if _, err := st.UpsertPackCollaborator(store.PackCollaborator{OwnerID: a.user.ID, UserID: b.user.ID, CanEdit: true, CanCreate: true}, a.user.ID); err != nil {
		t.Fatal(err)
	}
	one := id1
	cid, err := st.UpsertPackCollaborator(store.PackCollaborator{OwnerID: a.user.ID, UserID: b.user.ID, PackID: &one, CanDelete: true}, a.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := st.PackRightsFor(a.user.ID, b.user.ID, id1); !r.Found || !r.CanEdit || !r.CanDelete || !r.CanCreate {
		t.Fatalf("pack 1 rights %+v, want edit+delete (+create from the all-packs invite)", r)
	}
	if r, _ := st.PackRightsFor(a.user.ID, b.user.ID, id2); !r.CanEdit || r.CanDelete {
		t.Fatalf("pack 2 rights %+v, want edit only", r)
	}
	// Re-inviting replaces rather than adds a row.
	if _, err := st.UpsertPackCollaborator(store.PackCollaborator{OwnerID: a.user.ID, UserID: b.user.ID, PackID: &one, CanDelete: false}, a.user.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.ListPackCollaboratorsByOwner(a.user.ID); len(list) != 2 {
		t.Fatalf("%d invites after a re-invite, want 2", len(list))
	}
	// The invited user cannot change the owner's invite.
	if err := st.DeletePackCollaborator(b.user.ID, cid); err == nil {
		t.Fatal("a collaborator deleted an invite through their own id")
	}

	if err := st.AddPackAudit(a.user.ID, id1, "One", b.user.ID, "content.added", map[string]any{"buildId": "3"}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeletePack(id1, a.user.ID); err != nil {
		t.Fatal(err)
	}
	ev, err := st.ListPackAudit(a.user.ID, 0, 50)
	if err != nil || len(ev) != 1 || ev[0].PackID != nil || ev[0].PackName != "One" || ev[0].ActorID != b.user.ID {
		t.Fatalf("history after the pack was deleted: %+v %v", ev, err)
	}
	// The pack-1 invite went with the pack; the all-packs one stays.
	if list, _ := st.ListPackCollaborationsForUser(b.user.ID); len(list) != 1 || list[0].PackID != nil {
		t.Fatalf("invites after deleting pack 1: %+v", list)
	}
	_ = id2
}
