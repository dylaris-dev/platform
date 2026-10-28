package database

import (
	"testing"

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
}
