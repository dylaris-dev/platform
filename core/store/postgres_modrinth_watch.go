package store

import (
	"context"
	"database/sql"
	"dylaris-core/models"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (s *PostgresStore) DeleteNotification(id int64, userID string) error {
	_, err := s.db.Exec(`DELETE FROM notifications WHERE id=$1 AND user_id=$2`, id, userID)
	return err
}
func (s *PostgresStore) ListModrinthFavorites(ctx context.Context, user string) ([]models.ModrinthFavorite, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT project_id,title FROM modrinth_favorites WHERE user_id=$1 ORDER BY lower(title)`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.ModrinthFavorite{}
	for rows.Next() {
		var f models.ModrinthFavorite
		if err := rows.Scan(&f.ProjectID, &f.Title); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
func (s *PostgresStore) SetModrinthFavorite(ctx context.Context, user string, f models.ModrinthFavorite, remove bool) error {
	if remove {
		_, err := s.db.ExecContext(ctx, `DELETE FROM modrinth_favorites WHERE user_id=$1 AND project_id=$2`, user, f.ProjectID)
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, user); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO modrinth_favorites(user_id,project_id,title) SELECT $1,$2,$3 WHERE (SELECT count(*) FROM modrinth_favorites WHERE user_id=$1)<500 ON CONFLICT(user_id,project_id) DO UPDATE SET title=excluded.title`, user, f.ProjectID, f.Title)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("favorite limit reached (500)")
	}
	return tx.Commit()
}
func (s *PostgresStore) ListModrinthWatches(ctx context.Context, user string, due bool) ([]models.ModrinthWatch, error) {
	query := `SELECT id,user_id,build_id,spec,delivered,result,last_error,last_checked,next_check FROM modrinth_watches WHERE user_id=$1 ORDER BY id DESC LIMIT 100`
	args := []interface{}{user}
	if due {
		query = `SELECT id,user_id,build_id,spec,delivered,result,last_error,last_checked,next_check FROM modrinth_watches WHERE next_check<=NOW() ORDER BY next_check LIMIT 50`
		args = nil
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.ModrinthWatch{}
	for rows.Next() {
		var w models.ModrinthWatch
		var spec, delivered, result []byte
		var build sql.NullInt64
		if err := rows.Scan(&w.ID, &w.UserID, &build, &spec, &delivered, &result, &w.LastError, &w.LastChecked, &w.NextCheck); err != nil {
			return nil, err
		}
		saved := w
		if err := json.Unmarshal(spec, &w); err != nil {
			return nil, err
		}
		w.ID = saved.ID
		w.UserID = saved.UserID
		w.LastChecked = saved.LastChecked
		w.NextCheck = saved.NextCheck
		w.LastError = saved.LastError
		if err := json.Unmarshal(delivered, &w.Delivered); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(result, &w.Versions); err != nil {
			return nil, err
		}
		if build.Valid {
			id := int(build.Int64)
			w.BuildID = &id
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
func (s *PostgresStore) CreateModrinthWatch(ctx context.Context, w *models.ModrinthWatch) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serialize the per-account limit across Core replicas.
	if _, err = tx.ExecContext(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, w.UserID); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM modrinth_watches WHERE user_id=$1`, w.UserID).Scan(&count); err != nil {
		return err
	}
	if count >= 100 {
		return fmt.Errorf("watch limit reached (100)")
	}
	copy := *w
	copy.ID = 0
	copy.LastChecked = nil
	copy.NextCheck = time.Time{}
	spec, err := json.Marshal(copy)
	if err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, `INSERT INTO modrinth_watches(user_id,build_id,spec) VALUES($1,$2,$3) RETURNING id`, w.UserID, w.BuildID, spec).Scan(&w.ID); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *PostgresStore) DeleteModrinthWatch(ctx context.Context, id int64, user string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM modrinth_watches WHERE id=$1 AND user_id=$2`, id, user)
	return err
}
func (s *PostgresStore) CompleteModrinthWatch(ctx context.Context, w models.ModrinthWatch, versions []models.ModrinthWatchVersion, failure string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT delivered FROM modrinth_watches WHERE id=$1 AND user_id=$2 FOR UPDATE`, w.ID, w.UserID).Scan(&raw); err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	delivered := []string{}
	if err = json.Unmarshal(raw, &delivered); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, v := range delivered {
		seen[v] = true
	}
	for _, v := range versions {
		if failure != "" || v.Available == 0 || len(v.Missing) > 0 || seen[v.Minecraft] {
			continue
		}
		title := w.Title + " available for Minecraft " + v.Minecraft
		if runes := []rune(title); len(runes) > 200 {
			title = string(runes[:200])
		}
		body := fmt.Sprintf("%d Modrinth mods are available on %s for Minecraft %s. %d local mods need manual review. Availability does not verify dependencies, configs or gameplay.", len(w.Projects), w.Loader, v.Minecraft, w.Unlinked)
		link := "/modpacks/guide?section=favorites"
		if w.BuildID != nil {
			link = fmt.Sprintf("/modpacks/%d/builds/%d", w.PackID, *w.BuildID)
		}
		key := fmt.Sprintf("modrinth-watch:%d:%s:%s", w.ID, w.Loader, v.Minecraft)
		if _, err = tx.ExecContext(ctx, `INSERT INTO notifications(user_id,type,title,body,link,dedupe_key) VALUES($1,'modrinth_available',$2,$3,$4,$5) ON CONFLICT DO NOTHING`, w.UserID, title, body, link, key); err != nil {
			return err
		}
		seen[v.Minecraft] = true
		delivered = append(delivered, v.Minecraft)
	}
	done := !w.Future && len(w.Targets) > 0
	for _, target := range w.Targets {
		if !seen[target] {
			done = false
		}
	}
	if done {
		if _, err = tx.ExecContext(ctx, `DELETE FROM modrinth_watches WHERE id=$1 AND user_id=$2`, w.ID, w.UserID); err != nil {
			return err
		}
	} else {
		result, err := json.Marshal(versions)
		if err != nil {
			return err
		}
		notified, err := json.Marshal(delivered)
		if err != nil {
			return err
		}
		delay := 24 * time.Hour
		if failure != "" {
			delay = time.Hour
		}
		_, err = tx.ExecContext(ctx, `UPDATE modrinth_watches SET delivered=$2,result=$3,last_error=$4,last_checked=NOW(),next_check=$5 WHERE id=$1`, w.ID, notified, result, strings.TrimSpace(failure), time.Now().Add(delay))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
