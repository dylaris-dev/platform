package store

import (
	"context"
	"database/sql"
	"dylaris-core/models"
	"fmt"
	"strconv"
)

func (s *PostgresStore) ForkPackBuild(ctx context.Context, f *models.PackFork) (int, int, error) {
	// READ COMMITTED sees a build inserted before the target lock was acquired.
	// The lock also conflicts with normal build inserts' foreign-key key shares.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "pack-fork:"+f.CatalogSuffix); err != nil {
		return 0, 0, err
	}
	var source int
	if err = tx.QueryRowContext(ctx, `SELECT b.id FROM pack_builds b JOIN packs p ON p.id=b.pack_id WHERE b.id=$1 AND p.id=$2 FOR SHARE OF p,b`, f.SourceBuildID, f.SourcePackID).Scan(&source); err != nil {
		return 0, 0, fmt.Errorf("fork source unavailable: %w", err)
	}
	packID := f.Target.ID
	if packID == 0 {
		err = tx.QueryRowContext(ctx, `INSERT INTO packs(owner_id,internal_name,internal_slug,summary,modrinth_visibility)VALUES($1,$2,$3,$4,'unlisted')RETURNING id`, f.Target.OwnerID, f.Target.InternalName, f.Target.InternalSlug, f.Target.Summary).Scan(&packID)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT id FROM packs WHERE id=$1 AND owner_id=$2 FOR UPDATE`, packID, f.Target.OwnerID).Scan(&packID)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("fork target unavailable or slug already exists: %w", err)
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM pack_builds WHERE pack_id=$1`, packID).Scan(&count); err != nil {
		return 0, 0, err
	}
	if count != 0 {
		return 0, 0, fmt.Errorf("fork target must have no builds")
	}
	b := f.Build
	var buildID int
	if err = tx.QueryRowContext(ctx, `INSERT INTO pack_builds(pack_id,version_string,minecraft,loader,loader_version,min_java,min_memory,changelog,channel)VALUES($1,$2,$3,$4,$5,$6,$7,$8,'draft')RETURNING id`, packID, b.VersionString, b.Minecraft, b.Loader, b.LoaderVersion, b.MinJava, b.MinMemory, b.Changelog).Scan(&buildID); err != nil {
		return 0, 0, err
	}
	catalogs := map[int]int{}
	for _, e := range f.Content {
		modID, ok := catalogs[e.ModID]
		if !ok {
			slug := "fork-" + f.CatalogSuffix + "-" + strconv.Itoa(e.ModID)
			if err = tx.QueryRowContext(ctx, `INSERT INTO mods(owner_id,slug,pretty_name,content_type)VALUES($1,$2,$3,$4)RETURNING id`, f.Target.OwnerID, slug, e.PrettyName, e.ContentType).Scan(&modID); err != nil {
				return 0, 0, err
			}
			catalogs[e.ModID] = modID
		}
		var versionID int
		if err = tx.QueryRowContext(ctx, `INSERT INTO modversions(mod_id,version,filesize,storage_key,md5,sha1,sha512,source,target_path,modrinth_project_id,modrinth_version_id,modrinth_version_number,modrinth_game_versions,modrinth_latest_version_id,modrinth_last_checked,modrinth_download_url)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)RETURNING id`, modID, e.Version, e.Filesize, e.StorageKey, e.MD5, e.SHA1, e.SHA512, e.Source, e.TargetPath, e.ModrinthProjectID, e.ModrinthVersionID, e.ModrinthVersionNumber, e.ModrinthGameVersions, e.ModrinthLatestVersionID, e.ModrinthLastChecked, e.ModrinthDownloadURL).Scan(&versionID); err != nil {
			return 0, 0, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO build_modversions(build_id,modversion_id,side)VALUES($1,$2,$3)`, buildID, versionID, e.Side); err != nil {
			return 0, 0, err
		}
	}
	for _, folder := range f.Folders {
		if _, err = tx.ExecContext(ctx, `INSERT INTO pack_build_folders(build_id,path)VALUES($1,$2)`, buildID, folder); err != nil {
			return 0, 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, err
	}
	f.Target.ID = packID
	b.ID = buildID
	b.PackID = packID
	b.Channel = models.ChannelDraft
	return packID, buildID, nil
}

// A lost COMMIT reply is not proof of a rollback. Wait for the original fork
// transaction to resolve, then retain every object referenced by committed rows.
func (s *PostgresStore) UnreferencedPackForkUploads(ctx context.Context, suffix string, keys []string) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "pack-fork:"+suffix); err != nil {
		return nil, err
	}
	unused := []string{}
	for _, key := range keys {
		var referenced bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM modversions WHERE storage_key=$1)`, key).Scan(&referenced); err != nil {
			return nil, err
		}
		if !referenced {
			unused = append(unused, key)
		}
	}
	return unused, nil
}
