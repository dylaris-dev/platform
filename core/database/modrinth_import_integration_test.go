package database

import (
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

// An imported pack is created already bound to its Modrinth project, and its
// builds carry where they came from. Both were dropped by the INSERTs once.
func TestIntegrationModrinthImportPersistence(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)
	u := &models.User{Username: uniqueName("import_"), Password: "test"}
	u.Email = u.Username + "@example.test"
	if err := st.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	id, err := st.CreatePack(&models.Pack{OwnerID: u.ID, InternalName: "Imported", InternalSlug: "imported", ModrinthVisibility: "unlisted", ModrinthProjectID: "HGLtI1pC", ModrinthProjectName: "the-lands"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.GetPack(id)
	if err != nil || p.ModrinthProjectID != "HGLtI1pC" || p.ModrinthProjectName != "the-lands" {
		t.Fatalf("pack = %+v, err %v", p, err)
	}
	bid, err := st.CreatePackBuild(&models.PackBuild{PackID: id, VersionString: "1.0.0", Minecraft: "1.20.1", Loader: "forge", ImportedFrom: "modrinth"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.GetPackBuild(bid)
	if err != nil || b.ImportedFrom != "modrinth" {
		t.Fatalf("build = %+v, err %v", b, err)
	}
	if err := st.SetModrinthProjectHidden(u.ID, "HGLtI1pC", true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetModrinthProjectHidden(u.ID, "HGLtI1pC", true); err != nil {
		t.Fatal("hiding twice must not fail:", err)
	}
	if hidden, err := st.HiddenModrinthProjects(u.ID); err != nil || len(hidden) != 1 || hidden[0] != "HGLtI1pC" {
		t.Fatalf("hidden = %v, err %v", hidden, err)
	}
	if err := st.SetModrinthProjectHidden(u.ID, "HGLtI1pC", false); err != nil {
		t.Fatal(err)
	}
	if hidden, _ := st.HiddenModrinthProjects(u.ID); len(hidden) != 0 {
		t.Fatalf("still hidden: %v", hidden)
	}

	// One Modrinth project is one pack: a second binding is refused.
	if _, err := st.CreatePack(&models.Pack{OwnerID: u.ID, InternalName: "Copy", InternalSlug: "copy", ModrinthVisibility: "unlisted", ModrinthProjectID: "HGLtI1pC"}); err == nil {
		t.Fatal("a second pack bound to the same Modrinth project was accepted")
	}
	if got, err := st.GetPackByModrinthProject("HGLtI1pC"); err != nil || got == nil || got.ID != id {
		t.Fatalf("pack by project = %+v, err %v", got, err)
	}
	if got, err := st.GetPackByModrinthProject("unknown"); err != nil || got != nil {
		t.Fatalf("unknown project = %+v, err %v", got, err)
	}

	// Memberships: stored, updated in place, and Clear forgets only what is
	// not a live membership.
	if m, err := st.GetModrinthMembership(u.ID, "HGLtI1pC"); err != nil || m != nil {
		t.Fatalf("unchecked membership = %+v, err %v", m, err)
	}
	for _, m := range []models.ModrinthMembership{
		{UserID: u.ID, ProjectID: "HGLtI1pC", State: "member", Permissions: 1},
		{UserID: u.ID, ProjectID: "HGLtI1pC", State: "member", Permissions: 5},
		{UserID: u.ID, ProjectID: "lapsed", State: "token_invalid", Permissions: 1},
		{UserID: u.ID, ProjectID: "gone", State: "none"},
	} {
		m := m
		if err := st.UpsertModrinthMembership(&m); err != nil {
			t.Fatal(err)
		}
	}
	if m, err := st.GetModrinthMembership(u.ID, "HGLtI1pC"); err != nil || m == nil || m.Permissions != 5 || m.State != "member" || m.CheckedAt.IsZero() {
		t.Fatalf("membership = %+v, err %v", m, err)
	}
	// confirmed_at is stored as given and read back; a later lapsed answer
	// carrying an older confirmation must not be moved to now.
	confirmed := time.Now().Add(-30 * time.Hour).UTC().Truncate(time.Second)
	if err := st.UpsertModrinthMembership(&models.ModrinthMembership{UserID: u.ID, ProjectID: "lapsed", State: "token_invalid", Permissions: 1, ConfirmedAt: &confirmed}); err != nil {
		t.Fatal(err)
	}
	if m, err := st.GetModrinthMembership(u.ID, "lapsed"); err != nil || m.ConfirmedAt == nil || !m.ConfirmedAt.Equal(confirmed) {
		t.Fatalf("lapsed confirmation = %+v, err %v", m, err)
	}
	if all, err := st.ListModrinthMemberships(u.ID); err != nil || len(all) != 3 {
		t.Fatalf("memberships = %+v, err %v", all, err)
	}
	if n, err := st.ClearModrinthMemberships(u.ID); err != nil || n != 2 {
		t.Fatalf("cleared %d, err %v", n, err)
	}
	if all, _ := st.ListModrinthMemberships(u.ID); len(all) != 1 || all[0].ProjectID != "HGLtI1pC" {
		t.Fatalf("after clear = %+v", all)
	}
}
