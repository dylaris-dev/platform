package store

import (
	"context"
	"fmt"
)

func (s *PostgresStore) SetPackRecommended(ctx context.Context, packID, buildID int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int
	if err = tx.QueryRowContext(ctx, `SELECT id FROM packs WHERE id=$1 FOR UPDATE`, packID).Scan(&id); err != nil {
		return err
	}
	var selected any
	if buildID > 0 {
		if err = tx.QueryRowContext(ctx, `SELECT id FROM pack_builds WHERE id=$1 AND pack_id=$2 FOR SHARE`, buildID, packID).Scan(&id); err != nil {
			return fmt.Errorf("recommended build does not belong to pack: %w", err)
		}
		selected = buildID
	}
	if _, err = tx.ExecContext(ctx, `UPDATE packs SET recommended_build_id=$2,updated_at=NOW() WHERE id=$1`, packID, selected); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE modrinth_watches SET next_check=LEAST(next_check,NOW()) WHERE pack_id=$1`, packID); err != nil {
		return err
	}
	return tx.Commit()
}
