package database

import (
	"database/sql"
	"fmt"
)

// applyPhase13Schema sets up the Custom Tabs schema:
//   - server_tabs: per-server user-defined tabs that render an external URL
//     in an iframe (Minimap, BlueMap, custom plugin dashboards). V1 holds
//     just the URL — auto-reverse-proxy via Gateway is a follow-up.
func applyPhase13Schema(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS server_tabs (
		id             SERIAL PRIMARY KEY,
		server_id      INTEGER     NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
		name           VARCHAR(64) NOT NULL,
		icon           VARCHAR(64) NOT NULL DEFAULT 'layout-grid',
		url            TEXT        NOT NULL,
		position       INTEGER     NOT NULL DEFAULT 0,
		enabled        BOOLEAN     NOT NULL DEFAULT TRUE,
		open_in_panel  BOOLEAN     NOT NULL DEFAULT TRUE,
		created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		created_by     UUID REFERENCES users(id) ON DELETE SET NULL
	)`); err != nil {
		return fmt.Errorf("phase 13: create server_tabs: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_server_tabs_server
		ON server_tabs(server_id, position ASC)`); err != nil {
		return fmt.Errorf("phase 13: create server_tabs index: %w", err)
	}
	return nil
}

// applyWS5TabProxySchema extends the Custom Tabs schema (WS5) with the
// reverse-proxy columns. All columns are nullable or defaulted so existing
// `direct` rows are untouched. share_token is UNIQUE where present so a
// tokenized `/c/<token>` slug can never collide. Idempotent.
func applyWS5TabProxySchema(db *sql.DB) error {
	for _, q := range []string{
		`ALTER TABLE server_tabs ADD COLUMN IF NOT EXISTS mode             VARCHAR(16)  NOT NULL DEFAULT 'direct'`,
		`ALTER TABLE server_tabs ADD COLUMN IF NOT EXISTS target_port      INTEGER`,
		`ALTER TABLE server_tabs ADD COLUMN IF NOT EXISTS target_path      TEXT         NOT NULL DEFAULT '/'`,
		`ALTER TABLE server_tabs ADD COLUMN IF NOT EXISTS surface          VARCHAR(16)  NOT NULL DEFAULT 'tab'`,
		`ALTER TABLE server_tabs ADD COLUMN IF NOT EXISTS visibility       VARCHAR(16)  NOT NULL DEFAULT 'private'`,
		`ALTER TABLE server_tabs ADD COLUMN IF NOT EXISTS share_token      VARCHAR(32)`,
		`ALTER TABLE server_tabs ADD COLUMN IF NOT EXISTS share_expires_at TIMESTAMPTZ`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("ws5 tab proxy: alter server_tabs: %w", err)
		}
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_server_tabs_share_token
		ON server_tabs(share_token) WHERE share_token IS NOT NULL`); err != nil {
		return fmt.Errorf("ws5 tab proxy: create share_token index: %w", err)
	}
	return nil
}

// applyPhase11Schema sets up the Spark Profiler schema:
//   - spark_profiles: profile-completion records keyed by spark.lucko.me URL
func applyPhase11Schema(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS spark_profiles (
		id              SERIAL PRIMARY KEY,
		server_id       INTEGER     NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
		sub_server_name VARCHAR(128) NOT NULL DEFAULT '',
		url             VARCHAR(512) NOT NULL,
		started_at      TIMESTAMPTZ,
		completed_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		requested_by    UUID REFERENCES users(id) ON DELETE SET NULL,
		UNIQUE(server_id, url)
	)`); err != nil {
		return fmt.Errorf("phase 11: create spark_profiles: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_spark_profiles_server
		ON spark_profiles(server_id, completed_at DESC)`); err != nil {
		return fmt.Errorf("phase 11: create spark_profiles index: %w", err)
	}
	return nil
}

// applyPhase10Schema sets up the Modrinth Mod Browser schema:
//   - server_mods: tracks which Modrinth project/version is installed per
//     server+sub-server so "Update available" can compare installed vs latest
func applyPhase10Schema(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS server_mods (
		id                     SERIAL PRIMARY KEY,
		server_id              INTEGER     NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
		sub_server_name        VARCHAR(128) NOT NULL DEFAULT '',
		modrinth_project_id    VARCHAR(64) NOT NULL,
		modrinth_project_slug  VARCHAR(128) NOT NULL DEFAULT '',
		modrinth_version_id    VARCHAR(64) NOT NULL,
		title                  VARCHAR(255) NOT NULL DEFAULT '',
		file_name              VARCHAR(255) NOT NULL,
		-- Which directory the jar actually went into. Must be REMEMBERED, not
		-- re-derived: the uninstall path used to recompute it from the server's
		-- current installer_type, and PATCH /loader-metadata can change that after
		-- the install, which pointed the removal at the wrong directory.
		-- '' means a row written before this column existed; the handler falls
		-- back to deriving it, which is what those rows were installed with.
		target_dir             VARCHAR(16) NOT NULL DEFAULT '',
		sha512                 VARCHAR(128) NOT NULL DEFAULT '',
		installed_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		installed_by           UUID REFERENCES users(id) ON DELETE SET NULL,
		UNIQUE(server_id, sub_server_name, modrinth_project_id)
	)`); err != nil {
		return fmt.Errorf("phase 10: create server_mods: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_server_mods_server
		ON server_mods(server_id, sub_server_name)`); err != nil {
		return fmt.Errorf("phase 10: create server_mods index: %w", err)
	}
	// For databases created before target_dir was declared above. It belongs
	// here rather than in migrateSchema's ADD COLUMN set: that set runs before
	// this phase, so there the ALTER hits a table that does not exist yet.
	if _, err := db.Exec(`ALTER TABLE server_mods ADD COLUMN IF NOT EXISTS target_dir VARCHAR(16) NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("phase 10: add server_mods.target_dir: %w", err)
	}
	// An install is queued, not performed, so the row used to be written as a
	// fact before the node had done anything - and it stayed a fact whatever the
	// node found. A download that 404ed, a hash that did not match and a
	// successful install were the same row.
	//
	// 'installed' is the DEFAULT so every row written before this reads as what
	// it was taken to be. Only new attempts pass through 'installing'.
	for _, stmt := range []string{
		`ALTER TABLE server_mods ADD COLUMN IF NOT EXISTS status VARCHAR(16) NOT NULL DEFAULT 'installed'`,
		`ALTER TABLE server_mods ADD COLUMN IF NOT EXISTS status_message TEXT NOT NULL DEFAULT ''`,
		// Correlates one attempt with the node's report. Without it a report
		// from a superseded attempt would overwrite the state of the one that
		// replaced it - two clicks in a row is all that takes.
		`ALTER TABLE server_mods ADD COLUMN IF NOT EXISTS install_id VARCHAR(64) NOT NULL DEFAULT ''`,
		// Set by a member who wants this mod left alone by "Update all". The
		// install upsert does not name it, so a manual update keeps the pin.
		`ALTER TABLE server_mods ADD COLUMN IF NOT EXISTS updates_paused BOOLEAN NOT NULL DEFAULT FALSE`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("phase 10: %s: %w", stmt, err)
		}
	}

	// What a mod USED to be, so an update can be undone.
	//
	// The row in server_mods is keyed on the project and is overwritten by an
	// update, which is how the previous file name came to be destroyed by the
	// very operation that needed it: the new jar was written beside the old one
	// and nothing could name the old one afterwards. This is where the
	// superseded entry goes, one row per replaced version, newest kept.
	//
	// Bounded to three per project rather than unbounded: past the last couple
	// of builds nobody rolls back by memory, they pick a build from the list,
	// and an unbounded table would grow with every update forever.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS server_mod_history (
		id                    SERIAL PRIMARY KEY,
		server_id             INTEGER      NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
		sub_server_name       VARCHAR(128) NOT NULL DEFAULT '',
		modrinth_project_id   VARCHAR(64)  NOT NULL,
		modrinth_version_id   VARCHAR(64)  NOT NULL,
		title                 VARCHAR(255) NOT NULL DEFAULT '',
		file_name             VARCHAR(255) NOT NULL,
		target_dir            VARCHAR(16)  NOT NULL DEFAULT '',
		sha512                VARCHAR(128) NOT NULL DEFAULT '',
		-- When this version was installed, and when it stopped being current.
		installed_at          TIMESTAMPTZ  NOT NULL,
		replaced_at           TIMESTAMPTZ  NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("phase 10: create server_mod_history: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_server_mod_history_project
		ON server_mod_history(server_id, sub_server_name, modrinth_project_id, replaced_at DESC)`); err != nil {
		return fmt.Errorf("phase 10: create server_mod_history index: %w", err)
	}
	return nil
}

// applyPhase9Schema sets up the RCON + API keys + Player Management schema:
//   - servers: rcon_enabled, rcon_port, rcon_password columns
//   - api_keys table for external RCON automation
//
// Idempotent. Wired from ensureSchema right after the scheduled-tasks schema.
func applyPhase9Schema(db *sql.DB) error {
	for _, q := range []string{
		`ALTER TABLE servers ADD COLUMN IF NOT EXISTS rcon_enabled       BOOLEAN     NOT NULL DEFAULT FALSE`,
		`ALTER TABLE servers ADD COLUMN IF NOT EXISTS rcon_port          INTEGER     NOT NULL DEFAULT 0`,
		`ALTER TABLE servers ADD COLUMN IF NOT EXISTS rcon_password      TEXT        NOT NULL DEFAULT ''`,
		`ALTER TABLE servers ADD COLUMN IF NOT EXISTS rcon_needs_restart BOOLEAN     NOT NULL DEFAULT FALSE`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("phase 9: alter servers: %w", err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS api_keys (
		id             SERIAL PRIMARY KEY,
		user_id        UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name           VARCHAR(128) NOT NULL,
		key_hash       VARCHAR(64) NOT NULL UNIQUE,
		scope          JSONB        NOT NULL DEFAULT '{}'::jsonb,
		last_used_at   TIMESTAMPTZ,
		expires_at     TIMESTAMPTZ,
		revoked_at     TIMESTAMPTZ,
		rate_per_min   INTEGER      NOT NULL DEFAULT 60,
		created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("phase 9: create api_keys: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_api_keys_user
		ON api_keys(user_id, created_at DESC)`); err != nil {
		return fmt.Errorf("phase 9: create api_keys user index: %w", err)
	}
	return nil
}

