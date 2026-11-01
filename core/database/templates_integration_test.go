package database

import (
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

func TestTemplatesSchemaAndSharesIntegration(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)

	mkUser := func(name string) string {
		var id string
		if err := db.QueryRow(`INSERT INTO users (username, password, role) VALUES ($1, 'x', 'user') RETURNING id`, name).Scan(&id); err != nil {
			t.Fatalf("seed user %s: %v", name, err)
		}
		return id
	}
	a, b := mkUser("tpl-a"), mkUser("tpl-b")

	tpl := &models.Template{OwnerID: a, Name: "Survival", Kind: "server", Software: "paper", McVersion: "1.21.4"}
	if err := st.CreateTemplate(tpl); err != nil || tpl.ID == 0 || tpl.Visibility != "personal" {
		t.Fatalf("CreateTemplate: id %d vis %q err %v", tpl.ID, tpl.Visibility, err)
	}
	if n, err := st.CountTemplatesByOwner(a); err != nil || n != 1 {
		t.Fatalf("CountTemplatesByOwner = %d, %v", n, err)
	}
	if err := st.SetTemplateSize(tpl.ID, 4096); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetTemplate(tpl.ID); err != nil || got == nil || got.SizeBytes != 4096 || got.OwnerName != "tpl-a" {
		t.Fatalf("GetTemplate = %+v, %v", got, err)
	}

	visible := func(includeAdmin, includePlatform bool) int {
		t.Helper()
		l, err := st.ListTemplatesVisibleTo(b, includeAdmin, includePlatform)
		if err != nil {
			t.Fatal(err)
		}
		return len(l)
	}
	if n := visible(false, false); n != 0 {
		t.Fatalf("B sees %d templates before any share", n)
	}

	if err := st.UpsertOwnerShare(a, b, "use"); err != nil {
		t.Fatal(err)
	}
	if n := visible(false, false); n != 1 {
		t.Fatalf("B sees %d templates after the owner share, want 1", n)
	}
	if l, err := st.ListTemplatesVisibleTo(b, false, false); err != nil || len(l) != 1 || l[0].OwnerName != "tpl-a" {
		t.Fatalf("shared list = %+v, %v; want ownerName tpl-a", l, err)
	}
	if lvl, err := st.TemplateShareLevel(tpl.ID, b); err != nil || lvl != "use" {
		t.Fatalf("level = %q, %v; want use", lvl, err)
	}
	// The higher of the two wins, and a lower owner-wide share does not demote it.
	if err := st.UpsertTemplateShare(tpl.ID, b, "edit"); err != nil {
		t.Fatal(err)
	}
	if lvl, _ := st.TemplateShareLevel(tpl.ID, b); lvl != "edit" {
		t.Fatalf("level = %q, want edit", lvl)
	}
	if n := visible(false, false); n != 1 {
		t.Fatalf("B sees %d templates with both shares, want 1 (no duplicates)", n)
	}
	if sh, err := st.ListTemplateShares(tpl.ID); err != nil || len(sh) != 1 || sh[0].Username != "tpl-b" {
		t.Fatalf("ListTemplateShares = %+v, %v", sh, err)
	}
	if sh, err := st.ListOwnerShares(a); err != nil || len(sh) != 1 || sh[0].Level != "use" {
		t.Fatalf("ListOwnerShares = %+v, %v", sh, err)
	}

	// Visibility tiers.
	for _, vis := range []string{"platform", "admin"} {
		p := &models.Template{OwnerID: a, Name: vis, Kind: "proxy", Visibility: vis}
		if err := st.CreateTemplate(p); err != nil {
			t.Fatal(err)
		}
	}
	// Drop B's access so only the tiers can make these visible.
	if err := st.DeleteOwnerShare(a, b); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteTemplateShare(tpl.ID, b); err != nil {
		t.Fatal(err)
	}
	if lvl, _ := st.TemplateShareLevel(tpl.ID, b); lvl != "" {
		t.Fatalf("level after deletes = %q, want empty", lvl)
	}
	if n := visible(false, false); n != 0 {
		t.Fatalf("no flags: %d, want 0", n)
	}
	if n := visible(false, true); n != 1 {
		t.Fatalf("platform only: %d, want 1", n)
	}
	if n := visible(true, false); n != 1 {
		t.Fatalf("admin only: %d, want 1", n)
	}
	if ids, err := st.ListTemplateIDsByOwner(a); err != nil || len(ids) != 3 {
		t.Fatalf("ListTemplateIDsByOwner = %v, %v", ids, err)
	}

	if err := st.UpdateTemplate(&models.Template{ID: tpl.ID, Name: "Renamed", Visibility: "personal"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetTemplate(tpl.ID); got.Name != "Renamed" {
		t.Fatalf("name = %q after update", got.Name)
	}

	// Deleting the owner cascades to templates and both kinds of share row.
	if err := st.UpsertOwnerShare(a, b, "use"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertTemplateShare(tpl.ID, b, "use"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM users WHERE id = $1`, a); err != nil {
		t.Fatalf("delete owner: %v", err)
	}
	for _, tbl := range []string{"templates", "template_shares", "template_owner_shares"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s has %d rows after owner delete (%v)", tbl, n, err)
		}
	}

	if got, err := st.GetTemplate(tpl.ID); err != nil || got != nil {
		t.Errorf("GetTemplate after cascade = %+v, %v; want nil, nil", got, err)
	}
	// DeleteTemplate on a fresh one.
	d := &models.Template{OwnerID: b, Name: "d", Kind: "server"}
	if err := st.CreateTemplate(d); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteTemplate(d.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetTemplate(d.ID); got != nil {
		t.Errorf("template still there after DeleteTemplate")
	}
}

// Storage connection deletion asks this count; a wrong column or a count of
// all templates would either strand files or block every delete.
func TestCountTemplatesOnStorageConnectionIntegration(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)
	var owner string
	if err := db.QueryRow(`INSERT INTO users (username, password, role) VALUES ('tpl-conn', 'x', 'user') RETURNING id`).Scan(&owner); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	for i, conn := range []int{0, 3, 3, 5} {
		tpl := &models.Template{OwnerID: owner, Name: "t" + string(rune('a'+i)), Kind: "server", StorageConnectionID: conn}
		if err := st.CreateTemplate(tpl); err != nil {
			t.Fatalf("CreateTemplate on connection %d: %v", conn, err)
		}
	}
	for _, tc := range []struct{ conn, want int }{{3, 2}, {5, 1}, {9, 0}} {
		if n, err := st.CountTemplatesOnStorageConnection(tc.conn); err != nil || n != tc.want {
			t.Errorf("CountTemplatesOnStorageConnection(%d) = %d, %v; want %d", tc.conn, n, err, tc.want)
		}
	}
}
