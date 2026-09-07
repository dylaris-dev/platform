package database

import (
	"database/sql"
	"fmt"
)

func createBackupTables(db *sql.DB) error {
	tables := []string{
		// Configured storage backends. Admin-managed. The s3 secret access key
		// lives encrypted in its OWN column (secret_enc, AES-256-GCM via
		// pkg/crypto/at_rest.go), hoisted out of the config JSONB at rest so a
		// stray blob read can never surface it; the store re-injects it only on
		// the provider-build read path. Mirrors storage_connections.
		`CREATE TABLE IF NOT EXISTS backup_storages (
			id SERIAL PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			provider TEXT NOT NULL,           -- 'local' | 'shared' | 's3' | 'node-local' | 'core-storage'
			config JSONB NOT NULL DEFAULT '{}'::jsonb,
			secret_enc TEXT NOT NULL DEFAULT '',
			is_default BOOLEAN DEFAULT FALSE,
			created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
		)`,
		// Additive: brings existing deployments' backup_storages up to the
		// encrypted-secret shape. Idempotent, drops nothing. A legacy row keeps
		// its plaintext secret inside config until its next write, which the
		// store migrates into secret_enc (read-then-rewrite).
		`ALTER TABLE backup_storages ADD COLUMN IF NOT EXISTS secret_enc TEXT NOT NULL DEFAULT ''`,
		// Whose storage this is. NULL is the platform's own, which is every row
		// that existed before this column, so the meaning of the existing data
		// does not change. A tenant's row is theirs alone: they configure it,
		// their backups go to it, and we never pay for what it holds.
		//
		// CASCADE rather than SET NULL: a deleted user's S3 credentials must not
		// survive them, and a row left behind with no owner would become a
		// PLATFORM storage - visible to admins and offerable as a default.
		`ALTER TABLE backup_storages ADD COLUMN IF NOT EXISTS owner_id UUID REFERENCES users(id) ON DELETE CASCADE`,
		`CREATE INDEX IF NOT EXISTS idx_backup_storages_owner ON backup_storages(owner_id) WHERE owner_id IS NOT NULL`,
		// The name is unique WITHIN a scope, not globally. A column-level UNIQUE
		// would let one tenant's "Backblaze" block every other tenant's, and a
		// plain UNIQUE (owner_id, name) would do the opposite: Postgres treats
		// NULLs as distinct, so it would stop enforcing uniqueness among the
		// platform rows, which is where it is enforced today.
		`ALTER TABLE backup_storages DROP CONSTRAINT IF EXISTS backup_storages_name_key`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_backup_storages_platform_name ON backup_storages(name) WHERE owner_id IS NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_backup_storages_owner_name ON backup_storages(owner_id, name) WHERE owner_id IS NOT NULL`,
		// One default per scope, enforced by the database rather than only by the
		// transaction that clears the previous one. Two platform defaults would
		// make "the default" whichever row the LIMIT 1 happened to return.
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_backup_storages_platform_default ON backup_storages((is_default)) WHERE is_default AND owner_id IS NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_backup_storages_owner_default ON backup_storages(owner_id) WHERE is_default AND owner_id IS NOT NULL`,
		// Per-server backup job. Multiple jobs allowed per container; each
		// optionally targets a single sub-server (sub_server = NULL means
		// full-container snapshot).
		`CREATE TABLE IF NOT EXISTS backup_jobs (
			id SERIAL PRIMARY KEY,
			server_id INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
			sub_server TEXT,
			name TEXT NOT NULL,
			schedule TEXT NOT NULL DEFAULT 'manual',
			include_patterns TEXT[] NOT NULL DEFAULT '{}',
			exclude_patterns TEXT[] NOT NULL DEFAULT '{}',
			retention_count INTEGER NOT NULL DEFAULT 3,
			storage_id INTEGER REFERENCES backup_storages(id) ON DELETE SET NULL,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			last_run_at TIMESTAMPTZ,
			next_run_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_backup_jobs_server ON backup_jobs(server_id)`,
		`CREATE INDEX IF NOT EXISTS idx_backup_jobs_next_run ON backup_jobs(next_run_at) WHERE enabled = TRUE`,
		// One row per actual backup run (snapshot or attempt).
		`CREATE TABLE IF NOT EXISTS backup_runs (
			id SERIAL PRIMARY KEY,
			job_id INTEGER NOT NULL REFERENCES backup_jobs(id) ON DELETE CASCADE,
			started_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
			completed_at TIMESTAMPTZ,
			status TEXT NOT NULL DEFAULT 'running',  -- running | success | failed
			size_bytes BIGINT NOT NULL DEFAULT 0,
			storage_key TEXT NOT NULL DEFAULT '',
			error_message TEXT NOT NULL DEFAULT ''
		)`,
		// How the sub-servers in this archive were INSTALLED, as JSON, captured
		// when the run succeeded.
		//
		// A restore replaces files without saying anything about the install, so
		// without this the record kept describing whatever was installed LAST -
		// restore a backup taken under modpack version 3 while the record says 5
		// and the setup screen confidently shows the wrong pack. Carried on the
		// run rather than inside the archive so the backup FORMAT does not have to
		// change, and so an archive written by an older node still restores.
		`ALTER TABLE backup_runs ADD COLUMN IF NOT EXISTS install_snapshot TEXT NOT NULL DEFAULT ''`,
		// The archive's own description, the same bytes the node wrote INTO the
		// archive as .dylaris/backup.json. Kept here as well so a same-instance
		// restore does not have to fetch the object to read its first kilobyte;
		// the copy in the archive is what makes a downloaded archive importable
		// into a different Dylaris, which a column in this database cannot be.
		//
		// It supersedes install_snapshot, which stays: every run written before
		// this column has one, and a restore of such a run must still put its
		// install records back.
		`ALTER TABLE backup_runs ADD COLUMN IF NOT EXISTS manifest TEXT NOT NULL DEFAULT ''`,
		// Where this archive actually WENT, resolved at run time.
		//
		// The location used to be re-derived from the job on every read, which is
		// only correct while a job never changes storage. It also could not
		// answer the question billing now asks - whether these bytes are on our
		// storage or on the tenant's own - because a job with no storage set
		// resolves through a chain, and the chain's answer changes.
		//
		// SET NULL on delete, and a NULL here means "before this column, or the
		// storage is gone": both are read as ours, which is the safe direction
		// for a quota (it counts rather than silently exempting).
		`ALTER TABLE backup_runs ADD COLUMN IF NOT EXISTS storage_id INTEGER REFERENCES backup_storages(id) ON DELETE SET NULL`,
		`CREATE INDEX IF NOT EXISTS idx_backup_runs_job ON backup_runs(job_id, started_at DESC)`,
		// One row per restore attempt. We keep history separate from
		// backup_runs because a single archive can be restored many times
		// — collapsing them would mask that.
		`CREATE TABLE IF NOT EXISTS backup_restores (
			id SERIAL PRIMARY KEY,
			run_id INTEGER NOT NULL REFERENCES backup_runs(id) ON DELETE CASCADE,
			server_id INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
			requested_by UUID REFERENCES users(id) ON DELETE SET NULL,
			requested_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
			completed_at TIMESTAMPTZ,
			status TEXT NOT NULL DEFAULT 'queued',
			error_message TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_backup_restores_server ON backup_restores(server_id, requested_at DESC)`,
	}
	for _, q := range tables {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("backup table creation error: %w", err)
		}
	}
	return nil
}