// applyPhase8Schema sets up the Configuration Sub-Tabs + Scheduled Tasks schema:
//   - new table: scheduled_tasks (per-server cron jobs for restart + say)
//
// Idempotent. task_type is open-ended (varchar) so later work can introduce
// new types (e.g. rcon-command) without a schema migration.
func applyPhase8Schema(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS scheduled_tasks (
		id             SERIAL PRIMARY KEY,
		server_id      INTEGER     NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
		name           VARCHAR(128) NOT NULL DEFAULT '',
		task_type      VARCHAR(32)  NOT NULL,
		schedule_cron  VARCHAR(128) NOT NULL,
		payload        TEXT         NOT NULL DEFAULT '',
		enabled        BOOLEAN      NOT NULL DEFAULT TRUE,
		next_run       TIMESTAMPTZ,
		last_run       TIMESTAMPTZ,
		last_status    VARCHAR(32)  NOT NULL DEFAULT '',
		last_error     TEXT         NOT NULL DEFAULT '',
		created_by     UUID REFERENCES users(id) ON DELETE SET NULL,
		created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
		updated_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("phase 8: create scheduled_tasks: %w", err)
	}
	// Hot path is "scan due tasks": leader-gated service ticks every 30s and
	// reads WHERE enabled AND next_run <= NOW(). Partial index keeps the
	// scan cheap even at thousands of tasks.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_scheduled_tasks_due
		ON scheduled_tasks(next_run)
		WHERE enabled = TRUE AND next_run IS NOT NULL`); err != nil {
		return fmt.Errorf("phase 8: create scheduled_tasks due index: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_scheduled_tasks_server
		ON scheduled_tasks(server_id, created_at DESC)`); err != nil {
		return fmt.Errorf("phase 8: create scheduled_tasks server index: %w", err)
	}
	return nil
}

