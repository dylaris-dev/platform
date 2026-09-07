package database

import (
	"database/sql"
	"fmt"
)

// The platform's own backups, as opposed to a game server's.
//
// A separate pair of tables rather than a flag on backup_jobs, and that is a
// decision rather than duplication. A server backup job is keyed on a server,
// runs on the NODE that holds it, and is bounded by that server owner's quota.
// A platform job is keyed on nothing, runs in Core, and covers a selection that
// may include no servers at all. Sharing the tables would mean a server_id that
// is meaningless for half the rows and a quota that applies to half of them.
func createPlatformBackupTables(db *sql.DB) error {
	tables := []string{
		// selection is the operator's choice of components, as
		// models.PlatformBackupSelection. JSONB rather than columns: the shape
		// grows a component at a time, and each one would otherwise be a
		// migration plus a scan plus a write on a table with single-digit rows.
		`CREATE TABLE IF NOT EXISTS platform_backup_jobs (
			id SERIAL PRIMARY KEY,
			name TEXT NOT NULL,
			schedule TEXT NOT NULL DEFAULT 'manual',
			selection JSONB NOT NULL DEFAULT '{}'::jsonb,
			storage_id INTEGER REFERENCES backup_storages(id) ON DELETE SET NULL,
			retention_count INTEGER NOT NULL DEFAULT 3,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			last_run_at TIMESTAMPTZ,
			next_run_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_platform_backup_jobs_next_run ON platform_backup_jobs(next_run_at) WHERE enabled = TRUE`,
		// components is what the run actually did, per part, including the
		// parts it skipped. A selected server that has since been deleted is a
		// skip and not a failure, so the run's status cannot carry that
		// information on its own.
		`CREATE TABLE IF NOT EXISTS platform_backup_runs (
			id SERIAL PRIMARY KEY,
			job_id INTEGER NOT NULL REFERENCES platform_backup_jobs(id) ON DELETE CASCADE,
			started_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
			completed_at TIMESTAMPTZ,
			status TEXT NOT NULL DEFAULT 'running',
			size_bytes BIGINT NOT NULL DEFAULT 0,
			storage_key TEXT NOT NULL DEFAULT '',
			storage_id INTEGER REFERENCES backup_storages(id) ON DELETE SET NULL,
			error_message TEXT NOT NULL DEFAULT '',
			components JSONB NOT NULL DEFAULT '[]'::jsonb
		)`,
		`CREATE INDEX IF NOT EXISTS idx_platform_backup_runs_job ON platform_backup_runs(job_id, started_at DESC)`,
	}

	for _, q := range tables {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("platform backup schema: %w", err)
		}
	}
	return nil
}
