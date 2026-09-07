package store

import (
	"database/sql"
	"dylaris-core/models"
	"encoding/json"
	"time"

	"github.com/lib/pq"
)

// ───────────── Storages ─────────────

// backupStorageCols is the read shape. owner_id is last so the existing scans
// stay readable; every read goes through scanBackupStorage.
const backupStorageCols = `id, name, provider, config, secret_enc, is_default, created_at, owner_id`

func (s *PostgresStore) scanBackupStorage(row interface{ Scan(...interface{}) error }) (*models.BackupStorage, string, error) {
	var bs models.BackupStorage
	var cfg []byte
	var secretEnc string
	var owner sql.NullString
	if err := row.Scan(&bs.ID, &bs.Name, &bs.Provider, &cfg, &secretEnc, &bs.IsDefault, &bs.CreatedAt, &owner); err != nil {
		return nil, "", err
	}
	bs.Config = json.RawMessage(cfg)
	if owner.Valid {
		v := owner.String
		bs.OwnerID = &v
	}
	return &bs, secretEnc, nil
}

// ListBackupStorages returns the PLATFORM's own storages, which is every row
// that existed before tenants could bring their own.
//
// Deliberately not "all rows": this feeds the admin screen and the storage
// dropdown, and a tenant's private bucket must not be offerable as a target for
// somebody else's backups.
func (s *PostgresStore) ListBackupStorages() ([]models.BackupStorage, error) {
	return s.listBackupStorages(`owner_id IS NULL`)
}

// ListBackupStoragesByOwner returns one tenant's own storages, and only theirs.
func (s *PostgresStore) ListBackupStoragesByOwner(ownerID string) ([]models.BackupStorage, error) {
	return s.listBackupStorages(`owner_id = $1`, ownerID)
}