// dropTelemetrySettings removes the two settings rows the former usage-stats
// heartbeat owned (phase 18, removed in full). Without this an upgraded install
// keeps rows nothing reads, which later reads as a feature that is merely
// switched off. Idempotent: a fresh install deletes nothing.
func dropTelemetrySettings(db *sql.DB) error {
	if _, err := db.Exec(`DELETE FROM settings
		WHERE key IN ('telemetry_enabled', 'telemetry_endpoint')`); err != nil {
		return fmt.Errorf("drop telemetry settings: %w", err)
	}
	return nil
}

// applyPhase17Schema seeds two settings rows that are INERT: nothing reads
// either of them any more. setup_recovery_token lost its reader when the
// break-glass rework replaced the recovery token with ADMIN_SECRET, and
// last_admin_reset_nonce went with the DYLARIS_RESET_ADMINS path
// (docs/superpowers/plans/2026-07-15-admin-secret-break-glass.md, which records
// leaving this seed in place as a deliberate choice).
//
// Kept, and the comment corrected rather than the code deleted, because both
// rows hold the empty string and dropping them is a migration that buys
// nothing. But see dropTelemetrySettings above for the opposite call on the
// same question - rows nothing reads are exactly what "a feature that is merely
// switched off" looks like to whoever reads the settings table next.
func applyPhase17Schema(db *sql.DB) error {
	for _, k := range []string{"setup_recovery_token", "last_admin_reset_nonce"} {
		if _, err := db.Exec(`INSERT INTO settings (key, value) VALUES ($1, '')
			ON CONFLICT (key) DO NOTHING`, k); err != nil {
			return fmt.Errorf("phase 17: seed %s: %w", k, err)
		}
	}
	return nil
}

