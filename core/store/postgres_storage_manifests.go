package store

import (
	"database/sql"
	"fmt"
	"strings"

	"dylaris-core/models"
)

// storageManifestEntryBatchSize bounds how many entries go into one multi-row
// INSERT. 500 rows x 4 placeholders = 2000 parameters, comfortably under
// PostgreSQL's 65535-parameter statement limit even if the row shape grows.
const storageManifestEntryBatchSize = 500

// manifestEntryBatches splits entries into chunks of at most size. Pure, so
// the chunking arithmetic is testable without a database.
func manifestEntryBatches(entries []models.StorageManifestEntry, size int) [][]models.StorageManifestEntry {
	if size <= 0 || len(entries) == 0 {
		if len(entries) == 0 {
			return nil
		}
		size = len(entries)
	}
	var out [][]models.StorageManifestEntry
	for i := 0; i < len(entries); i += size {
		end := i + size
		if end > len(entries) {
			end = len(entries)
		}
		out = append(out, entries[i:end])
	}
	return out
}

// buildManifestEntriesInsert renders one multi-row INSERT for a batch.
// ON CONFLICT DO NOTHING makes a retried insert harmless; the primary key is
// (manifest_id, key), so a duplicate key inside one capture is a no-op rather
// than a hard failure. Returns ("", nil) for an empty batch.
func buildManifestEntriesInsert(manifestID int, batch []models.StorageManifestEntry) (string, []interface{}) {
	if len(batch) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("INSERT INTO storage_manifest_entries (manifest_id, key, size, checksum) VALUES ")
	args := make([]interface{}, 0, len(batch)*4)
	for i, e := range batch {
		if i > 0 {
			b.WriteString(",")
		}
		n := i * 4
		fmt.Fprintf(&b, "($%d,$%d,$%d,$%d)", n+1, n+2, n+3, n+4)
		args = append(args, manifestID, e.Key, e.Size, e.Checksum)
	}
	b.WriteString(" ON CONFLICT (manifest_id, key) DO NOTHING")
	return b.String(), args
}