func (s *PostgresStore) listBackupStorages(where string, args ...interface{}) ([]models.BackupStorage, error) {
	rows, err := s.db.Query(`SELECT `+backupStorageCols+` FROM backup_storages WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.BackupStorage
	for rows.Next() {
		bs, secretEnc, err := s.scanBackupStorage(rows)
		if err != nil {
			continue
		}
		// List is an enumeration path: strip the secret from config so it never
		// leaves Core, even for a legacy row still carrying it in plaintext.
		cleaned, had := stripBackupStorageSecret(bs.Provider, bs.Config)
		bs.Config = cleaned
		bs.SecretSet = secretEnc != "" || had
		out = append(out, *bs)
	}
	return out, rows.Err()
}

func (s *PostgresStore) GetBackupStorage(id int) (*models.BackupStorage, error) {
	bs, secretEnc, err := s.scanBackupStorage(
		s.db.QueryRow(`SELECT `+backupStorageCols+` FROM backup_storages WHERE id = $1`, id))
	if err != nil {
		return nil, err
	}
	s.hydrateBackupStorageForBuild(bs, secretEnc)
	return bs, nil
}

// GetDefaultBackupStorage is the PLATFORM default: the last step of the chain,
// used for anyone who has not set one of their own.
func (s *PostgresStore) GetDefaultBackupStorage() (*models.BackupStorage, error) {
	bs, secretEnc, err := s.scanBackupStorage(s.db.QueryRow(
		`SELECT ` + backupStorageCols + ` FROM backup_storages WHERE is_default = TRUE AND owner_id IS NULL LIMIT 1`))
	if err == sql.ErrNoRows {
		// Fall back to the first configured PLATFORM storage when no default is
		// set. Scoped, or an install with no platform default would start
		// writing everybody's backups into whichever tenant happened to hold
		// the lowest-id bucket.
		bs, secretEnc, err = s.scanBackupStorage(s.db.QueryRow(
			`SELECT ` + backupStorageCols + ` FROM backup_storages WHERE owner_id IS NULL ORDER BY id LIMIT 1`))
		if err == sql.ErrNoRows {
			return nil, nil
		}
	}
	if err != nil {
		return nil, err
	}
	s.hydrateBackupStorageForBuild(bs, secretEnc)
	return bs, nil
}

// GetUserDefaultBackupStorage is a tenant's own default, or nil when they have
// not connected one. nil is not an error: it means "ask the next scope".
func (s *PostgresStore) GetUserDefaultBackupStorage(ownerID string) (*models.BackupStorage, error) {
	if ownerID == "" {
		return nil, nil
	}
	bs, secretEnc, err := s.scanBackupStorage(s.db.QueryRow(
		`SELECT `+backupStorageCols+` FROM backup_storages WHERE owner_id = $1 AND is_default = TRUE LIMIT 1`, ownerID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.hydrateBackupStorageForBuild(bs, secretEnc)
	return bs, nil
}

// hydrateBackupStorageForBuild prepares a storage read for a provider build:
// it decrypts secret_enc (falling back to a legacy plaintext secret still in
// config), re-injects the secret into Config where factory.Open and the node
// transport expect it, and sets SecretSet. Called only by the id/default reads,
// never by the list path.
func (s *PostgresStore) hydrateBackupStorageForBuild(bs *models.BackupStorage, secretEnc string) {
	secret := s.decodeBackupStorageSecret(secretEnc)
	if secret == "" {
		// Legacy row: the secret may still sit plaintext inside config.
		_, secret = splitBackupStorageSecret(bs.Provider, bs.Config)
	}
	bs.Config = injectBackupStorageSecret(bs.Provider, bs.Config, secret)
	bs.SecretSet = secret != ""
}

// CreateBackupStorage inserts a storage. When isDefault, it clears the previous
// default in the SAME transaction as the insert: the clear and the row that is
// supposed to replace it have to stand or fall together, or a rejected insert
// (a duplicate name is the reachable one) leaves the platform with no default
// at all - and GetDefaultBackupStorage then silently falls back to the
// lowest-id storage, so scheduled backups start writing somewhere else while
// the request that caused it reported failure. Same shape as CreatePlan.
func (s *PostgresStore) CreateBackupStorage(bs *models.BackupStorage) (int, error) {
	cleanCfg, secret := splitBackupStorageSecret(bs.Provider, bs.Config)
	cfg := []byte(cleanCfg)
	if len(cfg) == 0 {
		cfg = []byte("{}")
	}
	// Encrypt BEFORE any DB write so a fail-closed (no key) leaves no side
	// effect - never clears another default without persisting this row.
	enc, err := s.encodeBackupStorageSecret(secret)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit
	if bs.IsDefault {
		if err = clearOtherDefaults(tx, bs.OwnerID, 0); err != nil {
			return 0, err
		}
	}
	var id int
	err = tx.QueryRow(
		`INSERT INTO backup_storages (name, provider, config, secret_enc, is_default, owner_id) VALUES ($1, $2, $3::jsonb, $4, $5, $6) RETURNING id`,
		bs.Name, bs.Provider, cfg, enc, bs.IsDefault, nullableString(bs.OwnerID),
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrNameTaken
		}
		return 0, err
	}
	return id, tx.Commit()
}

// UpdateBackupStorage updates a storage in place, honoring the single-default
// invariant. Transactional for the same reason as CreateBackupStorage: a rename
// onto an existing name must not survive as a cleared default. Returns
// sql.ErrNoRows when the id does not exist, so the handler can answer 404
// instead of reporting a write that never happened.
func (s *PostgresStore) UpdateBackupStorage(bs *models.BackupStorage) error {
	cleanCfg, secret := splitBackupStorageSecret(bs.Provider, bs.Config)
	cfg := []byte(cleanCfg)
	if len(cfg) == 0 {
		cfg = []byte("{}")
	}
	enc, err := s.encodeBackupStorageSecret(secret)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit
	if bs.IsDefault {
		if err = clearOtherDefaults(tx, bs.OwnerID, bs.ID); err != nil {
			return err
		}
	}
	// owner_id is NOT in the SET list: ownership is decided once, at creation.
	// Moving a storage between scopes would either hand a tenant's credentials
	// to the platform or the platform's to a tenant, and neither is a thing an
	// edit form should be able to do.
	res, err := tx.Exec(
		`UPDATE backup_storages SET name = $1, provider = $2, config = $3::jsonb, secret_enc = $4, is_default = $5 WHERE id = $6`,
		bs.Name, bs.Provider, cfg, enc, bs.IsDefault, bs.ID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrNameTaken
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// clearOtherDefaults clears the default flag WITHIN one scope: the platform's
// rows when ownerID is nil, one tenant's rows otherwise. excludeID is the row
// about to become the default, and 0 on an insert where there is no row yet.
//
// The scoping is the point. Without it a tenant marking their own bucket as
// their default would clear the PLATFORM default, and every other tenant would
// silently fall through to whichever storage happened to have the lowest id.
func clearOtherDefaults(tx *sql.Tx, ownerID *string, excludeID int) error {
	if ownerID == nil {
		_, err := tx.Exec(
			`UPDATE backup_storages SET is_default = FALSE WHERE is_default = TRUE AND owner_id IS NULL AND id != $1`, excludeID)
		return err
	}
	_, err := tx.Exec(
		`UPDATE backup_storages SET is_default = FALSE WHERE is_default = TRUE AND owner_id = $1 AND id != $2`, *ownerID, excludeID)
	return err
}

func (s *PostgresStore) DeleteBackupStorage(id int) error {
	_, err := s.db.Exec(`DELETE FROM backup_storages WHERE id = $1`, id)
	return err
}

// ───────────── Jobs ─────────────

func (s *PostgresStore) scanJob(row interface{ Scan(...interface{}) error }) (*models.BackupJob, error) {
	var j models.BackupJob
	var subServer sql.NullString
	var storageID sql.NullInt64
	var lastRun, nextRun sql.NullTime
	var includes, excludes pq.StringArray
	err := row.Scan(
		&j.ID, &j.ServerID, &subServer, &j.Name, &j.Schedule,
		&includes, &excludes, &j.RetentionCount,
		&storageID, &j.Enabled, &lastRun, &nextRun, &j.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	if subServer.Valid {
		s := subServer.String
		j.SubServer = &s
	}
	if storageID.Valid {
		id := int(storageID.Int64)
		j.StorageID = &id
	}
	if lastRun.Valid {
		t := lastRun.Time
		j.LastRunAt = &t
	}
	if nextRun.Valid {
		t := nextRun.Time
		j.NextRunAt = &t
	}
	j.IncludePatterns = []string(includes)
	j.ExcludePatterns = []string(excludes)
	return &j, nil
}

const backupJobCols = `id, server_id, sub_server, name, schedule, include_patterns, exclude_patterns, retention_count, storage_id, enabled, last_run_at, next_run_at, created_at`

func (s *PostgresStore) ListBackupJobs(serverID int) ([]models.BackupJob, error) {
	rows, err := s.db.Query(`SELECT `+backupJobCols+` FROM backup_jobs WHERE server_id = $1 ORDER BY id`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.BackupJob
	for rows.Next() {
		j, err := s.scanJob(rows)
		if err != nil {
			continue
		}
		out = append(out, *j)
	}
	return out, nil
}

func (s *PostgresStore) GetBackupJob(id int) (*models.BackupJob, error) {
	row := s.db.QueryRow(`SELECT `+backupJobCols+` FROM backup_jobs WHERE id = $1`, id)
	return s.scanJob(row)
}

func (s *PostgresStore) CreateBackupJob(j *models.BackupJob) (int, error) {
	var id int
	err := s.db.QueryRow(
		`INSERT INTO backup_jobs (server_id, sub_server, name, schedule, include_patterns, exclude_patterns, retention_count, storage_id, enabled, next_run_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		j.ServerID, nullableString(j.SubServer), j.Name, j.Schedule,
		textArray(j.IncludePatterns), textArray(j.ExcludePatterns),
		j.RetentionCount, nullableInt(j.StorageID), j.Enabled, nullableTime(j.NextRunAt),
	).Scan(&id)
	return id, err
}

func (s *PostgresStore) UpdateBackupJob(j *models.BackupJob) error {
	_, err := s.db.Exec(
		`UPDATE backup_jobs SET sub_server = $1, name = $2, schedule = $3, include_patterns = $4,
		 exclude_patterns = $5, retention_count = $6, storage_id = $7, enabled = $8, next_run_at = $9 WHERE id = $10`,
		nullableString(j.SubServer), j.Name, j.Schedule,
		textArray(j.IncludePatterns), textArray(j.ExcludePatterns),
		j.RetentionCount, nullableInt(j.StorageID), j.Enabled, nullableTime(j.NextRunAt), j.ID,
	)
	return err
}

func (s *PostgresStore) DeleteBackupJob(id int) error {
	_, err := s.db.Exec(`DELETE FROM backup_jobs WHERE id = $1`, id)
	return err
}

func (s *PostgresStore) ListDueBackupJobs(now time.Time) ([]models.BackupJob, error) {
	rows, err := s.db.Query(
		`SELECT `+backupJobCols+` FROM backup_jobs WHERE enabled = TRUE AND next_run_at IS NOT NULL AND next_run_at <= $1`,
		now,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.BackupJob
	for rows.Next() {
		j, err := s.scanJob(rows)
		if err != nil {
			continue
		}
		out = append(out, *j)
	}
	// The scheduler runs exactly the jobs in this list and says nothing about
	// the ones it never saw, so a short read means backups that quietly do not
	// happen while the schedule still looks healthy in the panel.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PostgresStore) SetBackupJobScheduled(jobID int, lastRun, nextRun time.Time) error {
	_, err := s.db.Exec(`UPDATE backup_jobs SET last_run_at = $1, next_run_at = $2 WHERE id = $3`, lastRun, nextRun, jobID)
	return err
}

// ───────────── Runs ─────────────

func (s *PostgresStore) scanRun(row interface{ Scan(...interface{}) error }) (*models.BackupRun, error) {
	var r models.BackupRun
	var completed sql.NullTime
	var storageID sql.NullInt64
	err := row.Scan(&r.ID, &r.JobID, &r.StartedAt, &completed, &r.Status, &r.SizeBytes, &r.StorageKey, &r.ErrorMessage, &r.InstallSnapshot, &r.Manifest, &storageID)
	if err != nil {
		return nil, err
	}
	if completed.Valid {
		t := completed.Time
		r.CompletedAt = &t
	}
	if storageID.Valid {
		id := int(storageID.Int64)
		r.StorageID = &id
	}
	return &r, nil
}

const backupRunCols = `id, job_id, started_at, completed_at, status, size_bytes, storage_key, error_message, install_snapshot, manifest, storage_id`

func (s *PostgresStore) ListBackupRuns(jobID, limit int) ([]models.BackupRun, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+backupRunCols+` FROM backup_runs WHERE job_id = $1 ORDER BY started_at DESC LIMIT $2`, jobID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.BackupRun
	for rows.Next() {
		r, err := s.scanRun(rows)
		if err != nil {
			continue
		}
		out = append(out, *r)
	}
	return out, nil
}

func (s *PostgresStore) GetBackupRun(id int) (*models.BackupRun, error) {
	row := s.db.QueryRow(`SELECT `+backupRunCols+` FROM backup_runs WHERE id = $1`, id)
	return s.scanRun(row)
}

func (s *PostgresStore) CreateBackupRun(r *models.BackupRun) (int, error) {
	var id int
	err := s.db.QueryRow(
		`INSERT INTO backup_runs (job_id, status, storage_key, storage_id) VALUES ($1, $2, $3, $4) RETURNING id`,
		r.JobID, r.Status, r.StorageKey, nullableInt(r.StorageID),
	).Scan(&id)
	return id, err
}

func (s *PostgresStore) UpdateBackupRunStatus(id int, status, errorMsg string, sizeBytes int64, storageKey string, completed time.Time) error {
	var completedArg interface{} = completed
	if completed.IsZero() {
		completedArg = nil
	}
	_, err := s.db.Exec(
		`UPDATE backup_runs SET status = $1, error_message = $2, size_bytes = $3, storage_key = $4, completed_at = $5 WHERE id = $6`,
		status, errorMsg, sizeBytes, storageKey, completedArg, id,
	)
	return err
}

// ListAbandonedBackupRuns returns runs still marked "running" that started
// before the cutoff, oldest first.
//
// A run is created and committed as "running" BEFORE the work is dispatched to
// a node, and the only thing that ever moves it off "running" is a result
// message the node publishes over Redis Pub/Sub. Pub/Sub is fire-and-forget:
// if no Core is subscribed at that moment - every Core restarting, a rolling
// deploy - the result is gone and the row stays "running" for good. A node that
// dies mid-backup has the same effect.
//
// Nothing cleaned these up. PruneOldBackupRuns only ever deletes rows with
// status "success", so an abandoned row is never retried, never pruned and
// never counted, it simply accumulates.
//
// The limit bounds one sweep. Reaping opens a storage connection per run to
// find out whether an archive exists, and the first sweep after this ships may
// find a long backlog; a cap keeps that off a single tick.
func (s *PostgresStore) ListAbandonedBackupRuns(startedBefore time.Time, limit int) ([]models.BackupRun, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT `+backupRunCols+` FROM backup_runs
		 WHERE status = 'running' AND started_at < $1
		 ORDER BY started_at ASC
		 LIMIT $2`,
		startedBefore, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.BackupRun
	for rows.Next() {
		r, err := s.scanRun(rows)
		if err != nil {
			continue
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *PostgresStore) DeleteBackupRun(id int) error {
	_, err := s.db.Exec(`DELETE FROM backup_runs WHERE id = $1`, id)
	return err
}

func (s *PostgresStore) PruneOldBackupRuns(jobID, keep int) ([]models.BackupRun, error) {
	if keep < 0 {
		keep = 0
	}
	rows, err := s.db.Query(
		`SELECT `+backupRunCols+` FROM backup_runs
		 WHERE job_id = $1 AND status = 'success'
		 ORDER BY started_at DESC
		 OFFSET $2`,
		jobID, keep,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var toPrune []models.BackupRun
	for rows.Next() {
		r, err := s.scanRun(rows)
		if err != nil {
			continue
		}
		toPrune = append(toPrune, *r)
	}
	// Under-pruning is the harmless direction, but the caller also deletes the
	// archives behind these rows, so it should hear about a short read rather
	// than treat a partial list as the complete answer.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range toPrune {
		s.db.Exec(`DELETE FROM backup_runs WHERE id = $1`, r.ID)
	}
	return toPrune, nil
}

// ───────────── Restores ─────────────

func (s *PostgresStore) scanRestore(row interface{ Scan(...interface{}) error }) (*models.BackupRestore, error) {
	var r models.BackupRestore
	var requestedBy sql.NullString
	var completed sql.NullTime
	err := row.Scan(&r.ID, &r.RunID, &r.ServerID, &requestedBy, &r.RequestedAt, &completed, &r.Status, &r.ErrorMessage)
	if err != nil {
		return nil, err
	}
	if requestedBy.Valid {
		v := requestedBy.String
		r.RequestedBy = &v
	}
	if completed.Valid {
		t := completed.Time
		r.CompletedAt = &t
	}
	return &r, nil
}

const backupRestoreCols = `id, run_id, server_id, requested_by, requested_at, completed_at, status, error_message`

func (s *PostgresStore) CreateBackupRestore(r *models.BackupRestore) (int, error) {
	var id int
	var requestedBy interface{} = nil
	if r.RequestedBy != nil {
		requestedBy = *r.RequestedBy
	}
	err := s.db.QueryRow(
		`INSERT INTO backup_restores (run_id, server_id, requested_by, status) VALUES ($1, $2, $3, $4) RETURNING id`,
		r.RunID, r.ServerID, requestedBy, r.Status,
	).Scan(&id)
	return id, err
}

func (s *PostgresStore) GetBackupRestore(id int) (*models.BackupRestore, error) {
	row := s.db.QueryRow(`SELECT `+backupRestoreCols+` FROM backup_restores WHERE id = $1`, id)
	return s.scanRestore(row)
}

func (s *PostgresStore) ListBackupRestores(serverID, limit int) ([]models.BackupRestore, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := s.db.Query(
		`SELECT `+backupRestoreCols+` FROM backup_restores WHERE server_id = $1 ORDER BY requested_at DESC LIMIT $2`,
		serverID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.BackupRestore
	for rows.Next() {
		r, err := s.scanRestore(rows)
		if err != nil {
			continue
		}
		out = append(out, *r)
	}
	return out, nil
}

func (s *PostgresStore) UpdateBackupRestoreStatus(id int, status, errorMsg string, completed time.Time) error {
	var completedArg interface{} = completed
	if completed.IsZero() {
		completedArg = nil
	}
	_, err := s.db.Exec(
		`UPDATE backup_restores SET status = $1, error_message = $2, completed_at = $3 WHERE id = $4`,
		status, errorMsg, completedArg, id,
	)
	return err
}

// ───────────── helpers ─────────────

func nullableString(s *string) interface{} {
	if s == nil {
		return nil
	}
	return *s
}
func nullableInt(i *int) interface{} {
	if i == nil {
		return nil
	}
	return *i
}
func nullableTime(t *time.Time) interface{} {
	if t == nil {
		return nil
	}
	return *t
}

// textArray is pq.Array for a NOT NULL text[] column. A nil slice goes to the
// driver as NULL, and naming the column in the INSERT means its DEFAULT '{}'
// never applies - so the row is rejected with a not-null violation (23502).
//
// includePatterns and excludePatterns are optional in the API and nil whenever
// a request omits them, which made `POST /api/servers/{id}/backup-jobs`
// without a pattern list answer a bare 500.
func textArray(v []string) interface{} {
	if v == nil {
		return pq.Array([]string{})
	}
	return pq.Array(v)
}

// SetBackupRunInstallSnapshot records how the archived sub-servers were
// installed, so a restore can put the records back.
//
// Its own narrow writer rather than a field on UpdateBackupRunStatus: that one
// is called on the failure paths too, and a failed run has nothing to snapshot -
// widening it would mean every caller deciding what to pass for a column it does
// not care about.
func (s *PostgresStore) SetBackupRunInstallSnapshot(runID int, snapshot string) error {
	_, err := s.db.Exec(`UPDATE backup_runs SET install_snapshot = $1 WHERE id = $2`, snapshot, runID)
	return err
}

// SetBackupRunManifest records the archive's own description on the run.
//
// Written at DISPATCH, before the node has archived anything, and that is
// deliberate: these are the exact bytes handed to the node to write into the
// archive, so the two copies describe the same moment. Capturing it after the
// node reports back would describe the moment the report arrived, which is a
// different one - a modpack changed while a large world was still being
// archived would be recorded against an archive that predates it.
func (s *PostgresStore) SetBackupRunManifest(runID int, manifest string) error {
	_, err := s.db.Exec(`UPDATE backup_runs SET manifest = $1 WHERE id = $2`, manifest, runID)
	return err
}
