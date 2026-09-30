package store

import (
	"database/sql"
	"dylaris-core/models"
	"errors"
	"time"
)

// Scheduled-tasks queries kept in a dedicated file so changes don't
// churn the main postgres.go. All methods return sql.ErrNoRows-equivalent
// errors via errors.New("not found") so callers can compare without importing
// database/sql.

var errScheduledTaskNotFound = errors.New("scheduled task not found")

func scanScheduledTask(row interface {
	Scan(dest ...interface{}) error
}) (*models.ScheduledTask, error) {
	var t models.ScheduledTask
	var nextRun, lastRun sql.NullTime
	var createdBy sql.NullString
	err := row.Scan(
		&t.ID, &t.ServerID, &t.Name, &t.TaskType, &t.ScheduleCron, &t.Payload,
		&t.Enabled, &nextRun, &lastRun, &t.LastStatus, &t.LastError,
		&createdBy, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if nextRun.Valid {
		v := nextRun.Time
		t.NextRun = &v
	}
	if lastRun.Valid {
		v := lastRun.Time
		t.LastRun = &v
	}
	if createdBy.Valid {
		v := createdBy.String
		t.CreatedBy = &v
	}
	return &t, nil
}

const scheduledTaskCols = `id, server_id, name, task_type, schedule_cron, payload,
		enabled, next_run, last_run, last_status, last_error,
		created_by, created_at, updated_at`

func (s *PostgresStore) ListScheduledTasksByServer(serverID int) ([]models.ScheduledTask, error) {
	rows, err := s.db.Query(`SELECT `+scheduledTaskCols+`
		FROM scheduled_tasks WHERE server_id=$1 ORDER BY created_at ASC`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.ScheduledTask{}
	for rows.Next() {
		t, err := scanScheduledTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *PostgresStore) GetScheduledTask(id int) (*models.ScheduledTask, error) {
	row := s.db.QueryRow(`SELECT `+scheduledTaskCols+` FROM scheduled_tasks WHERE id=$1`, id)
	t, err := scanScheduledTask(row)
	if err == sql.ErrNoRows {
		return nil, errScheduledTaskNotFound
	}
	return t, err
}

func (s *PostgresStore) CreateScheduledTask(t *models.ScheduledTask) (int, error) {
	var createdBy interface{}
	if t.CreatedBy != nil {
		createdBy = *t.CreatedBy
	}
	var nextRun interface{}
	if t.NextRun != nil {
		nextRun = *t.NextRun
	}
	var id int
	err := s.db.QueryRow(`INSERT INTO scheduled_tasks
		(server_id, name, task_type, schedule_cron, payload, enabled, next_run, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		t.ServerID, t.Name, t.TaskType, t.ScheduleCron, t.Payload, t.Enabled, nextRun, createdBy,
	).Scan(&id)
	return id, err
}

func (s *PostgresStore) UpdateScheduledTask(t *models.ScheduledTask) error {
	var nextRun interface{}
	if t.NextRun != nil {
		nextRun = *t.NextRun
	}
	res, err := s.db.Exec(`UPDATE scheduled_tasks SET
		name=$2, task_type=$3, schedule_cron=$4, payload=$5, enabled=$6, next_run=$7,
		updated_at=NOW()
		WHERE id=$1`,
		t.ID, t.Name, t.TaskType, t.ScheduleCron, t.Payload, t.Enabled, nextRun,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errScheduledTaskNotFound
	}
	return nil
}

func (s *PostgresStore) DeleteScheduledTask(id int) error {
	res, err := s.db.Exec(`DELETE FROM scheduled_tasks WHERE id=$1`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errScheduledTaskNotFound
	}
	return nil
}

func (s *PostgresStore) SetScheduledTaskEnabled(id int, enabled bool, nextRun *time.Time) error {
	var nr interface{}
	if nextRun != nil {
		nr = *nextRun
	}
	res, err := s.db.Exec(`UPDATE scheduled_tasks SET enabled=$2, next_run=$3, updated_at=NOW() WHERE id=$1`,
		id, enabled, nr)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errScheduledTaskNotFound
	}
	return nil
}

// ListDueScheduledTasks is the executor's hot path. Indexed by
// idx_scheduled_tasks_due (partial, only enabled rows with non-null next_run)
// so the scan is essentially O(due-set-size). The limit guard is a safety
// belt against a backlog of stale tasks taking down the executor on first tick.
func (s *PostgresStore) ListDueScheduledTasks(now time.Time, limit int) ([]models.ScheduledTask, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT `+scheduledTaskCols+`
		FROM scheduled_tasks
		WHERE enabled = TRUE AND next_run IS NOT NULL AND next_run <= $1
		ORDER BY next_run ASC
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.ScheduledTask{}
	for rows.Next() {
		t, err := scanScheduledTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// ClaimScheduledTaskRun moves next_run on from dueAt to next, but only while the
// row still holds dueAt and is enabled. It reports whether THIS call made the
// move, which is what decides which Core replica fires a due task.
func (s *PostgresStore) ClaimScheduledTaskRun(id int, dueAt, next time.Time) (bool, error) {
	res, err := s.db.Exec(`UPDATE scheduled_tasks SET next_run=$3, updated_at=NOW()
		WHERE id=$1 AND enabled = TRUE AND next_run = $2`, id, dueAt, next)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (s *PostgresStore) RecordScheduledTaskRun(id int, ranAt time.Time, status, errMsg string, nextRun *time.Time) error {
	var nr interface{}
	if nextRun != nil {
		nr = *nextRun
	}
	_, err := s.db.Exec(`UPDATE scheduled_tasks SET
		last_run=$2, last_status=$3, last_error=$4, next_run=$5, updated_at=NOW()
		WHERE id=$1`, id, ranAt, status, errMsg, nr)
	return err
}