// applyModpackCrosscheckSchema sets up the modpack cross-check snapshot:
//   - server_modpack_contents: per-server-per-sub-server snapshot of the
//     modpack's Modrinth-identified members, captured at install/reinstall.
//     Sibling of server_mods; drives the advisory Content-tab cross-check.
//
// Idempotent. Only Modrinth-identified members are ever inserted (a pack member
// with no project id is skipped by the snapshot writer, not stored here).
func applyModpackCrosscheckSchema(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS server_modpack_contents (
		id                       SERIAL PRIMARY KEY,
		server_id                INTEGER     NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
		sub_server_name          TEXT        NOT NULL,
		modrinth_project_id      VARCHAR(64) NOT NULL,
		modrinth_version_id      VARCHAR(64) NOT NULL,
		modrinth_version_number  VARCHAR(64) NOT NULL DEFAULT '',
		file_name                TEXT        NOT NULL DEFAULT '',
		side                     VARCHAR(8)  NOT NULL DEFAULT 'both',
		UNIQUE (server_id, sub_server_name, modrinth_project_id)
	)`); err != nil {
		return fmt.Errorf("modpack crosscheck: create server_modpack_contents: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_server_modpack_contents_server
		ON server_modpack_contents(server_id, sub_server_name)`); err != nil {
		return fmt.Errorf("modpack crosscheck: create server_modpack_contents index: %w", err)
	}
	return nil
}

// applySubServerInstallSchema records HOW each sub-server was installed.
//
// The setup request has carried modrinthProjectId / modrinthVersionId / packId
// since modpack installs existed, with a comment saying we remember them "so the
// panel can later check Modrinth for newer versions and offer one-click update".
// Nothing ever stored them - they were handed to the node and dropped - so the
// panel could not show an operator which modpack a sub-server runs, let alone
// prefill it for an edit. Editing a modpack server to change a JVM flag meant
// re-picking the modpack from memory.
//
// Per (server, sub_server_name) rather than on servers, because sub-servers are
// directories and each one is installed separately; the servers row only ever
// described whichever was active. Same key shape as server_mods and
// server_modpack_contents, deliberately.
func applySubServerInstallSchema(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS sub_server_installs (
		server_id             INTEGER     NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
		sub_server_name       TEXT        NOT NULL,
		installer_type        TEXT        NOT NULL DEFAULT '',
		mc_version            TEXT        NOT NULL DEFAULT '',
		build_version         TEXT        NOT NULL DEFAULT '',
		loader                TEXT        NOT NULL DEFAULT '',
		modrinth_project_id   VARCHAR(64) NOT NULL DEFAULT '',
		modrinth_version_id   VARCHAR(64) NOT NULL DEFAULT '',
		modrinth_project_slug TEXT        NOT NULL DEFAULT '',
		pack_id               INTEGER     NOT NULL DEFAULT 0,
		pack_build_id         INTEGER     NOT NULL DEFAULT 0,
		installed_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (server_id, sub_server_name)
	)`); err != nil {
		return fmt.Errorf("sub-server installs: create sub_server_installs: %w", err)
	}
	return nil
}