// CreateStorageManifest writes the header and every entry in ONE transaction.
// A half-written manifest is worse than none: verification would report every
// un-inserted key as "extra" against a perfectly good target.
func (s *PostgresStore) CreateStorageManifest(m *models.StorageManifest, entries []models.StorageManifestEntry) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("create storage manifest: begin: %w", err)
	}
	var id int
	err = tx.QueryRow(`INSERT INTO storage_manifests
		(data_set, backend_label, algo, captured_at, object_count, total_bytes, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		m.DataSet, m.BackendLabel, m.Algo, m.CapturedAt, m.ObjectCount, m.TotalBytes, m.CreatedBy,
	).Scan(&id)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("create storage manifest: insert header: %w", err)
	}
	for _, batch := range manifestEntryBatches(entries, storageManifestEntryBatchSize) {
		q, args := buildManifestEntriesInsert(id, batch)
		if q == "" {
			continue
		}
		if _, err := tx.Exec(q, args...); err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("create storage manifest: insert entries: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("create storage manifest: commit: %w", err)
	}
	m.ID = id
	return id, nil
}

// GetStorageManifest returns one manifest header, or (nil, nil) when absent.
func (s *PostgresStore) GetStorageManifest(id int) (*models.StorageManifest, error) {
	var m models.StorageManifest
	err := s.db.QueryRow(`SELECT id, data_set, backend_label, algo, captured_at, object_count, total_bytes, created_by
		FROM storage_manifests WHERE id = $1`, id).
		Scan(&m.ID, &m.DataSet, &m.BackendLabel, &m.Algo, &m.CapturedAt, &m.ObjectCount, &m.TotalBytes, &m.CreatedBy)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ListStorageManifests returns manifest headers newest-first. An empty
// dataSet lists every data set. limit <= 0 falls back to 50.
func (s *PostgresStore) ListStorageManifests(dataSet string, limit int) ([]models.StorageManifest, error) {
	if limit <= 0 {
		limit = 50
	}
	var (
		rows *sql.Rows
		err  error
	)
	if dataSet == "" {
		rows, err = s.db.Query(`SELECT id, data_set, backend_label, algo, captured_at, object_count, total_bytes, created_by
			FROM storage_manifests ORDER BY captured_at DESC, id DESC LIMIT $1`, limit)
	} else {
		rows, err = s.db.Query(`SELECT id, data_set, backend_label, algo, captured_at, object_count, total_bytes, created_by
			FROM storage_manifests WHERE data_set = $1 ORDER BY captured_at DESC, id DESC LIMIT $2`, dataSet, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.StorageManifest{}
	for rows.Next() {
		var m models.StorageManifest
		if err := rows.Scan(&m.ID, &m.DataSet, &m.BackendLabel, &m.Algo, &m.CapturedAt, &m.ObjectCount, &m.TotalBytes, &m.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListStorageManifestEntries returns every entry of a manifest, ascending by
// key so the copy loop and the CSV export are deterministic.
func (s *PostgresStore) ListStorageManifestEntries(manifestID int) ([]models.StorageManifestEntry, error) {
	rows, err := s.db.Query(`SELECT manifest_id, key, size, checksum
		FROM storage_manifest_entries WHERE manifest_id = $1 ORDER BY key ASC`, manifestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.StorageManifestEntry{}
	for rows.Next() {
		var e models.StorageManifestEntry
		if err := rows.Scan(&e.ManifestID, &e.Key, &e.Size, &e.Checksum); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteStorageManifest removes a manifest; entries go with it via
// ON DELETE CASCADE.
func (s *PostgresStore) DeleteStorageManifest(id int) error {
	_, err := s.db.Exec(`DELETE FROM storage_manifests WHERE id = $1`, id)
	return err
}

// modpackStorageKeysSQL is the union query behind ListModpackStorageKeys.
// Exposed as a function so a test can assert both sources are covered without
// a live database.
//
// The modpacks key space is NOT enumerable from the provider:
// ModpackStorageProvider has no List, and adding one is meaningless for
// LocalProvider (which mirrors across N paths, so "the" key space is
// ambiguous). The keys therefore come from the two DB columns that point at
// storage. Blank values are skipped - both columns DEFAULT ”.
//
// There used to be a third source, the Solder loader zips (loaders.client_storage_key).
// Solder is gone and so is that table; any loaders/ objects still in a bucket are
// orphans that no row points at, which is exactly what the CONSEQUENCE below says
// this check cannot see.
//
// CONSEQUENCE, surfaced in the panel: verification of the modpacks data set is
// authoritative for REFERENCED objects only. An orphan in storage that no DB
// row points at is invisible to both the manifest and the "extra" check.
func modpackStorageKeysSQL() string {
	return `SELECT storage_key FROM modversions WHERE storage_key <> ''
UNION
SELECT mrpack_storage_key FROM pack_builds WHERE mrpack_storage_key <> ''`
}

// ListModpackStorageKeys returns the deduplicated union of every modpack
// storage key referenced by the database.
func (s *PostgresStore) ListModpackStorageKeys() ([]string, error) {
	rows, err := s.db.Query(modpackStorageKeysSQL())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		if strings.TrimSpace(k) == "" {
			continue
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ListModversionSHA512ByStorageKey maps a modversion's storage key to the
// Modrinth-supplied SHA-512 recorded for that mod file, skipping rows
// with no key or no hash.
//
// This is an OPPORTUNISTIC integrity signal about pre-existing data, never a
// manifest source of truth: the hashes are third-party-supplied, they exist
// only for modversions (not for mrpack_storage_key objects), and nothing verifies them against storage today. The manifest's
// checksum column is always the freshly computed SHA-256.
func (s *PostgresStore) ListModversionSHA512ByStorageKey() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT storage_key, sha512 FROM modversions WHERE storage_key <> '' AND sha512 <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, sum string
		if err := rows.Scan(&key, &sum); err != nil {
			return nil, err
		}
		if strings.TrimSpace(key) == "" || strings.TrimSpace(sum) == "" {
			continue
		}
		out[key] = sum
	}
	return out, rows.Err()
}
