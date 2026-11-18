package database

import (
	"sort"
	"strings"
	"testing"

	"dylaris-core/store"
)

// Only build_modversions cascaded with a build, so a deleted build's (or
// pack's) content rows survived and kept their storage objects referenced for
// good. A row another build still holds must stay.
func TestIntegrationPackDeletesDropContentOnlyTheirBuildsHeld(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)

	var owner string
	if err := db.QueryRow(`INSERT INTO users (username, password, role) VALUES ('content-owner', 'x', 'user') RETURNING id`).Scan(&owner); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	ins := func(q string, args ...any) (id int) {
		t.Helper()
		if err := db.QueryRow(q, args...).Scan(&id); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		return id
	}
	p := ins(`INSERT INTO packs (owner_id, internal_name, internal_slug) VALUES ($1, 'P', 'p') RETURNING id`, owner)
	q := ins(`INSERT INTO packs (owner_id, internal_name, internal_slug) VALUES ($1, 'Q', 'q') RETURNING id`, owner)
	b1 := ins(`INSERT INTO pack_builds (pack_id, version_string) VALUES ($1, '1') RETURNING id`, p)
	b2 := ins(`INSERT INTO pack_builds (pack_id, version_string) VALUES ($1, '2') RETURNING id`, p)
	b3 := ins(`INSERT INTO pack_builds (pack_id, version_string) VALUES ($1, '1') RETURNING id`, q)
	mod := ins(`INSERT INTO mods (owner_id, slug) VALUES ($1, 'm') RETURNING id`, owner)
	mv := map[string]int{}
	for key, builds := range map[string][]int{"a": {b1}, "b": {b1, b2}, "c": {b2, b3}, "d": {b3}} {
		mv[key] = ins(`INSERT INTO modversions (mod_id, storage_key) VALUES ($1, $2) RETURNING id`, mod, "packs/"+owner+"/mods/m/"+key+".zip")
		for _, b := range builds {
			if _, err := db.Exec(`INSERT INTO build_modversions (build_id, modversion_id) VALUES ($1, $2)`, b, mv[key]); err != nil {
				t.Fatal(err)
			}
		}
	}
	left := func() string {
		var keys []string
		for k, id := range mv {
			var n int
			db.QueryRow(`SELECT COUNT(*) FROM modversions WHERE id=$1`, id).Scan(&n)
			if n == 1 {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		return strings.Join(keys, "")
	}
	base := func(keys []string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, k := range keys {
			out = append(out, strings.TrimSuffix(k[strings.LastIndex(k, "/")+1:], ".zip"))
		}
		sort.Strings(out)
		return strings.Join(out, "")
	}

	if got := base(st.ListBuildContentStorageKeys(b1)); got != "ab" {
		t.Fatalf("build content = %q, want ab", got)
	}
	if err := st.DeletePackBuild(b1, q); err != nil || left() != "abcd" {
		t.Fatalf("a build deleted through the wrong pack removed content: %v, left %s", err, left())
	}
	if err := st.DeletePackBuild(b1, p); err != nil || left() != "bcd" {
		t.Fatalf("after the build delete: %v, left %s, want bcd", err, left())
	}
	if err := st.DeletePack(p, "00000000-0000-4000-8000-000000000000"); err != nil || left() != "bcd" {
		t.Fatalf("another owner's pack delete removed content: %v, left %s", err, left())
	}
	if err := st.DeletePack(p, owner); err != nil || left() != "cd" {
		t.Fatalf("after the pack delete: %v, left %s, want cd", err, left())
	}
	if got := base(st.ListOwnerContentStorageKeys(owner)); got != "cd" {
		t.Fatalf("owner content = %q, want cd", got)
	}
}

// One publish of a build at a time; a published build is never claimed, and a
// claim a dead Core left behind expires.
func TestIntegrationPackBuildPublishClaim(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)
	var owner string
	var pack, build int
	if err := db.QueryRow(`INSERT INTO users (username, password, role) VALUES ('publish-owner', 'x', 'user') RETURNING id`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`INSERT INTO packs (owner_id, internal_name, internal_slug) VALUES ($1, 'P', 'p') RETURNING id`, owner).Scan(&pack); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`INSERT INTO pack_builds (pack_id, version_string) VALUES ($1, '1') RETURNING id`, pack).Scan(&build); err != nil {
		t.Fatal(err)
	}
	claim := func(want bool, why string) {
		t.Helper()
		if got, err := st.ClaimPackBuildPublish(build); err != nil || got != want {
			t.Fatalf("%s: claimed=%v err=%v, want %v", why, got, err, want)
		}
	}
	claim(true, "first")
	claim(false, "second while the first runs")
	if err := st.ReleasePackBuildPublish(build); err != nil {
		t.Fatal(err)
	}
	claim(true, "after the release")
	if _, err := db.Exec(`UPDATE pack_builds SET publish_claimed_at = NOW() - interval '1 hour' WHERE id=$1`, build); err != nil {
		t.Fatal(err)
	}
	claim(true, "a stale claim")
	st.ReleasePackBuildPublish(build)
	if _, err := db.Exec(`UPDATE pack_builds SET modrinth_published = TRUE WHERE id=$1`, build); err != nil {
		t.Fatal(err)
	}
	claim(false, "a published build")
}
