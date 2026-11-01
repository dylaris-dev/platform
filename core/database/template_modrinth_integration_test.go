package database

import (
	"testing"

	"dylaris-core/models"
)

func TestIntegrationTemplateModrinth(t *testing.T) {
	db, st := integrationDB(t)
	var owner string
	if err := db.QueryRow(`INSERT INTO users (username, password, role) VALUES ($1, 'x', 'user') RETURNING id`,
		uniqueName("tplmr_u_")).Scan(&owner); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, owner) })
	tpl := &models.Template{OwnerID: owner, Name: "mr", Kind: "server"}
	if err := st.CreateTemplate(tpl); err != nil {
		t.Fatal(err)
	}

	ref := func(path, version string) *models.TemplateModrinthRef {
		return &models.TemplateModrinthRef{TemplateID: tpl.ID, Path: path, ProjectID: "p", VersionID: version,
			FileName: "f.jar", URL: "https://cdn.modrinth.com/data/p/f.jar", Sha1: "s1", Sha512: "s512", SizeBytes: 3}
	}
	for _, r := range []*models.TemplateModrinthRef{
		ref("plugins/a.jar", "v1"), ref("plugins/a.jar", "v2"), // same path: replaced
		ref("plugins/b.jar", "v1"), ref("plugins_x/c.jar", "v1"), ref("mods/d.jar", "v1"),
	} {
		if err := st.UpsertTemplateModrinth(r); err != nil || r.ID == 0 {
			t.Fatalf("upsert %s: id %d, %v", r.Path, r.ID, err)
		}
	}
	refs, err := st.ListTemplateModrinth(tpl.ID)
	if err != nil || len(refs) != 4 || refs[1].Path != "plugins/a.jar" || refs[1].VersionID != "v2" {
		t.Fatalf("list = %+v, %v; want 4 with plugins/a.jar at v2", refs, err)
	}

	// A folder takes what is under it, not a sibling sharing its prefix.
	if err := st.DeleteTemplateModrinthPath(tpl.ID, "plugins"); err != nil {
		t.Fatal(err)
	}
	refs, _ = st.ListTemplateModrinth(tpl.ID)
	if len(refs) != 2 || refs[0].Path != "mods/d.jar" || refs[1].Path != "plugins_x/c.jar" {
		t.Fatalf("after deleting plugins: %+v", refs)
	}
	if ok, err := st.DeleteTemplateModrinth(tpl.ID+1, refs[0].ID); err != nil || ok {
		t.Errorf("delete through another template = %v, %v; want false", ok, err)
	}
	if ok, err := st.DeleteTemplateModrinth(tpl.ID, refs[0].ID); err != nil || !ok {
		t.Errorf("delete by id = %v, %v; want true", ok, err)
	}

	if err := st.DeleteTemplate(tpl.ID); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := db.QueryRow(`SELECT COUNT(*) FROM template_modrinth WHERE template_id = $1`, tpl.ID).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d references outlived their template (%v)", left, err)
	}
}
