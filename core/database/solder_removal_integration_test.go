package database

import (
	"testing"
)

// The Solder removal runs on a live database at boot, and a schema error there
// stops Core from starting at all. The push gate runs no Postgres, so this is
// the check that the drop statements are valid against a real one: it plants
// the objects the removal targets, exactly as the removed CREATE statements left
// them, runs the step, and then runs the whole boot schema again.
//
// Skipped without DYLARIS_TEST_DB_HOST, like its neighbours; CI's db-tests job
// runs it against a real Postgres.
func TestIntegrationSolderRemovalOnAnOldSchema(t *testing.T) {
	db := freshSchemaDB(t) // a fully booted CURRENT schema, so no Solder objects

	// What an install that ran the Solder code carries. Written from the
	// removed CREATE statements, not guessed, and including the one index the
	// older boot re-creates on packs.solder_slug.
	for _, q := range []string{
		`ALTER TABLE packs ADD COLUMN solder_display_name VARCHAR(128) NOT NULL DEFAULT ''`,
		`ALTER TABLE packs ADD COLUMN solder_slug VARCHAR(128) NOT NULL DEFAULT ''`,
		`ALTER TABLE packs ADD COLUMN hidden BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE packs ADD COLUMN private BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE packs ADD COLUMN recommended_build VARCHAR(64) NOT NULL DEFAULT ''`,
		`ALTER TABLE packs ADD COLUMN latest_build VARCHAR(64) NOT NULL DEFAULT ''`,
		`ALTER TABLE packs ADD COLUMN icon_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE packs ADD COLUMN logo_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE packs ADD COLUMN background_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE packs ADD COLUMN icon_md5 VARCHAR(32) NOT NULL DEFAULT ''`,
		`ALTER TABLE packs ADD COLUMN logo_md5 VARCHAR(32) NOT NULL DEFAULT ''`,
		`ALTER TABLE packs ADD COLUMN background_md5 VARCHAR(32) NOT NULL DEFAULT ''`,
		`CREATE UNIQUE INDEX packs_owner_solder_slug_uniq ON packs (owner_id, solder_slug) WHERE solder_slug <> ''`,
		`ALTER TABLE pack_builds ADD COLUMN solder_published BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE pack_builds ADD COLUMN solder_private BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE modversions ADD COLUMN url_override TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN solder_handle VARCHAR(64)`,
		`CREATE UNIQUE INDEX users_solder_handle_uniq ON users (solder_handle) WHERE solder_handle IS NOT NULL`,
		`CREATE TABLE loaders (id SERIAL PRIMARY KEY, minecraft TEXT NOT NULL, loader TEXT NOT NULL,
			loader_version TEXT NOT NULL, client_storage_key TEXT NOT NULL DEFAULT '',
			UNIQUE (minecraft, loader, loader_version))`,
		`CREATE TABLE solder_clients (id SERIAL PRIMARY KEY, uuid UUID NOT NULL UNIQUE DEFAULT gen_random_uuid(),
			name VARCHAR(128) NOT NULL DEFAULT '', owner_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE)`,
		`CREATE TABLE pack_clients (id SERIAL PRIMARY KEY,
			pack_id INTEGER NOT NULL REFERENCES packs(id) ON DELETE CASCADE,
			client_id INTEGER NOT NULL REFERENCES solder_clients(id) ON DELETE CASCADE,
			UNIQUE (pack_id, client_id))`,
		`CREATE TABLE solder_keys (id SERIAL PRIMARY KEY, key_hash VARCHAR(64) NOT NULL UNIQUE,
			name VARCHAR(128) NOT NULL DEFAULT '', owner_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE)`,
		`INSERT INTO settings (key, value) VALUES ('solder_delivery_mode', 'core'), ('solder_mirror_url', '')
			ON CONFLICT (key) DO NOTHING`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("planting the old schema: %v\n%s", err, q)
		}
	}

	if err := applySolderRemoval(db); err != nil {
		t.Fatalf("the removal failed on a real database, so Core would not boot: %v", err)
	}

	for _, table := range []string{"loaders", "solder_clients", "pack_clients", "solder_keys"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM information_schema.tables
			WHERE table_schema = current_schema() AND table_name = $1`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("table %s survived the removal", table)
		}
	}
	for _, c := range []struct{ table, column string }{
		{"packs", "solder_slug"}, {"packs", "solder_display_name"}, {"packs", "background_md5"},
		{"pack_builds", "solder_published"}, {"pack_builds", "solder_private"},
		{"modversions", "url_override"}, {"users", "solder_handle"},
	} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`,
			c.table, c.column).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("column %s.%s survived the removal", c.table, c.column)
		}
	}
	var settings int
	if err := db.QueryRow(`SELECT count(*) FROM settings
		WHERE key IN ('solder_delivery_mode', 'solder_mirror_url')`).Scan(&settings); err != nil {
		t.Fatal(err)
	}
	if settings != 0 {
		t.Errorf("%d Solder setting row(s) survived the removal", settings)
	}

	// The next boot runs the whole schema again, removal included. Nothing may
	// re-create what was dropped, and the drops must be a no-op the second time.
	if err := ensureSchema(db, false); err != nil {
		t.Fatalf("a second boot after the removal failed: %v", err)
	}
	var back int
	if err := db.QueryRow(`SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema()
		  AND table_name IN ('loaders', 'solder_clients', 'pack_clients', 'solder_keys')`).Scan(&back); err != nil {
		t.Fatal(err)
	}
	if back != 0 {
		t.Errorf("a second boot re-created %d Solder table(s)", back)
	}
}
