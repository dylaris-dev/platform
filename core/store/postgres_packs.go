package store

import (
	"database/sql"
	"errors"

	"dylaris-core/models"
)

var errPackNotFound = errors.New("pack not found")

const packCols = `id, owner_id, internal_name, internal_slug, summary,
	modrinth_project_id, modrinth_project_name, modrinth_visibility, created_at, updated_at`

func scanPack(row interface{ Scan(...interface{}) error }) (*models.Pack, error) {
	var p models.Pack
	if err := row.Scan(&p.ID, &p.OwnerID, &p.InternalName, &p.InternalSlug, &p.Summary,
		&p.ModrinthProjectID, &p.ModrinthProjectName, &p.ModrinthVisibility, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *PostgresStore) CreatePack(p *models.Pack) (int, error) {
	var id int
	err := s.db.QueryRow(`INSERT INTO packs
		(owner_id, internal_name, internal_slug, summary, modrinth_visibility)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		p.OwnerID, p.InternalName, p.InternalSlug, p.Summary, p.ModrinthVisibility,
	).Scan(&id)
	return id, err
}

func (s *PostgresStore) UpdatePack(p *models.Pack) error {
	// internal_slug is intentionally immutable after creation: it is the stable
	// UNIQUE (owner_id, internal_slug) handle.
	_, err := s.db.Exec(`UPDATE packs SET
		internal_name=$1, summary=$2,
		modrinth_project_id=$3, modrinth_project_name=$4, modrinth_visibility=$5, updated_at=NOW()
		WHERE id=$6`,
		p.InternalName, p.Summary,
		p.ModrinthProjectID, p.ModrinthProjectName, p.ModrinthVisibility, p.ID)
	return err
}

func (s *PostgresStore) DeletePack(id int, ownerID string) error {
	_, err := s.db.Exec(`DELETE FROM packs WHERE id=$1 AND owner_id=$2`, id, ownerID)
	return err
}

func (s *PostgresStore) GetPack(id int) (*models.Pack, error) {
	row := s.db.QueryRow(`SELECT `+packCols+` FROM packs WHERE id=$1`, id)
	p, err := scanPack(row)
	if err == sql.ErrNoRows {
		return nil, errPackNotFound
	}
	return p, err
}

func (s *PostgresStore) ListPacksByOwner(ownerID string) ([]models.Pack, error) {
	rows, err := s.db.Query(`SELECT `+packCols+` FROM packs WHERE owner_id=$1 ORDER BY updated_at DESC`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.Pack{}
	for rows.Next() {
		p, err := scanPack(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

const buildCols = `id, pack_id, version_string, minecraft, loader, loader_version,
	min_java, min_memory, changelog, channel, frozen,
	modrinth_published, modrinth_version_id, mrpack_storage_key, mrpack_sha256, created_at, published_at`

func scanBuild(row interface{ Scan(...interface{}) error }) (*models.PackBuild, error) {
	var b models.PackBuild
	var publishedAt sql.NullTime
	if err := row.Scan(&b.ID, &b.PackID, &b.VersionString, &b.Minecraft, &b.Loader, &b.LoaderVersion,
		&b.MinJava, &b.MinMemory, &b.Changelog, &b.Channel, &b.Frozen,
		&b.ModrinthPublished, &b.ModrinthVersionID, &b.MrpackStorageKey, &b.MrpackSHA256, &b.CreatedAt, &publishedAt); err != nil {
		return nil, err
	}
	if publishedAt.Valid {
		t := publishedAt.Time
		b.PublishedAt = &t
	}
	return &b, nil
}

func (s *PostgresStore) CreatePackBuild(b *models.PackBuild) (int, error) {
	ch := b.Channel
	if ch == "" {
		ch = models.ChannelDraft
	}
	var id int
	err := s.db.QueryRow(`INSERT INTO pack_builds
		(pack_id, version_string, minecraft, loader, loader_version, min_java, min_memory, changelog, channel)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		b.PackID, b.VersionString, b.Minecraft, b.Loader, b.LoaderVersion, b.MinJava, b.MinMemory, b.Changelog, ch,
	).Scan(&id)
	return id, err
}

func (s *PostgresStore) UpdatePackBuild(b *models.PackBuild) error {
	var publishedAt interface{}
	if b.PublishedAt != nil {
		publishedAt = *b.PublishedAt
	}
	_, err := s.db.Exec(`UPDATE pack_builds SET
		version_string=$1, minecraft=$2, loader=$3, loader_version=$4, min_java=$5, min_memory=$6,
		changelog=$7, channel=$8, frozen=$9,
		modrinth_published=$10, modrinth_version_id=$11, mrpack_storage_key=$12, mrpack_sha256=$13, published_at=$14
		WHERE id=$15`,
		b.VersionString, b.Minecraft, b.Loader, b.LoaderVersion, b.MinJava, b.MinMemory,
		b.Changelog, b.Channel, b.Frozen,
		b.ModrinthPublished, b.ModrinthVersionID, b.MrpackStorageKey, b.MrpackSHA256, publishedAt, b.ID)
	return err
}

func (s *PostgresStore) DeletePackBuild(id, packID int) error {
	_, err := s.db.Exec(`DELETE FROM pack_builds WHERE id=$1 AND pack_id=$2`, id, packID)
	return err
}

func (s *PostgresStore) GetPackBuild(id int) (*models.PackBuild, error) {
	row := s.db.QueryRow(`SELECT `+buildCols+` FROM pack_builds WHERE id=$1`, id)
	b, err := scanBuild(row)
	if err == sql.ErrNoRows {
		return nil, errPackNotFound
	}
	return b, err
}

func (s *PostgresStore) ListPackBuilds(packID int) ([]models.PackBuild, error) {
	rows, err := s.db.Query(`SELECT `+buildCols+` FROM pack_builds WHERE pack_id=$1 ORDER BY created_at DESC`, packID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.PackBuild{}
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}
