package store

import (
	"context"
	"database/sql"
	"fmt"

	"dylaris-core/models"
)

// ClonePackBuild snapshots content into independent modversion rows. Objects
// remain shared and immutable; editing a config writes a new key, so editing or
// removing content from the draft cannot rewrite its source build.
func (s *PostgresStore) ClonePackBuild(ctx context.Context, b *models.PackBuild, sourceID int) (int, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return 0, fmt.Errorf("begin build copy: %w", err)
	}
	defer tx.Rollback()
	var source int
	if err := tx.QueryRowContext(ctx, `SELECT id FROM pack_builds
		WHERE id=$1 AND pack_id=$2 AND minecraft=$3 AND loader=$4 FOR SHARE`, sourceID, b.PackID, b.Minecraft, b.Loader).Scan(&source); err != nil {
		return 0, fmt.Errorf("validate build copy source: %w", err)
	}
	var id int
	if err := tx.QueryRowContext(ctx, `INSERT INTO pack_builds
		(pack_id,version_string,minecraft,loader,loader_version,min_java,min_memory,changelog,channel)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'draft') RETURNING id`, b.PackID, b.VersionString, b.Minecraft, b.Loader, b.LoaderVersion, b.MinJava, b.MinMemory, b.Changelog).Scan(&id); err != nil {
		return 0, fmt.Errorf("create copied build: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT modversion_id,side FROM build_modversions WHERE build_id=$1`, source)
	if err != nil {
		return 0, fmt.Errorf("read build copy content: %w", err)
	}
	type item struct {
		id   int
		side string
	}
	var items []item
	for rows.Next() {
		var entry item
		if err := rows.Scan(&entry.id, &entry.side); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan build copy content: %w", err)
		}
		items = append(items, entry)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, fmt.Errorf("iterate build copy content: %w", err)
	}
	for _, entry := range items {
		var versionID int
		if err := tx.QueryRowContext(ctx, `INSERT INTO modversions
			(mod_id,version,filesize,storage_key,md5,sha1,sha512,source,target_path,modrinth_project_id,
			modrinth_version_id,modrinth_version_number,modrinth_game_versions,modrinth_latest_version_id,
			modrinth_last_checked,modrinth_download_url)
			SELECT mod_id,version,filesize,storage_key,md5,sha1,sha512,source,target_path,modrinth_project_id,
			modrinth_version_id,modrinth_version_number,modrinth_game_versions,modrinth_latest_version_id,
			modrinth_last_checked,modrinth_download_url FROM modversions WHERE id=$1 RETURNING id`, entry.id).Scan(&versionID); err != nil {
			return 0, fmt.Errorf("copy build content version: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO build_modversions(build_id,modversion_id,side) VALUES ($1,$2,$3)`, id, versionID, entry.side); err != nil {
			return 0, fmt.Errorf("attach copied build content: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pack_build_folders(build_id,path)SELECT $1,path FROM pack_build_folders WHERE build_id=$2`, id, source); err != nil {
		return 0, fmt.Errorf("copy build folders: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit build copy: %w", err)
	}
	return id, nil
}
