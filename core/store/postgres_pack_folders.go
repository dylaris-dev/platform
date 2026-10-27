package store

import (
	"context"
	"fmt"
)

func (s *PostgresStore) ListPackFolders(ctx context.Context, id int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM pack_build_folders WHERE build_id=$1 ORDER BY path`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		out = append(out, path)
	}
	return out, rows.Err()
}
func (s *PostgresStore) SetPackFolder(ctx context.Context, id int, path string, remove bool) error {
	if remove {
		_, err := s.db.ExecContext(ctx, `DELETE FROM pack_build_folders WHERE build_id=$1 AND path=$2`, id, path)
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT id FROM pack_builds WHERE id=$1 FOR UPDATE`, id); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM pack_build_folders WHERE build_id=$1`, id).Scan(&count); err != nil {
		return err
	}
	if count >= 1000 {
		return fmt.Errorf("folder limit reached")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO pack_build_folders(build_id,path)VALUES($1,$2)ON CONFLICT DO NOTHING`, id, path); err != nil {
		return err
	}
	return tx.Commit()
}
