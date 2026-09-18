package store

import (
	"database/sql"
	"strings"
	"time"

	"dylaris-core/models"
)

// prefixCols turns "a, b, c" into "alias.a, alias.b, alias.c" so a joined
// SELECT scans in the exact order of the shared column constant.
func prefixCols(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

func (s *PostgresStore) UpsertMod(m *models.Mod) (int, error) {
	ct := m.ContentType
	if ct == "" {
		ct = models.ContentTypeMod
	}
	var id int
	err := s.db.QueryRow(`INSERT INTO mods (owner_id, slug, pretty_name, author, description, link, content_type)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (owner_id, slug) DO UPDATE SET
			pretty_name=EXCLUDED.pretty_name, author=EXCLUDED.author,
			description=EXCLUDED.description, link=EXCLUDED.link, content_type=EXCLUDED.content_type
		RETURNING id`,
		m.OwnerID, m.Slug, m.PrettyName, m.Author, m.Description, m.Link, ct,
	).Scan(&id)
	return id, err
}

func (s *PostgresStore) GetModBySlug(ownerID, slug string) (*models.Mod, error) {
	var m models.Mod
	err := s.db.QueryRow(`SELECT id, owner_id, slug, pretty_name, author, description, link, content_type
		FROM mods WHERE owner_id=$1 AND slug=$2`, ownerID, slug).
		Scan(&m.ID, &m.OwnerID, &m.Slug, &m.PrettyName, &m.Author, &m.Description, &m.Link, &m.ContentType)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &m, err
}

const mvCols = `id, mod_id, version, filesize, storage_key, md5, sha1, sha512,
	source, target_path, modrinth_project_id, modrinth_version_id, modrinth_version_number,
	modrinth_game_versions, modrinth_latest_version_id, modrinth_last_checked, created_at, updated_at,
	modrinth_download_url`

func scanModversion(row interface{ Scan(...interface{}) error }) (*models.Modversion, error) {
	var v models.Modversion
	if err := row.Scan(&v.ID, &v.ModID, &v.Version, &v.Filesize, &v.StorageKey, &v.MD5, &v.SHA1, &v.SHA512,
		&v.Source, &v.TargetPath, &v.ModrinthProjectID, &v.ModrinthVersionID, &v.ModrinthVersionNumber,
		&v.ModrinthGameVersions, &v.ModrinthLatestVersionID, &v.ModrinthLastChecked, &v.CreatedAt, &v.UpdatedAt,
		&v.ModrinthDownloadURL); err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *PostgresStore) CreateModversion(mv *models.Modversion) (int, error) {
	src := mv.Source
	if src == "" {
		src = models.SourceUpload
	}
	var id int
	err := s.db.QueryRow(`INSERT INTO modversions
		(mod_id, version, filesize, storage_key, md5, sha1, sha512, source, target_path,
		 modrinth_project_id, modrinth_version_id, modrinth_version_number, modrinth_game_versions, modrinth_download_url)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`,
		mv.ModID, mv.Version, mv.Filesize, mv.StorageKey, mv.MD5, mv.SHA1, mv.SHA512, src, mv.TargetPath,
		mv.ModrinthProjectID, mv.ModrinthVersionID, mv.ModrinthVersionNumber, mv.ModrinthGameVersions, mv.ModrinthDownloadURL,
	).Scan(&id)
	return id, err
}

func (s *PostgresStore) UpdateModversion(mv *models.Modversion) error {
	_, err := s.db.Exec(`UPDATE modversions SET
		version=$1, filesize=$2, storage_key=$3, md5=$4, sha1=$5, sha512=$6,
		source=$7, target_path=$8, modrinth_project_id=$9, modrinth_version_id=$10, modrinth_version_number=$11,
		modrinth_game_versions=$12, modrinth_latest_version_id=$13, modrinth_last_checked=$14,
		modrinth_download_url=$15, updated_at=NOW()
		WHERE id=$16`,
		mv.Version, mv.Filesize, mv.StorageKey, mv.MD5, mv.SHA1, mv.SHA512,
		mv.Source, mv.TargetPath, mv.ModrinthProjectID, mv.ModrinthVersionID, mv.ModrinthVersionNumber,
		mv.ModrinthGameVersions, mv.ModrinthLatestVersionID, mv.ModrinthLastChecked, mv.ModrinthDownloadURL, mv.ID)
	return err
}

func (s *PostgresStore) GetModversion(id int) (*models.Modversion, error) {
	row := s.db.QueryRow(`SELECT `+mvCols+` FROM modversions WHERE id=$1`, id)
	v, err := scanModversion(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return v, err
}

// CountModversionsByStorageKey returns how many modversion rows point at one
// storage object.
//
// The object is NOT owned by a single row. MigrateBuild's copyUploadedContent
// deliberately creates a NEW modversion pointing at the SAME storage key, so
// that updating one build's row cannot rewrite the other's - which leaves two
// rows on one object. Anything about to DELETE that object has to ask this
// first, or it takes the file out from under the other build.
func (s *PostgresStore) CountModversionsByStorageKey(key string) (int, error) {
	if key == "" {
		return 0, nil
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM modversions WHERE storage_key=$1`, key).Scan(&n)
	return n, err
}

// FindModversionBySHA1 finds an existing artifact by the owner's catalog hash,
// used to auto-link/dedupe an uploaded jar. Joins through mods for owner scope.
func (s *PostgresStore) FindModversionBySHA1(ownerID, sha1 string) (*models.Modversion, error) {
	if sha1 == "" {
		return nil, nil
	}
	row := s.db.QueryRow(`SELECT `+prefixCols("mv", mvCols)+`
		FROM modversions mv JOIN mods m ON m.id = mv.mod_id
		WHERE m.owner_id=$1 AND mv.sha1=$2 LIMIT 1`, ownerID, sha1)
	v, err := scanModversion(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return v, err
}

func (s *PostgresStore) AttachModversionToBuild(buildID, modversionID int, side string) (int, error) {
	if side == "" {
		side = models.SideBoth
	}
	var id int
	err := s.db.QueryRow(`INSERT INTO build_modversions (build_id, modversion_id, side)
		VALUES ($1,$2,$3)
		ON CONFLICT (build_id, modversion_id) DO UPDATE SET side=EXCLUDED.side
		RETURNING id`, buildID, modversionID, side).Scan(&id)
	return id, err
}

func (s *PostgresStore) DetachFromBuild(buildID, modversionID int) error {
	_, err := s.db.Exec(`DELETE FROM build_modversions WHERE build_id=$1 AND modversion_id=$2`, buildID, modversionID)
	return err
}

// IsModversionInBuild reports whether the modversion is attached to the build.
// Guards mutation handlers against a caller passing a modversion id that belongs
// to someone else's build (loadOwnedBuild only proves pack/build ownership).
func (s *PostgresStore) IsModversionInBuild(buildID, modversionID int) (bool, error) {
	var exists bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM build_modversions WHERE build_id=$1 AND modversion_id=$2)`,
		buildID, modversionID).Scan(&exists)
	return exists, err
}

func (s *PostgresStore) ListBuildContent(buildID int) ([]models.BuildContentEntry, error) {
	rows, err := s.db.Query(`SELECT `+prefixCols("mv", mvCols)+`,
		bmv.side, m.slug, m.pretty_name, m.content_type
		FROM build_modversions bmv
		JOIN modversions mv ON mv.id = bmv.modversion_id
		JOIN mods m ON m.id = mv.mod_id
		WHERE bmv.build_id=$1
		ORDER BY m.slug ASC`, buildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.BuildContentEntry{}
	for rows.Next() {
		var e models.BuildContentEntry
		v := &e.Modversion
		if err := rows.Scan(&v.ID, &v.ModID, &v.Version, &v.Filesize, &v.StorageKey, &v.MD5, &v.SHA1, &v.SHA512,
			&v.Source, &v.TargetPath, &v.ModrinthProjectID, &v.ModrinthVersionID, &v.ModrinthVersionNumber,
			&v.ModrinthGameVersions, &v.ModrinthLatestVersionID, &v.ModrinthLastChecked, &v.CreatedAt, &v.UpdatedAt,
			&v.ModrinthDownloadURL,
			&e.Side, &e.ModSlug, &e.PrettyName, &e.ContentType); err != nil {
			return nil, err
		}
		e.Linked = v.ModrinthProjectID != ""
		out = append(out, e)
	}
	return out, rows.Err()
}

// ModversionCheckRow is a linked modversion plus the build context (loader + mc)
// the Modrinth update endpoint requires as filters.
type ModversionCheckRow struct {
	Modversion models.Modversion
	Loader     string
	Minecraft  string
}

// ListModversionsDueForCheck returns Modrinth-linked modversions (with a sha1)
// whose build has a concrete loader + minecraft and which have not been checked
// since `before`. A modversion attached to several builds yields one row per
// build so each (loader, mc) context is checked; the shared row's cached result
// reflects the last context processed (acceptable — the signal is a hint).
func (s *PostgresStore) ListModversionsDueForCheck(before time.Time) ([]ModversionCheckRow, error) {
	rows, err := s.db.Query(`SELECT `+prefixCols("mv", mvCols)+`, b.loader, b.minecraft
		FROM modversions mv
		JOIN build_modversions bmv ON bmv.modversion_id = mv.id
		JOIN pack_builds b ON b.id = bmv.build_id
		WHERE mv.modrinth_project_id <> ''
		  AND mv.sha1 <> ''
		  AND b.loader <> '' AND b.minecraft <> ''
		  AND (mv.modrinth_last_checked IS NULL OR mv.modrinth_last_checked < $1)`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModversionCheckRow{}
	for rows.Next() {
		var r ModversionCheckRow
		v := &r.Modversion
		if err := rows.Scan(&v.ID, &v.ModID, &v.Version, &v.Filesize, &v.StorageKey, &v.MD5, &v.SHA1, &v.SHA512,
			&v.Source, &v.TargetPath, &v.ModrinthProjectID, &v.ModrinthVersionID, &v.ModrinthVersionNumber,
			&v.ModrinthGameVersions, &v.ModrinthLatestVersionID, &v.ModrinthLastChecked, &v.CreatedAt, &v.UpdatedAt,
			&v.ModrinthDownloadURL,
			&r.Loader, &r.Minecraft); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetModversionCheckResult stamps only the auto-update cache columns. A targeted
// UPDATE (not the full-row UpdateModversion) so a concurrent user edit to other
// fields is not clobbered by a stale cron read.
func (s *PostgresStore) SetModversionCheckResult(id int, latestVersionID string, checkedAt time.Time) error {
	_, err := s.db.Exec(`UPDATE modversions
		SET modrinth_latest_version_id=$2, modrinth_last_checked=$3, updated_at=NOW()
		WHERE id=$1`, id, latestVersionID, checkedAt)
	return err
}
