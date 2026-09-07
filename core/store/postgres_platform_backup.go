package store

import (
	"database/sql"
	"encoding/json"
	"time"

	"dylaris-core/models"
)

// ListBackupTargetServers returns every server with the little a selection
// needs: who owns it, and whether it sits on hardware a CUSTOMER owns.
//
// The whole fleet in one query, filtered in Go by services.SelectBackupServers,
// rather than five variants of a WHERE clause. The two interesting cases - a
// selected server that has since been deleted, and the BYON distinction - are
// both easier to get right and to test there.
//
// n.owner_id, not s.owner_id: ownership of the server says who USES it,
// ownership of the node says whose machine it runs on, and only the second one
// makes a server BYON.
func (s *PostgresStore) ListBackupTargetServers() ([]models.BackupTargetServer, error) {
	rows, err := s.db.Query(`
		SELECT s.id, s.uuid, s.name, s.owner_id, s.node_id, (n.owner_id IS NOT NULL) AS byon
		FROM servers s
		JOIN nodes n ON s.node_id = n.id
		ORDER BY s.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []models.BackupTargetServer{}
	for rows.Next() {
		var t models.BackupTargetServer
		if err := rows.Scan(&t.ID, &t.UUID, &t.Name, &t.OwnerID, &t.NodeID, &t.BYON); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

const platformBackupJobCols = `id, name, schedule, selection, storage_id, retention_count, enabled, last_run_at, next_run_at, created_at`

func scanPlatformBackupJob(sc interface{ Scan(...any) error }) (*models.PlatformBackupJob, error) {
	var j models.PlatformBackupJob
	var selection []byte
	var storageID sql.NullInt64
	var lastRun, nextRun sql.NullTime
	if err := sc.Scan(&j.ID, &j.Name, &j.Schedule, &selection, &storageID,
		&j.RetentionCount, &j.Enabled, &lastRun, &nextRun, &j.CreatedAt); err != nil {
		return nil, err
	}
	// A selection that cannot be parsed is left at its zero value, which selects
	// NOTHING. The other direction - falling back to "everything" - would make
	// an unreadable configuration archive the entire platform.
	_ = json.Unmarshal(selection, &j.Selection)
	if storageID.Valid {
		v := int(storageID.Int64)
		j.StorageID = &v
	}
	if lastRun.Valid {
		t := lastRun.Time
		j.LastRunAt = &t
	}
	if nextRun.Valid {
		t := nextRun.Time
		j.NextRunAt = &t
	}
	return &j, nil
}

func (s *PostgresStore) CreatePlatformBackupJob(j *models.PlatformBackupJob) (int, error) {
	sel, err := json.Marshal(j.Selection)
	if err != nil {
		return 0, err
	}
	var id int
	err = s.db.QueryRow(`
		INSERT INTO platform_backup_jobs (name, schedule, selection, storage_id, retention_count, enabled)
		VALUES ($1, $2, $3::jsonb, $4, $5, $6) RETURNING id`,
		j.Name, j.Schedule, sel, nullableInt(j.StorageID), j.RetentionCount, j.Enabled).Scan(&id)
	return id, err
}

func (s *PostgresStore) GetPlatformBackupJob(id int) (*models.PlatformBackupJob, error) {
	return scanPlatformBackupJob(s.db.QueryRow(
		`SELECT `+platformBackupJobCols+` FROM platform_backup_jobs WHERE id = $1`, id))
}

func (s *PostgresStore) ListPlatformBackupJobs() ([]models.PlatformBackupJob, error) {
	rows, err := s.db.Query(`SELECT ` + platformBackupJobCols + ` FROM platform_backup_jobs ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []models.PlatformBackupJob{}
	for rows.Next() {
		j, err := scanPlatformBackupJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// ListDuePlatformBackupJobs returns the enabled jobs whose next run has come.
func (s *PostgresStore) ListDuePlatformBackupJobs() ([]models.PlatformBackupJob, error) {
	rows, err := s.db.Query(`SELECT ` + platformBackupJobCols + `
		FROM platform_backup_jobs
		WHERE enabled = TRUE AND next_run_at IS NOT NULL AND next_run_at <= NOW()
		ORDER BY next_run_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []models.PlatformBackupJob{}
	for rows.Next() {
		j, err := scanPlatformBackupJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

func (s *PostgresStore) UpdatePlatformBackupJob(j *models.PlatformBackupJob) error {
	sel, err := json.Marshal(j.Selection)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`
		UPDATE platform_backup_jobs
		SET name=$1, schedule=$2, selection=$3::jsonb, storage_id=$4, retention_count=$5, enabled=$6
		WHERE id=$7`,
		j.Name, j.Schedule, sel, nullableInt(j.StorageID), j.RetentionCount, j.Enabled, j.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *PostgresStore) DeletePlatformBackupJob(id int) error {
	_, err := s.db.Exec(`DELETE FROM platform_backup_jobs WHERE id = $1`, id)
	return err
}

// SetPlatformBackupJobSchedule records that a job ran and when it is next due.
func (s *PostgresStore) SetPlatformBackupJobSchedule(id int, next *time.Time) error {
	// A nil next is NULL, which is what a manual job is: it ran, and nothing
	// schedules it again.
	var v any
	if next != nil {
		v = *next
	}
	_, err := s.db.Exec(
		`UPDATE platform_backup_jobs SET last_run_at = NOW(), next_run_at = $1 WHERE id = $2`,
		v, id)
	return err
}

const platformBackupRunCols = `id, job_id, started_at, completed_at, status, size_bytes, storage_key, storage_id, error_message, components`

func scanPlatformBackupRun(sc interface{ Scan(...any) error }) (*models.PlatformBackupRun, error) {
	var r models.PlatformBackupRun
	var completed sql.NullTime
	var storageID sql.NullInt64
	var components []byte
	if err := sc.Scan(&r.ID, &r.JobID, &r.StartedAt, &completed, &r.Status,
		&r.SizeBytes, &r.StorageKey, &storageID, &r.ErrorMessage, &components); err != nil {
		return nil, err
	}
	if completed.Valid {
		t := completed.Time
		r.CompletedAt = &t
	}
	if storageID.Valid {
		v := int(storageID.Int64)
		r.StorageID = &v
	}
	r.Components = models.DecodePlatformBackupComponents(string(components))
	return &r, nil
}

// CreatePlatformBackupRun opens a run. The storage is recorded at the START,
// because it is where the archive is about to go, and a storage that is deleted
// later must not turn a finished run into one that went nowhere.
func (s *PostgresStore) CreatePlatformBackupRun(jobID int, storageID *int) (int, error) {
	var id int
	err := s.db.QueryRow(
		`INSERT INTO platform_backup_runs (job_id, storage_id) VALUES ($1, $2) RETURNING id`,
		jobID, nullableInt(storageID)).Scan(&id)
	return id, err
}

// FinishPlatformBackupRun closes a run with everything it learned.
func (s *PostgresStore) FinishPlatformBackupRun(id int, status string, sizeBytes int64,
	storageKey, errMessage string, components []models.PlatformBackupComponent) error {
	_, err := s.db.Exec(`
		UPDATE platform_backup_runs
		SET completed_at = NOW(), status = $1, size_bytes = $2, storage_key = $3,
		    error_message = $4, components = $5::jsonb
		WHERE id = $6`,
		status, sizeBytes, storageKey, errMessage,
		models.EncodePlatformBackupComponents(components), id)
	return err
}

func (s *PostgresStore) GetPlatformBackupRun(id int) (*models.PlatformBackupRun, error) {
	return scanPlatformBackupRun(s.db.QueryRow(
		`SELECT `+platformBackupRunCols+` FROM platform_backup_runs WHERE id = $1`, id))
}

func (s *PostgresStore) ListPlatformBackupRuns(jobID, limit int) ([]models.PlatformBackupRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+platformBackupRunCols+`
		FROM platform_backup_runs WHERE job_id = $1 ORDER BY started_at DESC LIMIT $2`, jobID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []models.PlatformBackupRun{}
	for rows.Next() {
		r, err := scanPlatformBackupRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ListPlatformBackupRunsOverRetention returns the SUCCESSFUL runs of a job
// beyond the newest keep, oldest first.
//
// Successful only, deliberately. A failed run's archive is partial or absent,
// so pruning by counting every row would delete a good archive to make room for
// a broken one. Same rule the server backup retention already follows.
func (s *PostgresStore) ListPlatformBackupRunsOverRetention(jobID, keep int) ([]models.PlatformBackupRun, error) {
	if keep < 0 {
		keep = 0
	}
	rows, err := s.db.Query(`SELECT `+platformBackupRunCols+`
		FROM platform_backup_runs
		WHERE job_id = $1 AND status = 'success'
		ORDER BY started_at DESC OFFSET $2`, jobID, keep)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []models.PlatformBackupRun{}
	for rows.Next() {
		r, err := scanPlatformBackupRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *PostgresStore) DeletePlatformBackupRun(id int) error {
	_, err := s.db.Exec(`DELETE FROM platform_backup_runs WHERE id = $1`, id)
	return err
}
