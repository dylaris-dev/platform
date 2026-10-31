package database

import "testing"

// An install upgraded from before the removal still holds the Library's table,
// its navbar row and its flag. One boot clears all three, and the next boot
// finds nothing to do and must not fail on it.
func TestLibraryRemovalClearsTheLeftoversIntegration(t *testing.T) {
	db := freshSchemaDB(t)

	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS library_disabled (path TEXT PRIMARY KEY)`,
		`INSERT INTO modules (name, type, icon, url, is_enabled, is_system, position, access_role)
			VALUES ('Library', 'internal', 'folder-open', '/library', TRUE, FALSE, 4, 'admin')`,
		// An operator's own module that happens to share the name is not ours.
		`INSERT INTO modules (name, type, icon, url, is_enabled, is_system, position, access_role)
			VALUES ('Library', 'iframe', 'book', 'https://example.com', TRUE, FALSE, 9, 'all')`,
		`INSERT INTO settings (key, value) VALUES ('feature_library_enabled', 'true')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	for round := 1; round <= 2; round++ {
		if err := applyLibraryRemoval(db); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}

	var table *string
	if err := db.QueryRow(`SELECT to_regclass('library_disabled')::text`).Scan(&table); err != nil {
		t.Fatalf("read table: %v", err)
	}
	if table != nil {
		t.Errorf("library_disabled still exists")
	}
	var urls []string
	rows, err := db.Query(`SELECT url FROM modules WHERE name = 'Library'`)
	if err != nil {
		t.Fatalf("read modules: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			t.Fatal(err)
		}
		urls = append(urls, u)
	}
	if len(urls) != 1 || urls[0] != "https://example.com" {
		t.Errorf("Library module rows left = %v, want only the operator's own", urls)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = 'feature_library_enabled'`).Scan(&n); err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if n != 0 {
		t.Errorf("feature_library_enabled is still set")
	}
}
