package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"dylaris-core/models"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/lib/pq"
	"log"
	"time"
)

// hashAuthToken returns the at-rest storage form of an emailed auth token
// (password-reset, email-verification). Only the hash is persisted, so a DB
// read can't recover a usable token, and lookups compare a fixed-length hash
// (constant-bucket index probe) rather than the raw secret. Mirrors the
// sha256-hex form used for API keys. 32-byte tokens hash to 64 hex chars,
// which fits the VARCHAR(64) columns.
func hashAuthToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type PostgresStore struct {
	db *sql.DB
	// settingsSecretKey encrypts the credential settings at rest. nil until
	// SetSettingsEncryptionKey runs at boot; nil means pass-through (the
	// pre-encryption behaviour), so tests and early boot are unaffected.
	settingsSecretKey []byte
	// storageConnSecretKey encrypts storage_connections.secret_enc at rest. nil
	// until SetStorageConnEncryptionKey runs at boot. Unlike the settings key a
	// nil key makes a non-empty-secret write fail closed: storage_connections is
	// a greenfield table, so there is no legacy plaintext to tolerate and a
	// secret must never land there in the clear.
	storageConnSecretKey []byte
	// backupStorageSecretKey encrypts backup_storages.secret_enc at rest. nil
	// until SetBackupStorageEncryptionKey runs at boot. A non-empty-secret write
	// fails closed with no key. Unlike storage_connections this table is NOT
	// greenfield, so the read path tolerates a legacy plaintext secret still
	// sitting in config and migrates it into secret_enc on the next write.
	backupStorageSecretKey []byte
}

func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// RawDB exposes the underlying *sql.DB so handlers that need explicit
// transaction control (e.g. backup restore) can use it without the Store
// interface having to grow first-class TX methods. Use sparingly — most
// callers should stick to the interface methods.
func (s *PostgresStore) RawDB() *sql.DB { return s.db }

// Ping verifies the database connection is alive for the status endpoint.
func (s *PostgresStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// TimescaleEnabled reports whether the TimescaleDB extension is installed in
// the current database. server_stats is promoted to a hypertable only when
// present; the status endpoint reports its absence as a degraded metrics
// component rather than a hard failure.
func (s *PostgresStore) TimescaleEnabled(ctx context.Context) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')`,
	).Scan(&exists)
	return exists, err
}

// IsServerStatsHypertable reports whether server_stats is already a hypertable.
// timescaledb_information.hypertables only exists when the extension is present,
// so callers should gate on TimescaleEnabled first.
func (s *PostgresStore) IsServerStatsHypertable(ctx context.Context) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM timescaledb_information.hypertables WHERE hypertable_name = 'server_stats')`,
	).Scan(&ok)
	return ok, err
}

// ConvertServerStatsToHypertable promotes an existing plain server_stats table to
// a hypertable in place. The legacy positional signature matches
// createServerStatsTable in db_tables.go for compatibility with whichever
// TimescaleDB version the operator runs; migrate_data rewrites existing rows.
func (s *PostgresStore) ConvertServerStatsToHypertable(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`SELECT create_hypertable('server_stats', 'time', migrate_data => TRUE, if_not_exists => TRUE)`,
	); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`SELECT add_retention_policy('server_stats', INTERVAL '24 hours', if_not_exists => TRUE)`)
	return err
}

// EstimateServerStatsRows returns the planner's row estimate for server_stats.
// reltuples is approximate (refreshed by ANALYZE/autovacuum) but instant, which
// is what the "consider TimescaleDB" recommendation heuristic needs.
func (s *PostgresStore) EstimateServerStatsRows(ctx context.Context) (int64, error) {
	var n float64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(reltuples, 0) FROM pg_class WHERE relname = 'server_stats'`,
	).Scan(&n)
	if n < 0 {
		n = 0 // reltuples is -1 before the first ANALYZE
	}
	return int64(n), err
}

// ==========================================
// USERS
// ==========================================

// userSelectCols is the canonical column list for User reads. Includes
// totp_secret + totp_backup_codes which are needed for 2FA verification —
// scrubbed from JSON via the json:"-" tag on the model. Region-access flag,
// verification/reset tokens and deletion lifecycle columns are included here;
// the sensitive ones (tokens) are also json:"-" on the model.
// users.id is a UUID (public_id was dropped), and last_username_change is
// tracked here.
const userSelectCols = `id, username, password, COALESCE(email, ''), COALESCE(minecraft_username, ''),
	is_admin, COALESCE(is_2fa_enabled, FALSE), COALESCE(totp_secret, ''), COALESCE(totp_backup_codes::text, '[]'),
	COALESCE(permissions, ''), created_at,
	COALESCE(all_regions_access, FALSE),
	email_verified_at, COALESCE(email_verification_token, ''), email_verification_sent_at,
	COALESCE(password_reset_token, ''), password_reset_expires_at,
	last_login_at, COALESCE(deletion_status, 'active'),
	deletion_warning_sent_at, deletion_scheduled_at,
	COALESCE(role, 'user'),
	COALESCE(can_delete_servers, FALSE),
	COALESCE(can_change_resources, FALSE),
	COALESCE(support_team, ''),
	last_username_change,
	COALESCE(can_create_modpacks, TRUE),
	COALESCE(can_create_modpacks_manual, FALSE)`

func scanUser(scan func(dest ...interface{}) error) (*models.User, error) {
	var (
		u                                                                                        models.User
		createdAt, emailVerifiedAt, emailVerificationSentAt, passwordResetExpiresAt, lastLoginAt sql.NullTime
		deletionWarningSentAt, deletionScheduledAt, lastUsernameChange                           sql.NullTime
	)
	err := scan(&u.ID, &u.Username, &u.Password, &u.Email, &u.MinecraftUsername,
		&u.IsAdmin, &u.Is2FAEnabled, &u.TOTPSecret, &u.TOTPBackupCodes,
		&u.Permissions, &createdAt,
		&u.AllRegionsAccess,
		&emailVerifiedAt, &u.EmailVerificationToken, &emailVerificationSentAt,
		&u.PasswordResetToken, &passwordResetExpiresAt,
		&lastLoginAt, &u.DeletionStatus,
		&deletionWarningSentAt, &deletionScheduledAt,
		&u.Role, &u.CanDeleteServers, &u.CanChangeResources, &u.SupportTeam,
		&lastUsernameChange,
		&u.CanCreateModpacks, &u.CanCreateModpacksManual)
	if err != nil {
		return nil, err
	}
	if createdAt.Valid {
		u.CreatedAt = createdAt.Time
	}
	if emailVerifiedAt.Valid {
		t := emailVerifiedAt.Time
		u.EmailVerifiedAt = &t
	}
	if emailVerificationSentAt.Valid {
		t := emailVerificationSentAt.Time
		u.EmailVerificationSentAt = &t
	}
	if passwordResetExpiresAt.Valid {
		t := passwordResetExpiresAt.Time
		u.PasswordResetExpiresAt = &t
	}
	if lastLoginAt.Valid {
		t := lastLoginAt.Time
		u.LastLoginAt = &t
	}
	if deletionWarningSentAt.Valid {
		t := deletionWarningSentAt.Time
		u.DeletionWarningSentAt = &t
	}
	if deletionScheduledAt.Valid {
		t := deletionScheduledAt.Time
		u.DeletionScheduledAt = &t
	}
	if lastUsernameChange.Valid {
		t := lastUsernameChange.Time
		u.LastUsernameChange = &t
	}
	return &u, nil
}

func (s *PostgresStore) GetUserByUsername(username string) (*models.User, error) {
	query := `SELECT ` + userSelectCols + ` FROM users WHERE username = $1`
	return scanUser(s.db.QueryRow(query, username).Scan)
}

func (s *PostgresStore) GetUserByID(id string) (*models.User, error) {
	query := `SELECT ` + userSelectCols + ` FROM users WHERE id = $1`
	return scanUser(s.db.QueryRow(query, id).Scan)
}

// UsernameTaken reports whether a username is claimed, comparing WITHOUT case.
// excludeUserID lets a rename ignore the caller's own row; pass "" when there is
// none.
//
// Names used to be reserved case-SENSITIVELY, so `NewComer` could be registered
// beside an existing `newcomer` - verified live, two real accounts. Not an
// access-control break, since every lookup is exact and a typo therefore fails
// closed. The harm is that the two are indistinguishable at a glance in a member
// list, an audit log's actor column or a ticket thread, which is the usual
// reason a platform reserves names without case.
//
// This is the friendly half. The real arbiter is the unique index on
// LOWER(username) (see applyUsernameCaseIndex) - a concurrent claim between this
// check and the INSERT still lands as a 23505, which the callers already map to
// ErrUsernameTaken.
func (s *PostgresStore) UsernameTaken(username, excludeUserID string) (bool, error) {
	// NULL rather than "" for "no exclusion": comparing the same placeholder to
	// a string literal AND to a column makes Postgres deduce two types for it
	// and refuse to prepare the statement. TestNoParameterIsBothAssignedAndCompared
	// caught exactly that here before this ever ran.
	var exclude interface{}
	if excludeUserID != "" {
		exclude = excludeUserID
	}
	var exists bool
	err := s.db.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM users
			WHERE LOWER(username) = LOWER($1)
			  AND ($2::text IS NULL OR id::text <> $2::text)
		)`, username, exclude).Scan(&exists)
	return exists, err
}

// ListUsernameCaseCollisions returns each set of usernames that differ only by
// case, newest-registered last within a set.
//
// It exists because the unique index cannot simply be created on a deployment
// that already holds a collision - Postgres refuses, and a migration that dies
// on real data is worse than the drift it was fixing. So the migration asks this
// first and reports instead of failing.
func (s *PostgresStore) ListUsernameCaseCollisions() ([][]string, error) {
	rows, err := s.db.Query(`
		SELECT array_agg(username ORDER BY created_at)
		FROM users
		GROUP BY LOWER(username)
		HAVING COUNT(*) > 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		var names pq.StringArray
		if err := rows.Scan(&names); err != nil {
			return nil, err
		}
		out = append(out, []string(names))
	}
	return out, rows.Err()
}

func (s *PostgresStore) CreateUser(u *models.User) error {
	// Explicit defaults for totp_* — older deployments may have these columns
	// added via migration without the JSONB default actually being applied to
	// freshly inserted rows in some Postgres versions. Writing the values
	// explicitly avoids relying on the schema default at all.
	// users.id is UUID — let the DB DEFAULT gen_random_uuid()
	// fire by omitting id from the column list; RETURNING id pulls the value back.
	// can_create_modpacks is intentionally not in the column list — the
	// DB DEFAULT TRUE from applyPhase16Schema covers new users so callers
	// using the zero-value model still get the right default. Admin flips
	// happen through SetUserCanCreateModpacks.
	query := `INSERT INTO users
		(username, password, email, minecraft_username, is_admin, is_2fa_enabled,
		 totp_secret, totp_backup_codes, permissions)
		VALUES ($1, $2, $3, $4, $5, $6, '', '[]'::jsonb, $7)
		RETURNING id`
	return s.db.QueryRow(query, u.Username, u.Password, u.Email, u.MinecraftUsername, u.IsAdmin, u.Is2FAEnabled, u.Permissions).Scan(&u.ID)
}

// UpdateUser rewrites the user row. NOTE: any username change made via this
// path bypasses the username audit trail (user_username_history) and the
// cooldown policy. Callers MUST route user-initiated and admin-initiated
// rename flows through RenameUser instead; this method is for non-username
// field updates.
func (s *PostgresStore) UpdateUser(u *models.User) error {
	// can_create_modpacks_manual is derived, not passed in: the right-hand side
	// sees the PRE-update row, so the marker is set exactly when this write
	// actually changes the value. Today the only caller round-trips a row it just
	// read, so nothing flips - but the marker is what protects a per-user decision
	// from the platform authoring toggle, and a future caller writing this column
	// without it would silently reopen that hole.
	//
	// The reset-token clear uses the same pre-update read: this is the profile
	// save, which rewrites the password column on EVERY call (the caller
	// round-trips the row), so an unconditional clear would drop a valid reset
	// link because someone edited their email. Only an actual password CHANGE
	// invalidates the link - see UpdateUserPassword for why it does.
	query := `UPDATE users SET username = $1, password = $2, email = $3, minecraft_username = $4, is_admin = $5, is_2fa_enabled = $6, permissions = $7, can_create_modpacks = $8,
		can_create_modpacks_manual = (can_create_modpacks_manual OR can_create_modpacks IS DISTINCT FROM $8),
		password_reset_token      = CASE WHEN password IS DISTINCT FROM $2 THEN NULL ELSE password_reset_token END,
		password_reset_expires_at = CASE WHEN password IS DISTINCT FROM $2 THEN NULL ELSE password_reset_expires_at END
		WHERE id = $9`
	_, err := s.db.Exec(query, u.Username, u.Password, u.Email, u.MinecraftUsername, u.IsAdmin, u.Is2FAEnabled, u.Permissions, u.CanCreateModpacks, u.ID)
	return err
}

// UpdateUserPassword writes a new password AND drops any outstanding
// password-reset link, in one statement.
//
// The two belong together. A user who gets a reset mail they did not ask for
// does the right thing - signs in and changes the password themselves - and
// the link sitting in that mailbox stayed valid for the rest of its hour.
// Measured on a live stack: the owner set their own new password, the
// outstanding link then set a different one, and only the link-holder's
// password worked afterwards. The defensive action handed the account over.
//
// In the same UPDATE rather than a second call, so it cannot be half-done and
// cannot be forgotten by a future caller - the reason the reset handler's own
// ClearPasswordResetToken call is gone.
func (s *PostgresStore) UpdateUserPassword(id string, hashedPassword string) error {
	_, err := s.db.Exec(`UPDATE users
		SET password = $1, password_reset_token = NULL, password_reset_expires_at = NULL
		WHERE id = $2`, hashedPassword, id)
	return err
}

// ErrUserOwnsServers means the user still owns servers. servers.owner_id is
// REFERENCES users(id) with no ON DELETE clause, so Postgres refuses the delete -
// which is the safe outcome, the alternative being servers with no owner. The
// handler used to collapse it into a bare "Delete failed" 500, leaving an admin
// with a button that does not work and no hint that the remedy is to reassign or
// delete those servers first.
var ErrUserOwnsServers = errors.New("user still owns servers")

// ErrUserStillReferenced is the catch-all for any OTHER foreign key that
// refuses the delete. server_invites.invited_by used to be one - it was NOT NULL
// with no ON DELETE clause, so an admin who had granted access on a server they
// did not own became undeletable, with no way to act on the 409. It is nullable
// and ON DELETE SET NULL now (applyInviteAttributionNullable), leaving
// servers.owner_id as the only reference that deliberately blocks. This stays as
// the fallback so a future constraint added without an ON DELETE clause still
// surfaces as a 409 rather than a bare 500.
var ErrUserStillReferenced = errors.New("user is still referenced by other records")

func (s *PostgresStore) DeleteUser(id string) error {
	_, err := s.db.Exec("DELETE FROM users WHERE id = $1", id)
	if err != nil {
		var pqErr *pq.Error
		// 23503 = foreign_key_violation. Every other reference to users either
		// cascades or nulls out, so reaching here means one of the two NO ACTION
		// constraints held.
		if errors.As(err, &pqErr) && pqErr.Code == "23503" {
			if pqErr.Constraint == "servers_owner_id_fkey" {
				return ErrUserOwnsServers
			}
			return ErrUserStillReferenced
		}
	}
	return err
}

func (s *PostgresStore) ListUsers() ([]models.User, error) {
	query := `SELECT ` + userSelectCols + ` FROM users ORDER BY id ASC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []models.User
	for rows.Next() {
		u, err := scanUser(rows.Scan)
		if err != nil {
			// Surface scan failures instead of dropping rows silently —
			// a single bad row used to make freshly-created users vanish
			// from the admin list.
			log.Printf("ListUsers: scan failed: %v", err)
			return nil, err
		}
		users = append(users, *u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return users, nil
}

func (s *PostgresStore) CountUsers() (int, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count)
	return count, err
}

// SetUserTOTP stores the TOTP secret + hashed backup codes JSON for a user
// and sets is_2fa_enabled accordingly. enabled=true means 2FA is now active.
func (s *PostgresStore) SetUserTOTP(id string, secret, backupCodesJSON string, enabled bool) error {
	_, err := s.db.Exec(
		`UPDATE users SET totp_secret = $1, totp_backup_codes = $2::jsonb, is_2fa_enabled = $3 WHERE id = $4`,
		secret, backupCodesJSON, enabled, id,
	)
	return err
}

// DisableUserTOTP wipes the secret + backup codes and sets is_2fa_enabled to false.
// Used by user-self-disable and admin-reset.
func (s *PostgresStore) DisableUserTOTP(id string) error {
	_, err := s.db.Exec(
		`UPDATE users SET totp_secret = '', totp_backup_codes = '[]'::jsonb, is_2fa_enabled = FALSE WHERE id = $1`,
		id,
	)
	return err
}

// ==========================================
// ROLES + PERMISSIONS
// ==========================================

// SetUserRole writes the role and keeps is_admin in sync. is_admin is the
// canonical legacy flag still read by older handlers; keeping it derived from
// role here means new code can rely on role without invalidating legacy code.
//
// is_admin is computed in Go rather than in SQL because the SQL form could
// never run. It read `is_admin = ($1 = 'admin')`, which uses $1 twice: once
// assigned to the varchar role column and once compared to an untyped literal
// that makes it text. The driver declares no type for $1, so Postgres deduces
// both and refuses the contradiction:
//
//	ERROR: inconsistent types deduced for parameter $1
//	DETAIL: text versus character varying
//
// Every call failed, so no user's role could ever be changed. Same defect and
// same wording as the ticket status update fixed alongside this; those were the
// only two of the twelve parameter-reusing queries in core where the two uses
// deduce conflicting types.
func (s *PostgresStore) SetUserRole(userID string, role string) error {
	if role != "user" && role != "support" && role != "admin" {
		return fmt.Errorf("invalid role: %s", role)
	}
	_, err := s.db.Exec(
		`UPDATE users SET role = $1, is_admin = $2 WHERE id = $3`,
		role, role == "admin", userID,
	)
	return err
}

// SetUserPermissionFlags writes the can_* flags and the support_team. Empty
// supportTeam stores NULL so it doesn't pollute users that aren't in any team.
func (s *PostgresStore) SetUserPermissionFlags(userID string, canDeleteServers, canChangeResources bool, supportTeam string) error {
	team := sql.NullString{String: supportTeam, Valid: supportTeam != ""}
	_, err := s.db.Exec(
		`UPDATE users
		    SET can_delete_servers = $1,
		        can_change_resources = $2,
		        support_team = $3
		  WHERE id = $4`,
		canDeleteServers, canChangeResources, team, userID,
	)
	return err
}

// SetUserCanCreateModpacks flips the per-user modpack-author gate. Admin-only
// call site: used by the user-edit form to revoke modpack-authoring rights
// without disabling the feature globally. Returns "user not found" when no
// row was touched, so the handler can return a clean 404.
//
// It also marks the row manual, in the same statement. That marker is what makes
// this decision survive the next platform-wide authoring toggle: the bulk apply
// can skip rows an admin set by hand. Doing it here rather than at the call site
// means no future caller can set the value and forget the marker.
func (s *PostgresStore) SetUserCanCreateModpacks(userID string, can bool) error {
	res, err := s.db.Exec(`UPDATE users SET can_create_modpacks = $1, can_create_modpacks_manual = TRUE WHERE id = $2`, can, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("user not found")
	}
	return nil
}

// ClearUserCanCreateModpacksManual drops the manual marker without touching the
// value, putting the user back under the platform authoring toggle. This is the
// "let the global switch decide again" escape hatch; without it a row marked
// manual once would be pinned forever.
func (s *PostgresStore) ClearUserCanCreateModpacksManual(userID string) error {
	res, err := s.db.Exec(`UPDATE users SET can_create_modpacks_manual = FALSE WHERE id = $1`, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("user not found")
	}
	return nil
}

// BulkSetCanCreateModpacks applies one value to every non-admin user and reports
// how many rows changed.
//
// includeManual = false (the default the admin UI offers) skips rows an admin set
// by hand, so those decisions are preserved. includeManual = true overwrites them
// AND clears their marker, because after a deliberate "apply to everyone" the
// rows are no longer per-user decisions and pretending otherwise would keep them
// frozen out of every later toggle.
//
// Admins are excluded because their authoring never depends on this column
// (RequireUserCanCreateModpacks lets them through before it is read), so writing
// it for them would only be misleading in the users list.
func (s *PostgresStore) BulkSetCanCreateModpacks(can bool, includeManual bool) (int64, error) {
	q := `UPDATE users SET can_create_modpacks = $1 WHERE is_admin = FALSE AND can_create_modpacks IS DISTINCT FROM $1`
	if includeManual {
		q = `UPDATE users SET can_create_modpacks = $1, can_create_modpacks_manual = FALSE
			WHERE is_admin = FALSE AND (can_create_modpacks IS DISTINCT FROM $1 OR can_create_modpacks_manual)`
	} else {
		q += ` AND NOT can_create_modpacks_manual`
	}
	res, err := s.db.Exec(q, can)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// GetUserUpdatesSeen returns the release this user has acknowledged in the
// updates view, or "" when they have acknowledged nothing.
func (s *PostgresStore) GetUserUpdatesSeen(userID string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT COALESCE(updates_seen_version, '') FROM users WHERE id = $1`, userID).Scan(&v)
	if err != nil {
		return "", err
	}
	return v, nil
}

// SetUserUpdatesSeen records the release this user has acknowledged.
// Returns "user not found" when no row was touched.
func (s *PostgresStore) SetUserUpdatesSeen(userID, version string) error {
	res, err := s.db.Exec(`UPDATE users SET updates_seen_version = $1 WHERE id = $2`, version, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("user not found")
	}
	return nil
}

// ==========================================
// NODES
// ==========================================

const nodeSelectCols = `id, name, address, token, status, is_local, COALESCE(tags, ''),
	link_enabled, link_instances, COALESCE(link_secret, ''), COALESCE(cpuset_cpus, ''), created_at,
	COALESCE(public_ip, ''), COALESCE(private_ips::text, '[]'), last_seen_at,
	COALESCE(cpu_overcommit_ratio, 1.0), COALESCE(ram_overcommit_ratio, 1.0),
	COALESCE(total_cpu, 0), COALESCE(total_ram_mb, 0), COALESCE(region, ''), COALESCE(configured, false), owner_id,
	COALESCE(display_name, '')`

func scanNode(scan func(dest ...interface{}) error) (*models.Node, error) {
	var n models.Node
	var privateIPsJSON []byte
	var ownerID sql.NullString
	err := scan(&n.ID, &n.Name, &n.Address, &n.Token, &n.Status, &n.IsLocal, &n.Tags,
		&n.LinkEnabled, &n.LinkInstances, &n.LinkSecret, &n.CpusetCpus, &n.CreatedAt, &n.PublicIP, &privateIPsJSON, &n.LastSeenAt,
		&n.CPUOvercommitRatio, &n.RAMOvercommitRatio, &n.TotalCPU, &n.TotalRAMMB, &n.Region, &n.Configured, &ownerID,
		&n.DisplayName)
	if err != nil {
		return nil, err
	}
	if ownerID.Valid {
		n.OwnerID = &ownerID.String
	}
	if len(privateIPsJSON) > 0 {
		json.Unmarshal(privateIPsJSON, &n.PrivateIPs)
	}
	if n.PrivateIPs == nil {
		n.PrivateIPs = []string{}
	}
	return &n, nil
}

func (s *PostgresStore) GetNodeByID(id int) (*models.Node, error) {
	query := `SELECT ` + nodeSelectCols + ` FROM nodes WHERE id = $1`
	return scanNode(s.db.QueryRow(query, id).Scan)
}

func (s *PostgresStore) GetNodeByToken(token string) (*models.Node, error) {
	query := `SELECT ` + nodeSelectCols + ` FROM nodes WHERE token = $1`
	return scanNode(s.db.QueryRow(query, token).Scan)
}

func (s *PostgresStore) GetNodeByName(name string) (*models.Node, error) {
	query := `SELECT ` + nodeSelectCols + ` FROM nodes WHERE name = $1`
	return scanNode(s.db.QueryRow(query, name).Scan)
}

// ListNodes returns the whole fleet. Like ListServersByNode this drives
// decisions rather than a display: the discovery sweep marks every node it does
// not find here offline, and the rebalance worker picks both its source and its
// target from it. A node silently dropped by a failed scan is a node the
// platform stops reasoning about while it is still running servers, so a short
// read is an error here too.
// ListNodesByOwner returns the nodes a tenant brought, which is a different
// question from ListNodes filtered in Go: the account teardown has to enumerate
// them BEFORE the user row goes, and afterwards owner_id is NULL and there is
// nothing left to match on.
func (s *PostgresStore) ListNodesByOwner(ownerID string) ([]models.Node, error) {
	query := `SELECT ` + nodeSelectCols + ` FROM nodes WHERE owner_id = $1 ORDER BY id ASC`
	rows, err := s.db.Query(query, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []models.Node
	for rows.Next() {
		n, err := scanNode(rows.Scan)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, *n)
	}
	return nodes, rows.Err()
}

func (s *PostgresStore) ListNodes() ([]models.Node, error) {
	query := `SELECT ` + nodeSelectCols + ` FROM nodes ORDER BY id ASC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []models.Node
	for rows.Next() {
		n, err := scanNode(rows.Scan)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, *n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return nodes, nil
}

func (s *PostgresStore) CreateNode(n *models.Node) error {
	query := `INSERT INTO nodes (name, address, token, status, is_local, tags, link_enabled, link_instances, link_secret, region)
	          VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`
	return s.db.QueryRow(query, n.Name, n.Address, n.Token, n.Status, n.IsLocal, n.Tags, n.LinkEnabled, n.LinkInstances, n.LinkSecret, n.Region).Scan(&n.ID)
}

// ErrNodeHasServers means servers still point at this node. servers.node_id is
// REFERENCES nodes(id) with no ON DELETE clause, so Postgres refuses - the safe
// outcome, the alternative being servers pointing at a node that is gone. Same
// treatment as ErrUserOwnsServers: without it the handler answered a bare 500
// and the operator had a delete button that silently did nothing. ForceDeleteNode
// is the deliberate path through this, and it removes the servers first.
var ErrNodeHasServers = errors.New("node still has servers")

func (s *PostgresStore) DeleteNode(id int) error {
	_, err := s.db.Exec("DELETE FROM nodes WHERE id = $1", id)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23503" && pqErr.Constraint == "servers_node_id_fkey" {
			return ErrNodeHasServers
		}
	}
	return err
}

func (s *PostgresStore) SetNodeStatus(id int, status string) error {
	_, err := s.db.Exec("UPDATE nodes SET status = $1 WHERE id = $2", status, id)
	return err
}

func (s *PostgresStore) SetNodeTags(id int, tags string) error {
	_, err := s.db.Exec("UPDATE nodes SET tags = $1 WHERE id = $2", tags, id)
	return err
}

func (s *PostgresStore) SetNodeName(id int, name string) error {
	_, err := s.db.Exec("UPDATE nodes SET name = $1 WHERE id = $2", name, id)
	return err
}

func (s *PostgresStore) SetNodeAddress(id int, address string) error {
	_, err := s.db.Exec("UPDATE nodes SET address = $1 WHERE id = $2", address, id)
	return err
}

func (s *PostgresStore) SetNodeIPs(id int, publicIP string, privateIPs []string) error {
	ipsJSON, err := json.Marshal(privateIPs)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("UPDATE nodes SET public_ip = $1, private_ips = $2::jsonb WHERE id = $3", publicIP, string(ipsJSON), id)
	return err
}

func (s *PostgresStore) UpdateNodeCpusetCpus(id int, cpusetCpus string) error {
	_, err := s.db.Exec("UPDATE nodes SET cpuset_cpus = $1 WHERE id = $2", cpusetCpus, id)
	return err
}

func (s *PostgresStore) SetNodeLastSeen(id int) error {
	_, err := s.db.Exec("UPDATE nodes SET last_seen_at = CURRENT_TIMESTAMP WHERE id = $1", id)
	return err
}

func (s *PostgresStore) CountServersByNode(nodeID int) (int, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM servers WHERE node_id = $1", nodeID).Scan(&count)
	return count, err
}

// ServerDiskLimitsByNode returns uuid -> disk limit in MB for every server on
// the node. Includes servers with a 0 limit so a caller can tell "unlimited"
// apart from "not on this node".
func (s *PostgresStore) ServerDiskLimitsByNode(nodeID int) (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT uuid, COALESCE(disk_limit, 0) FROM servers WHERE node_id = $1`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int64{}
	for rows.Next() {
		var uuid string
		var limitMB int64
		if err := rows.Scan(&uuid, &limitMB); err != nil {
			continue
		}
		out[uuid] = limitMB
	}
	return out, rows.Err()
}

// ListServersByNode returns every server assigned to a node.
//
// This list is NOT a display list: three callers read an omission as a positive
// statement that the server does not exist, and act on it destructively.
//
//   - services/acl_reconciler.go and the node handshake (main.go
//     ServerUUIDsByNode) turn it into the node's Redis ACL. BuildNodeACLRules
//     starts with "resetkeys", so a server missing from this slice is not
//     merely skipped - its ~dylaris:server:<uuid>:* grant is REVOKED, and the
//     node running it loses console, status, stats and commands for it.
//   - handlers/nodes.go's disk analysis builds dbByUUID from it and classifies
//     every on-disk directory with no matching entry as ORPHANED, which the
//     panel offers to delete. A live server dropped here is presented to the
//     operator as garbage, one click from losing its data.
//
// So a short read must be an error, never a short slice: both a scan failure
// and an iteration cut short mid-stream return here instead of continuing.
func (s *PostgresStore) ListServersByNode(nodeID int) ([]models.Server, error) {
	query := `SELECT s.id, s.uuid, s.name, s.status FROM servers s WHERE s.node_id = $1 ORDER BY s.id ASC`
	rows, err := s.db.Query(query, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var servers []models.Server
	for rows.Next() {
		var srv models.Server
		if err := rows.Scan(&srv.ID, &srv.UUID, &srv.Name, &srv.Status); err != nil {
			return nil, err
		}
		servers = append(servers, srv)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return servers, nil
}

func (s *PostgresStore) DeleteServersByNode(nodeID int) error {
	_, err := s.db.Exec("DELETE FROM servers WHERE node_id = $1", nodeID)
	return err
}

// DeleteStaleOfflineNodes sweeps PLATFORM nodes (owner_id IS NULL) that have
// been offline past the cutoff and carry no servers. NodeCleanupService runs it
// every 5 minutes with a 24h cutoff.
//
// The owner_id filter is the load-bearing half. A BYON node is a tenant's own
// machine, paired deliberately through a single-use enroll token, and a customer
// who registers one and then does not create a server for a day - a laptop, a
// home box switched off over a weekend - had the pairing deleted out from under
// them. The consequences are worse than a missing row: the node's ACL users are
// pruned, and on the next boot it still holds its cached .node_secret, so
// redisacl_bootstrap's "paired node with no cached secret" guard never fires. It
// reconnects with an identity Core has forgotten, is rejected as unknown, and
// loops on a rejected handshake with no BYON way back in (that path needs an
// enroll or recovery token the tenant does not have). Support ticket, every time.
//
// The operator's own fleet keeps the sweep: an unadopted platform node that
// stopped reporting is exactly the churn this was written to clean up, and the
// operator can always re-enroll one with the cluster secret.
func (s *PostgresStore) DeleteStaleOfflineNodes(offlineSince time.Time) (int, error) {
	result, err := s.db.Exec(`
		DELETE FROM nodes
		WHERE status = 'offline'
			AND owner_id IS NULL
			AND last_seen_at IS NOT NULL
			AND last_seen_at < $1
			AND id NOT IN (SELECT DISTINCT node_id FROM servers)
	`, offlineSince)
	if err != nil {
		return 0, err
	}
	count, _ := result.RowsAffected()
	return int(count), nil
}

// ==========================================
// SERVERS
// ==========================================

func (s *PostgresStore) CreateServer(srv *models.Server) (int64, error) {
	var id int64
	// region is written explicitly. It used to be left to the column default, so
	// every server read back as "default" no matter which node it actually landed
	// on - and servers.region is what CountServersInRegion counts, which is the
	// guard that refuses to delete a region still in use. A region full of servers
	// looked empty to it.
	query := `INSERT INTO servers (uuid, name, node_id, owner_id, game_image, port, memory, cpu_limit, start_command, status, is_fixed, active_sub_server, extra_jvm_flags, disk_limit, server_type, proxy_id, auto_move, region)
	          VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, COALESCE(NULLIF($18, ''), 'default')) RETURNING id`

	err := s.db.QueryRow(query, srv.UUID, srv.Name, srv.NodeID, srv.OwnerID, srv.GameImage, srv.Port, srv.Memory, srv.CPULimit, srv.StartCommand, srv.Status, srv.IsFixed, srv.ActiveSubServer, srv.ExtraJvmFlags, srv.DiskLimit, srv.ServerType, srv.ProxyID, srv.AutoMove, srv.Region).Scan(&id)
	return id, err
}

// SetServerAutoMove flips the auto-move opt-in for a single server.
// The migration worker only ever considers servers with auto_move=true.
func (s *PostgresStore) SetServerAutoMove(id int, enabled bool) error {
	_, err := s.db.Exec("UPDATE servers SET auto_move = $1 WHERE id = $2", enabled, id)
	return err
}

// UpdateServerNode reassigns a server to a different node (auto-move target).
//
// The region follows the node, because that is what the region MEANS here: where
// the server physically runs. Leaving it behind would make a migrated server
// count against a region it no longer occupies, and CountServersInRegion is the
// guard on deleting a region.
func (s *PostgresStore) UpdateServerNode(serverID int, newNodeID int) error {
	_, err := s.db.Exec(
		`UPDATE servers SET node_id = $1,
		        region = COALESCE(NULLIF((SELECT region FROM nodes WHERE id = $1), ''), region)
		  WHERE id = $2`, newNodeID, serverID)
	return err
}

// ResetAllAutoMove clears every per-server auto-move opt-in in one statement.
func (s *PostgresStore) ResetAllAutoMove() error {
	_, err := s.db.Exec("UPDATE servers SET auto_move = false")
	return err
}

// SumAllocatedByNode returns the total memory (MB) and cpu_limit (cores)
// allocated across every server on a node, regardless of running state.
// Used by the scheduler to enforce capacity * overcommit_ratio.
func (s *PostgresStore) SumAllocatedByNode(nodeID int) (totalRAMMB int64, totalCPU float64, err error) {
	err = s.db.QueryRow(
		`SELECT COALESCE(SUM(memory), 0), COALESCE(SUM(cpu_limit), 0) FROM servers WHERE node_id = $1`,
		nodeID,
	).Scan(&totalRAMMB, &totalCPU)
	return
}

// SetNodePlacement persists the admin-configured overcommit ratios for a node.
func (s *PostgresStore) SetNodePlacement(id int, cpuRatio, ramRatio float64) error {
	_, err := s.db.Exec(
		`UPDATE nodes SET cpu_overcommit_ratio = $1, ram_overcommit_ratio = $2 WHERE id = $3`,
		cpuRatio, ramRatio, id,
	)
	return err
}

// UpdateNodeCapacity caches the physical totals reported in the node's
// heartbeat so the scheduler does not need a live ping to compute available
// resources. Called from the discovery service when stats land.
func (s *PostgresStore) UpdateNodeCapacity(id int, totalCPU float64, totalRAMMB int64) error {
	_, err := s.db.Exec(
		`UPDATE nodes SET total_cpu = $1, total_ram_mb = $2 WHERE id = $3`,
		totalCPU, totalRAMMB, id,
	)
	return err
}

// SetNodeRegion persists the canonical region key reported by a node's
// DYLARIS_REGION env. Empty string clears the region (node is "anywhere").
func (s *PostgresStore) SetNodeRegion(id int, region string) error {
	_, err := s.db.Exec(`UPDATE nodes SET region = $1 WHERE id = $2`, region, id)
	return err
}

// SetNodeOwner binds a node to a BYON tenant (or clears it back to a platform
// node when ownerID is nil). Only used in BYON mode.
func (s *PostgresStore) SetNodeOwner(id int, ownerID *string) error {
	_, err := s.db.Exec(`UPDATE nodes SET owner_id = $1 WHERE id = $2`, ownerID, id)
	return err
}

// SetNodeDisplayName sets the optional human label shown in the Panel. Does not
// touch the identity (token) or the unique name.
func (s *PostgresStore) SetNodeDisplayName(id int, name string) error {
	_, err := s.db.Exec(`UPDATE nodes SET display_name = $1 WHERE id = $2`, name, id)
	return err
}

func (s *PostgresStore) GetNodeSecretEnc(id int) (string, error) {
	var enc string
	err := s.db.QueryRow("SELECT node_secret_enc FROM nodes WHERE id = $1", id).Scan(&enc)
	return enc, err
}

func (s *PostgresStore) SetNodeSecretEnc(id int, enc string) error {
	_, err := s.db.Exec("UPDATE nodes SET node_secret_enc = $1 WHERE id = $2", enc, id)
	return err
}

// SetNodeSecretEncIfUnchanged writes next only while the row still holds prev,
// and reports whether it landed. Every Core replica mints for the same node at
// the same moment, because a node dials them all at once; an unconditional
// UPDATE lets the last writer win and locks the node out for good. See
// redisacl.LoadOrCreateNodeSecret.
func (s *PostgresStore) SetNodeSecretEncIfUnchanged(id int, prev, next string) (bool, error) {
	res, err := s.db.Exec(
		"UPDATE nodes SET node_secret_enc = $1 WHERE id = $2 AND node_secret_enc = $3", next, id, prev)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// SetNodeConfig persists an admin's panel-configured name, region and tags in
// one update and flips configured=true so the discovery scan stops letting the
// heartbeat env overwrite these fields. Used by the unconfigured-node flow.
func (s *PostgresStore) SetNodeConfig(id int, name, region, tags string) error {
	_, err := s.db.Exec(
		`UPDATE nodes SET name = $1, region = $2, tags = $3, configured = TRUE WHERE id = $4`,
		name, region, tags, id,
	)
	return err
}

func (s *PostgresStore) ListServers(filterByUser string) ([]models.Server, error) {
	query := `
		SELECT s.id, s.uuid, s.name, n.name as node_name, u.username as owner_name, s.port, s.status, COALESCE(s.desired_state, 'stopped'), s.game_image, s.is_fixed, COALESCE(s.active_sub_server, ''), s.created_at, COALESCE(s.server_type, 'game'), s.proxy_id
		FROM servers s
		JOIN nodes n ON s.node_id = n.id
		JOIN users u ON s.owner_id = u.id
	`
	var rows *sql.Rows
	var err error

	if filterByUser != "" {
		query += " WHERE u.username = $1 ORDER BY s.id ASC"
		rows, err = s.db.Query(query, filterByUser)
	} else {
		query += " ORDER BY s.id ASC"
		rows, err = s.db.Query(query)
	}

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var servers []models.Server
	for rows.Next() {
		var srv models.Server
		if err := rows.Scan(&srv.ID, &srv.UUID, &srv.Name, &srv.NodeName, &srv.OwnerName, &srv.Port, &srv.Status, &srv.DesiredState, &srv.GameImage, &srv.IsFixed, &srv.ActiveSubServer, &srv.CreatedAt, &srv.ServerType, &srv.ProxyID); err != nil {
			continue
		}
		servers = append(servers, srv)
	}
	return servers, nil
}

func (s *PostgresStore) GetServerByID(id int) (*models.Server, error) {
	var srv models.Server
	query := `
		SELECT s.id, s.uuid, s.name, s.node_id, n.name as node_name, s.owner_id, u.username as owner_name, s.game_image, s.port, s.memory, COALESCE(s.cpu_limit, 0), COALESCE(s.start_command, ''), s.status, COALESCE(s.desired_state, 'stopped'), s.is_fixed, COALESCE(s.active_sub_server, ''), COALESCE(s.extra_jvm_flags, ''), s.created_at, COALESCE(s.installer_type, ''), COALESCE(s.minecraft_version, ''), COALESCE(s.build_number, ''), COALESCE(s.disk_limit, 0), COALESCE(s.server_type, 'game'), s.proxy_id, COALESCE(n.address, ''), COALESCE(s.host_port, 0), COALESCE(s.container_port, 25565), COALESCE(s.auto_move, FALSE), COALESCE(s.cpu_pinning_mode, 'shared'), COALESCE(s.cpuset, ''), COALESCE(n.status, 'offline'), n.last_seen_at
		FROM servers s
		JOIN nodes n ON s.node_id = n.id
		JOIN users u ON s.owner_id = u.id
		WHERE s.id = $1
	`
	err := s.db.QueryRow(query, id).Scan(&srv.ID, &srv.UUID, &srv.Name, &srv.NodeID, &srv.NodeName, &srv.OwnerID, &srv.OwnerName, &srv.GameImage, &srv.Port, &srv.Memory, &srv.CPULimit, &srv.StartCommand, &srv.Status, &srv.DesiredState, &srv.IsFixed, &srv.ActiveSubServer, &srv.ExtraJvmFlags, &srv.CreatedAt, &srv.InstallerType, &srv.MinecraftVersion, &srv.BuildNumber, &srv.DiskLimit, &srv.ServerType, &srv.ProxyID, &srv.NodeAddress, &srv.HostPort, &srv.ContainerPort, &srv.AutoMove, &srv.CPUPinningMode, &srv.Cpuset, &srv.NodeStatus, &srv.NodeLastSeenAt)
	if err != nil {
		return nil, err
	}
	return &srv, nil
}

func (s *PostgresStore) GetServerByUUID(uuid string) (*models.Server, error) {
	var srv models.Server
	query := `
		SELECT s.id, s.uuid, s.name, s.node_id, n.name as node_name, s.owner_id, u.username as owner_name, s.game_image, s.port, s.memory, COALESCE(s.cpu_limit, 0), COALESCE(s.start_command, ''), s.status, COALESCE(s.desired_state, 'stopped'), s.is_fixed, COALESCE(s.active_sub_server, ''), COALESCE(s.extra_jvm_flags, ''), s.created_at, COALESCE(s.installer_type, ''), COALESCE(s.minecraft_version, ''), COALESCE(s.build_number, ''), COALESCE(s.disk_limit, 0), COALESCE(s.server_type, 'game'), s.proxy_id, COALESCE(n.address, ''), COALESCE(s.host_port, 0), COALESCE(s.container_port, 25565), COALESCE(s.cpu_pinning_mode, 'shared'), COALESCE(s.cpuset, ''), COALESCE(n.status, 'offline'), n.last_seen_at
		FROM servers s
		JOIN nodes n ON s.node_id = n.id
		JOIN users u ON s.owner_id = u.id
		WHERE s.uuid = $1
	`
	err := s.db.QueryRow(query, uuid).Scan(&srv.ID, &srv.UUID, &srv.Name, &srv.NodeID, &srv.NodeName, &srv.OwnerID, &srv.OwnerName, &srv.GameImage, &srv.Port, &srv.Memory, &srv.CPULimit, &srv.StartCommand, &srv.Status, &srv.DesiredState, &srv.IsFixed, &srv.ActiveSubServer, &srv.ExtraJvmFlags, &srv.CreatedAt, &srv.InstallerType, &srv.MinecraftVersion, &srv.BuildNumber, &srv.DiskLimit, &srv.ServerType, &srv.ProxyID, &srv.NodeAddress, &srv.HostPort, &srv.ContainerPort, &srv.CPUPinningMode, &srv.Cpuset, &srv.NodeStatus, &srv.NodeLastSeenAt)
	if err != nil {
		return nil, err
	}
	return &srv, nil
}

func (s *PostgresStore) DeleteServer(id int) error {
	_, err := s.db.Exec("DELETE FROM servers WHERE id = $1", id)
	return err
}

func (s *PostgresStore) UpdateServerStatus(id int, status string) error {
	_, err := s.db.Exec("UPDATE servers SET status = $1 WHERE id = $2", status, id)
	return err
}

func (s *PostgresStore) UpdateServerDesiredState(id int, desiredState string) error {
	_, err := s.db.Exec("UPDATE servers SET desired_state = $1 WHERE id = $2", desiredState, id)
	return err
}

func (s *PostgresStore) UpdateServerSetup(id int, image, command, activeSubServer, extraJvmFlags, installerType, minecraftVersion, buildNumber string) error {
	_, err := s.db.Exec("UPDATE servers SET game_image = $1, start_command = $2, active_sub_server = $3, extra_jvm_flags = $4, installer_type = $5, minecraft_version = $6, build_number = $7 WHERE id = $8",
		image, command, activeSubServer, extraJvmFlags, installerType, minecraftVersion, buildNumber, id)
	return err
}

// UpdateServerRuntime persists ONLY the Java image and the JVM flags.
//
// Deliberately narrow. UpdateServerSetup writes those two alongside the active
// sub-server and the whole loader triple, which is right after an install and
// wrong for a settings change: a runtime edit would then rewrite the installer
// metadata from whatever the form happened to hold, and the Content tab reads
// that triple to decide whether it is looking at mods or plugins.
func (s *PostgresStore) UpdateServerRuntime(serverID int, javaImage, extraJvmFlags string) error {
	_, err := s.db.Exec("UPDATE servers SET game_image = $1, extra_jvm_flags = $2 WHERE id = $3",
		javaImage, extraJvmFlags, serverID)
	return err
}

// UpdateServerLoaderMetadata persists ONLY installer_type/minecraft_version/
// build_number, unlike UpdateServerSetup which also rewrites game_image/
// start_command/active_sub_server/extra_jvm_flags. Used by the metadata-only
// "declare loader/version" path for an imported server, which must never touch
// those other columns or trigger an install.
func (s *PostgresStore) UpdateServerLoaderMetadata(id int, installerType, minecraftVersion, buildNumber string) error {
	_, err := s.db.Exec("UPDATE servers SET installer_type = $1, minecraft_version = $2, build_number = $3 WHERE id = $4",
		installerType, minecraftVersion, buildNumber, id)
	return err
}

func (s *PostgresStore) UpdateServerActiveSubServer(id int, subServer string) error {
	_, err := s.db.Exec("UPDATE servers SET active_sub_server = $1 WHERE id = $2", subServer, id)
	return err
}

func (s *PostgresStore) UpdateServerName(id int, name string) error {
	_, err := s.db.Exec("UPDATE servers SET name = $1 WHERE id = $2", name, id)
	return err
}

func (s *PostgresStore) UpdateServerResources(id int, ram int, cpuLimit float64, diskLimit int64) error {
	_, err := s.db.Exec("UPDATE servers SET memory = $1, cpu_limit = $2, disk_limit = $3 WHERE id = $4", ram, cpuLimit, diskLimit, id)
	return err
}

// UpdateServerCPUPinning persists the per-server CPU pinning mode + effective
// cpuset. The container is recreated separately (by the resources-update path)
// for the change to take effect.
func (s *PostgresStore) UpdateServerCPUPinning(id int, mode, cpuset string) error {
	_, err := s.db.Exec("UPDATE servers SET cpu_pinning_mode = $1, cpuset = $2 WHERE id = $3", mode, cpuset, id)
	return err
}

// ResetServerCPUPinningByNode clears CPU pinning (back to 'shared', no cpuset)
// for every server on a node. Used when the node's host CPU topology changes
// (hardware swap) so stale cpusets do not reference cores that no longer exist.
// Returns how many rows were reset.
func (s *PostgresStore) ResetServerCPUPinningByNode(nodeID int) (int64, error) {
	res, err := s.db.Exec(
		`UPDATE servers SET cpu_pinning_mode = 'shared', cpuset = ''
		 WHERE node_id = $1 AND (cpu_pinning_mode <> 'shared' OR COALESCE(cpuset, '') <> '')`,
		nodeID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListServerCpusetsByNode returns serverID -> cpuset for every server on the
// node that has a non-empty cpuset. Used to compute per-core load for the
// auto-distribution spread.
func (s *PostgresStore) ListServerCpusetsByNode(nodeID int) (map[int]string, error) {
	rows, err := s.db.Query(
		`SELECT id, COALESCE(cpuset, '') FROM servers WHERE node_id = $1 AND COALESCE(cpuset, '') <> ''`,
		nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var id int
		var cs string
		if err := rows.Scan(&id, &cs); err != nil {
			return nil, err
		}
		out[id] = cs
	}
	return out, rows.Err()
}

func (s *PostgresStore) UpdateServerPorts(id int, hostPort, containerPort int) error {
	_, err := s.db.Exec("UPDATE servers SET host_port = $1, container_port = $2 WHERE id = $3", hostPort, containerPort, id)
	return err
}

func (s *PostgresStore) GetUsedHostPortsOnNode(nodeID int) ([]int, error) {
	rows, err := s.db.Query("SELECT host_port FROM servers WHERE node_id = $1 AND host_port > 0", nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ports []int
	for rows.Next() {
		var p int
		// Errors are RETURNED, not skipped. This list is a conflict check: a
		// port dropped here reads as free and the caller hands it to a second
		// server, which then fails to bind at start time - far from the admin
		// action that caused it.
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		ports = append(ports, p)
	}
	return ports, rows.Err()
}

func (s *PostgresStore) GetAllActiveServers() ([]models.Server, error) {
	rows, err := s.db.Query(`
		SELECT s.id, s.uuid, s.node_id, n.name as node_name, n.token as node_token, s.status, COALESCE(s.host_port, 0), COALESCE(s.container_port, 25565), COALESCE(s.memory, 1024), COALESCE(s.cpu_limit, 0), COALESCE(s.disk_limit, 0), COALESCE(s.start_command, ''), COALESCE(s.game_image, ''), COALESCE(s.active_sub_server, ''), COALESCE(s.extra_jvm_flags, '')
		FROM servers s
		JOIN nodes n ON s.node_id = n.id
		WHERE s.status != 'pending_setup'
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var servers []models.Server
	for rows.Next() {
		var srv models.Server
		// NodeName is reused to carry the node token for migration purposes
		var nodeToken string
		if err := rows.Scan(&srv.ID, &srv.UUID, &srv.NodeID, &srv.NodeName, &nodeToken, &srv.Status, &srv.HostPort, &srv.ContainerPort, &srv.Memory, &srv.CPULimit, &srv.DiskLimit, &srv.StartCommand, &srv.GameImage, &srv.ActiveSubServer, &srv.ExtraJvmFlags); err != nil {
			return nil, err
		}
		// Store the node token in NodeAddress field temporarily (migration service reads it)
		srv.NodeAddress = nodeToken
		servers = append(servers, srv)
	}
	// The routing migration takes this list as the whole fleet: it reports
	// len(servers) as its Total and migrates exactly these. An iteration cut
	// short by a connection blip would otherwise return the rows read so far
	// with a nil error, so the migration would convert part of the fleet,
	// report success, and leave the rest on the old routing - unreachable, with
	// nothing anywhere saying so.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return servers, nil
}

func (s *PostgresStore) UpdateServerProxyID(id int, proxyID *int) error {
	_, err := s.db.Exec("UPDATE servers SET proxy_id = $1 WHERE id = $2", proxyID, id)
	return err
}

func (s *PostgresStore) UpdateServerOwner(id int, ownerID *string) error {
	if ownerID == nil {
		return fmt.Errorf("owner_id cannot be null (use DeleteServer to remove orphaned DB entries)")
	}
	_, err := s.db.Exec("UPDATE servers SET owner_id = $1 WHERE id = $2", *ownerID, id)
	return err
}

func (s *PostgresStore) CountServersByOwner(ownerID string) (int, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM servers WHERE owner_id = $1", ownerID).Scan(&count)
	return count, err
}

// ==========================================
// SERVER INVITES
// ==========================================

// CreateInvite inserts a legacy-shaped member invite. cap_overrides, inherit
// and owner_user_id are derived from the SAME permissions blob via
// MapLegacyInviteCaps so the resolver (which reads cap_overrides, not
// permissions) sees the invite's caps immediately - without this, a new
// invite would have zero caps until the next boot's migration pass. The
// derived shape is identical to what migrateLegacyServerInvites writes, so an
// inline-written row and a migrated row are indistinguishable to that guard.
func (s *PostgresStore) CreateInvite(serverID int, userID, invitedBy string, permissions map[string]bool) error {
	permsJSON, err := json.Marshal(permissions)
	if err != nil {
		return err
	}
	var tp models.TabPermissions
	_ = json.Unmarshal(permsJSON, &tp)
	ovJSON, err := json.Marshal(CapOverrides{Grant: MapLegacyInviteCaps(tp)})
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO server_invites (server_id, user_id, invited_by, permissions, cap_overrides, inherit, owner_user_id)
		 VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6, (SELECT owner_id FROM servers WHERE id = $1))`,
		serverID, userID, invitedBy, string(permsJSON), string(ovJSON), tp.Inherit)
	return err
}

func (s *PostgresStore) DeleteInvite(serverID int, userID string) error {
	_, err := s.db.Exec("DELETE FROM server_invites WHERE server_id = $1 AND user_id = $2", serverID, userID)
	return err
}

// UpdateInvitePermissions edits a legacy-shaped invite's permissions and
// recomputes cap_overrides + inherit + owner_user_id from the new blob (see
// CreateInvite). Without this, editing or revoking permissions would update
// only the legacy blob: cap_overrides would keep the OLD caps forever (the
// boot migration guard skips already-migrated rows), so a revoke would
// silently fail to reach the resolver. server_role_id is left untouched - a
// member holding a server-role via /grants keeps it; this path only manages
// the override set.
func (s *PostgresStore) UpdateInvitePermissions(serverID int, userID string, permissions map[string]bool) error {
	permsJSON, err := json.Marshal(permissions)
	if err != nil {
		return err
	}
	var tp models.TabPermissions
	_ = json.Unmarshal(permsJSON, &tp)
	ovJSON, err := json.Marshal(CapOverrides{Grant: MapLegacyInviteCaps(tp)})
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`UPDATE server_invites SET permissions = $1::jsonb, cap_overrides = $2::jsonb, inherit = $3,
		   owner_user_id = (SELECT owner_id FROM servers WHERE id = $4)
		 WHERE server_id = $4 AND user_id = $5`,
		string(permsJSON), string(ovJSON), tp.Inherit, serverID, userID)
	return err
}

func (s *PostgresStore) GetInvite(serverID int, userID string) (*models.ServerInvite, error) {
	var inv models.ServerInvite
	var permsJSON []byte
	// LEFT JOIN on the inviter: invited_by is nulled when that account is
	// deleted, and an inner join would drop the row - hiding a member from the
	// list while their access carried on working.
	query := `
		SELECT si.id, si.server_id, si.user_id, u.username, COALESCE(u.email, ''),
			si.permissions, COALESCE(si.invited_by::text, ''), COALESCE(inv_u.username, ''), si.created_at
		FROM server_invites si
		JOIN users u ON si.user_id = u.id
		LEFT JOIN users inv_u ON si.invited_by = inv_u.id
		WHERE si.server_id = $1 AND si.user_id = $2
	`
	err := s.db.QueryRow(query, serverID, userID).Scan(
		&inv.ID, &inv.ServerID, &inv.UserID, &inv.Username, &inv.Email,
		&permsJSON, &inv.InvitedBy, &inv.InviterName, &inv.CreatedAt)
	if err != nil {
		return nil, err
	}
	json.Unmarshal(permsJSON, &inv.Permissions)
	return &inv, nil
}

func (s *PostgresStore) ListInvitesByServer(serverID int) ([]models.ServerInvite, error) {
	// Same LEFT JOIN as GetInvite: a deleted inviter must not remove a live
	// member from the list.
	query := `
		SELECT si.id, si.server_id, si.user_id, u.username, COALESCE(u.email, ''),
			si.permissions, COALESCE(si.invited_by::text, ''), COALESCE(inv_u.username, ''), si.created_at
		FROM server_invites si
		JOIN users u ON si.user_id = u.id
		LEFT JOIN users inv_u ON si.invited_by = inv_u.id
		WHERE si.server_id = $1
		ORDER BY si.created_at ASC
	`
	rows, err := s.db.Query(query, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var invites []models.ServerInvite
	for rows.Next() {
		var inv models.ServerInvite
		var permsJSON []byte
		if err := rows.Scan(&inv.ID, &inv.ServerID, &inv.UserID, &inv.Username, &inv.Email,
			&permsJSON, &inv.InvitedBy, &inv.InviterName, &inv.CreatedAt); err != nil {
			continue
		}
		json.Unmarshal(permsJSON, &inv.Permissions)
		invites = append(invites, inv)
	}
	return invites, nil
}

func (s *PostgresStore) CountInvitesPerServer() (map[int]int, error) {
	rows, err := s.db.Query(`SELECT server_id, COUNT(*) FROM server_invites GROUP BY server_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[int]int)
	for rows.Next() {
		var sid, c int
		if err := rows.Scan(&sid, &c); err != nil {
			continue
		}
		counts[sid] = c
	}
	return counts, nil
}

func (s *PostgresStore) ListServersForUser(userID string, isAdmin bool) ([]models.Server, error) {
	serverCols := `s.id, s.uuid, s.name, n.name, u.username, s.port, s.status, COALESCE(s.desired_state, 'stopped'), s.game_image,
		s.is_fixed, COALESCE(s.active_sub_server, ''), s.created_at, s.owner_id,
		s.memory, COALESCE(s.cpu_limit, 0), s.node_id, COALESCE(s.extra_jvm_flags, ''), COALESCE(s.start_command, ''),
		COALESCE(s.installer_type, ''), COALESCE(s.minecraft_version, ''), COALESCE(s.build_number, ''),
		COALESCE(s.disk_limit, 0), COALESCE(s.server_type, 'game'), s.proxy_id,
		COALESCE(n.address, ''), COALESCE(s.host_port, 0), COALESCE(s.container_port, 25565),
		COALESCE(s.region, 'default'), COALESCE(n.status, 'offline'), n.last_seen_at`

	scanServer := func(srv *models.Server, rows *sql.Rows, role string, permsJSON []byte) {
		if role != "" {
			srv.Role = role
		}
		if (role == "invited" || role == "inherited") && len(permsJSON) > 0 {
			var perms models.TabPermissions
			json.Unmarshal(permsJSON, &perms)
			srv.Permissions = &perms
		}
	}

	if isAdmin {
		query := `SELECT ` + serverCols + ` FROM servers s JOIN nodes n ON s.node_id = n.id JOIN users u ON s.owner_id = u.id ORDER BY s.id ASC`
		rows, err := s.db.Query(query)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var servers []models.Server
		for rows.Next() {
			var srv models.Server
			if err := rows.Scan(&srv.ID, &srv.UUID, &srv.Name, &srv.NodeName, &srv.OwnerName,
				&srv.Port, &srv.Status, &srv.DesiredState, &srv.GameImage, &srv.IsFixed, &srv.ActiveSubServer,
				&srv.CreatedAt, &srv.OwnerID, &srv.Memory, &srv.CPULimit, &srv.NodeID,
				&srv.ExtraJvmFlags, &srv.StartCommand, &srv.InstallerType, &srv.MinecraftVersion, &srv.BuildNumber,
				&srv.DiskLimit, &srv.ServerType, &srv.ProxyID, &srv.NodeAddress, &srv.HostPort, &srv.ContainerPort,
				&srv.Region, &srv.NodeStatus, &srv.NodeLastSeenAt); err != nil {
				continue
			}
			srv.Role = "owner"
			if srv.OwnerID != userID {
				srv.Role = "admin"
			}
			servers = append(servers, srv)
		}
		return servers, nil
	}

	query := `
		SELECT ` + serverCols + `, 'owner' as role, NULL::jsonb as permissions
		FROM servers s JOIN nodes n ON s.node_id = n.id JOIN users u ON s.owner_id = u.id
		WHERE s.owner_id = $1
		UNION ALL
		SELECT ` + serverCols + `, 'invited' as role, si.permissions
		FROM server_invites si JOIN servers s ON si.server_id = s.id JOIN nodes n ON s.node_id = n.id JOIN users u ON s.owner_id = u.id
		WHERE si.user_id = $1
		UNION ALL
		SELECT ` + serverCols + `, 'inherited' as role, si.permissions
		FROM servers s JOIN servers proxy ON s.proxy_id = proxy.id
		JOIN server_invites si ON si.server_id = proxy.id AND si.user_id = $1
		JOIN nodes n ON s.node_id = n.id JOIN users u ON s.owner_id = u.id
		WHERE (si.permissions::jsonb->>'inherit')::boolean = true
			AND s.owner_id != $1
			AND NOT EXISTS (SELECT 1 FROM server_invites si2 WHERE si2.server_id = s.id AND si2.user_id = $1)
		ORDER BY id ASC
	`
	rows, err := s.db.Query(query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var servers []models.Server
	for rows.Next() {
		var srv models.Server
		var role string
		var permsJSON []byte
		if err := rows.Scan(&srv.ID, &srv.UUID, &srv.Name, &srv.NodeName, &srv.OwnerName,
			&srv.Port, &srv.Status, &srv.DesiredState, &srv.GameImage, &srv.IsFixed, &srv.ActiveSubServer,
			&srv.CreatedAt, &srv.OwnerID, &srv.Memory, &srv.CPULimit, &srv.NodeID,
			&srv.ExtraJvmFlags, &srv.StartCommand, &srv.InstallerType, &srv.MinecraftVersion, &srv.BuildNumber,
			&srv.DiskLimit, &srv.ServerType, &srv.ProxyID, &srv.NodeAddress, &srv.HostPort, &srv.ContainerPort,
			&srv.Region, &srv.NodeStatus, &srv.NodeLastSeenAt, &role, &permsJSON); err != nil {
			continue
		}
		scanServer(&srv, nil, role, permsJSON)
		servers = append(servers, srv)
	}
	return servers, nil
}

// ==========================================
// MODULES
// ==========================================

const moduleSelectCols = `id, name, type, icon, COALESCE(url, ''), is_enabled, is_system, position, COALESCE(access_role, 'all')`

func (s *PostgresStore) ListModules() ([]models.Module, error) {
	query := `SELECT ` + moduleSelectCols + ` FROM modules ORDER BY position ASC, id ASC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var modules []models.Module
	for rows.Next() {
		var m models.Module
		if err := rows.Scan(&m.ID, &m.Name, &m.Type, &m.Icon, &m.URL, &m.IsEnabled, &m.IsSystem, &m.Position, &m.AccessRole); err != nil {
			continue
		}
		modules = append(modules, m)
	}
	return modules, nil
}

func (s *PostgresStore) GetModuleByID(id int) (*models.Module, error) {
	var m models.Module
	err := s.db.QueryRow(
		`SELECT `+moduleSelectCols+` FROM modules WHERE id = $1`, id,
	).Scan(&m.ID, &m.Name, &m.Type, &m.Icon, &m.URL, &m.IsEnabled, &m.IsSystem, &m.Position, &m.AccessRole)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// SetModuleAccessRole gates a module to "admin" or "all".
// Servers is hard-protected — it must stay "all".
func (s *PostgresStore) SetModuleAccessRole(id int, role string) error {
	if role != "all" && role != "admin" {
		role = "all"
	}
	_, err := s.db.Exec(`UPDATE modules SET access_role = $1 WHERE id = $2 AND name <> 'Servers'`, role, id)
	return err
}

func (s *PostgresStore) CreateModule(m *models.Module) (int, error) {
	var id int
	// access_role defaults to "admin" — admins can flip newly-created modules
	// to "all" via the Modules tab if they want users to see them.
	role := m.AccessRole
	if role != "all" && role != "admin" {
		role = "admin"
	}
	query := `INSERT INTO modules (name, type, icon, url, is_enabled, is_system, position, access_role)
	          VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`
	err := s.db.QueryRow(query, m.Name, m.Type, m.Icon, m.URL, m.IsEnabled, m.IsSystem, m.Position, role).Scan(&id)
	return id, err
}

func (s *PostgresStore) DeleteModule(id int) error {
	_, err := s.db.Exec("DELETE FROM modules WHERE id = $1", id)
	return err
}

func (s *PostgresStore) UpdateModuleStatus(id int, isEnabled bool) error {
	_, err := s.db.Exec("UPDATE modules SET is_enabled = $1 WHERE id = $2", isEnabled, id)
	return err
}

func (s *PostgresStore) UpdateModulePosition(id int, position int) error {
	_, err := s.db.Exec("UPDATE modules SET position = $1 WHERE id = $2", position, id)
	return err
}

// ==========================================
// SETTINGS
// ==========================================

func (s *PostgresStore) GetSetting(key string) (string, error) {
	var value string
	err := s.db.QueryRow("SELECT value FROM settings WHERE key = $1", key).Scan(&value)
	if err != nil {
		return "", err
	}
	return s.decodeSettingValue(key, value), nil
}

func (s *PostgresStore) SetSetting(key, value string) error {
	stored, err := s.encodeSettingValue(key, value)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value
	`, key, stored)
	return err
}

// ConsumeOneShotJoin atomically flips node_join_mode from 'one-shot' to
// 'disabled' and reports whether THIS caller won the single slot. Row returned
// -> won (proceed); no row -> a concurrent enroll already consumed it (reject).
func (s *PostgresStore) ConsumeOneShotJoin() (bool, error) {
	var v string
	err := s.db.QueryRow(
		`UPDATE settings SET value = 'disabled'
		 WHERE key = 'node_join_mode' AND value = 'one-shot'
		 RETURNING value`).Scan(&v)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ==========================================
// STATS
// ==========================================

func (s *PostgresStore) InsertStatsBatch(stats []models.ServerStatRow) error {
	if len(stats) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO server_stats (time, server_uuid, cpu, cpu_limit, mem_used, mem_limit, players, max_players)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range stats {
		_, err := stmt.Exec(r.Time, r.ServerUUID, r.CPU, r.CPULimit, r.MemUsed, r.MemLimit, r.Players, r.MaxPlayers)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *PostgresStore) InsertGatewayBandwidthBatch(rows []models.GatewayBandwidthRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO gateway_bandwidth_stats (time, component, id, host, region, rx_bps, tx_bps, cap_mbit)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range rows {
		// rx_bps/tx_bps are bits/s; BIGINT is signed but throughput never
		// approaches 2^63, so the int64 cast is safe.
		if _, err := stmt.Exec(r.Time, r.Component, r.ID, r.Host, r.Region, int64(r.RxBps), int64(r.TxBps), r.CapMbit); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *PostgresStore) GetStatsHistory(serverUUID string, since time.Time) ([]models.ServerStatRow, error) {
	rows, err := s.db.Query(`SELECT time, server_uuid, cpu, cpu_limit, mem_used, mem_limit, players, max_players
		FROM server_stats WHERE server_uuid = $1 AND time >= $2 ORDER BY time ASC`, serverUUID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []models.ServerStatRow
	for rows.Next() {
		var r models.ServerStatRow
		if err := rows.Scan(&r.Time, &r.ServerUUID, &r.CPU, &r.CPULimit, &r.MemUsed, &r.MemLimit, &r.Players, &r.MaxPlayers); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func (s *PostgresStore) GetGatewayBandwidthHistory(since time.Time, component, host string) ([]models.GatewayBandwidthRow, error) {
	rows, err := s.db.Query(`SELECT time, component, id, host, region, rx_bps, tx_bps, cap_mbit
		FROM gateway_bandwidth_stats
		WHERE time >= $1 AND component = COALESCE(NULLIF($2, ''), component) AND host = COALESCE(NULLIF($3, ''), host)
		ORDER BY time ASC`, since, component, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []models.GatewayBandwidthRow
	for rows.Next() {
		var r models.GatewayBandwidthRow
		// rx_bps/tx_bps are BIGINT (signed) but hold unsigned bits/s; scan as
		// int64 then cast back, mirroring the InsertGatewayBandwidthBatch cast.
		var rx, tx int64
		if err := rows.Scan(&r.Time, &r.Component, &r.ID, &r.Host, &r.Region, &rx, &tx, &r.CapMbit); err != nil {
			return nil, err
		}
		r.RxBps = uint64(rx)
		r.TxBps = uint64(tx)
		result = append(result, r)
	}
	return result, rows.Err()
}

// ==========================================
// GATEWAY ROUTE LIMITS (still managed by Core, not Hub)
// ==========================================

// normalizeRouteLimit reads a NEGATIVE stored cap as no cap.
//
// A negative is not a value in the limits convention (nil = no cap, 0 = none,
// n = the cap). It is the OLD spelling of "unlimited", from before the column
// was nullable, and rows written back then are still in this table.
//
// Left alone it is worse than merely stale. Every cap is tested as
// "count >= limit", which a negative limit satisfies before the first route
// exists - so the one value that used to mean UNLIMITED now blocks everything,
// and says so in an operator's own words: "You have used all -1 addresses on
// our domains". Read as nil it keeps exactly the meaning it was written with.
func normalizeRouteLimit(l *models.GatewayRouteLimit) *models.GatewayRouteLimit {
	if l != nil && l.MaxRoutes != nil && *l.MaxRoutes < 0 {
		l.MaxRoutes = nil
	}
	return l
}

func (s *PostgresStore) GetGatewayRouteLimit(scope string) (*models.GatewayRouteLimit, error) {
	var l models.GatewayRouteLimit
	err := s.db.QueryRow(`SELECT id, scope, max_routes FROM gateway_route_limits WHERE scope = $1`, scope).
		Scan(&l.ID, &l.Scope, &l.MaxRoutes)
	if err != nil {
		return nil, err
	}
	return normalizeRouteLimit(&l), nil
}

// SetGatewayRouteLimit writes one scope's cap. A nil max stores NULL, which is
// "this scope is set, and it sets no cap" - distinct from deleting the row,
// which hands the question to the next scope down.
//
// A negative max is written as NULL for the same reason it is READ as nil (see
// normalizeRouteLimit): it is the old spelling of unlimited, and storing it
// verbatim would keep recreating the row that has to be healed on every read.
func (s *PostgresStore) SetGatewayRouteLimit(scope string, max *int) error {
	if max != nil && *max < 0 {
		max = nil
	}
	_, err := s.db.Exec(`
		INSERT INTO gateway_route_limits (scope, max_routes) VALUES ($1, $2)
		ON CONFLICT (scope) DO UPDATE SET max_routes = EXCLUDED.max_routes
	`, scope, max)
	return err
}

func (s *PostgresStore) ListGatewayRouteLimits() ([]models.GatewayRouteLimit, error) {
	rows, err := s.db.Query(`SELECT id, scope, max_routes FROM gateway_route_limits ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var limits []models.GatewayRouteLimit
	for rows.Next() {
		var l models.GatewayRouteLimit
		if err := rows.Scan(&l.ID, &l.Scope, &l.MaxRoutes); err != nil {
			continue
		}
		limits = append(limits, *normalizeRouteLimit(&l))
	}
	return limits, nil
}

func (s *PostgresStore) DeleteGatewayRouteLimit(scope string) error {
	_, err := s.db.Exec("DELETE FROM gateway_route_limits WHERE scope = $1", scope)
	return err
}

// ==========================================
// LIBRARY DISABLED PATHS
// ==========================================

func (s *PostgresStore) ListDisabledLibraryPaths() ([]string, error) {
	rows, err := s.db.Query(`SELECT path FROM library_disabled`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		// This list is a DENYLIST, so a dropped row silently un-blocks a path.
		// Skipping on a scan error (and ignoring rows.Err below) turned a
		// truncated read into "nothing is disabled" - fail open. Same reasoning
		// as ListLinkKitsForACLTeardown, which already returns on both.
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	// nil rather than the partial slice on purpose: a caller that ignores the
	// error must not end up with a denylist that is silently missing entries.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return paths, nil
}

// SetLibraryPathDisabled inserts the path when disabled=true and removes it
// when disabled=false. Idempotent in either direction.
func (s *PostgresStore) SetLibraryPathDisabled(path string, disabled bool) error {
	if disabled {
		_, err := s.db.Exec(
			`INSERT INTO library_disabled (path) VALUES ($1) ON CONFLICT (path) DO NOTHING`,
			path,
		)
		return err
	}
	_, err := s.db.Exec(`DELETE FROM library_disabled WHERE path = $1`, path)
	return err
}

// ==========================================
// SFTP
// ==========================================

// GetSFTPAccessByNode returns every (user, server) pair the grant tables
// connect to a server on this node: owners, per-server grants, and account-wide
// grants. It answers "who could plausibly have SFTP here", not "who may" -
// SFTPSyncService resolves each non-owner row against sftp.access before
// publishing anything.
//
// The third branch is not cosmetic. server_invites doubles as the grant table,
// and an account-wide grant is a row with server_id IS NULL keyed on
// owner_user_id, so joining on si.server_id = s.id skipped it entirely: someone
// granted access to a whole account could use every panel route on those
// servers and had no SFTP at all.
func (s *PostgresStore) GetSFTPAccessByNode(nodeID int) ([]SFTPAccess, error) {
	query := `
		SELECT s.uuid, s.name, s.id, u.id, u.username, TRUE
		FROM servers s
		JOIN users u ON s.owner_id = u.id
		WHERE s.node_id = $1
		UNION
		SELECT s.uuid, s.name, s.id, u.id, u.username, FALSE
		FROM servers s
		JOIN server_invites si ON si.server_id = s.id
		JOIN users u ON si.user_id = u.id
		WHERE s.node_id = $1
		UNION
		SELECT s.uuid, s.name, s.id, u.id, u.username, FALSE
		FROM servers s
		JOIN server_invites si ON si.server_id IS NULL AND si.owner_user_id = s.owner_id
		JOIN users u ON si.user_id = u.id
		WHERE s.node_id = $1
		ORDER BY 1, 5
	`
	rows, err := s.db.Query(query, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SFTPAccess
	for rows.Next() {
		var a SFTPAccess
		if err := rows.Scan(&a.ServerUUID, &a.ServerName, &a.ServerID, &a.UserID, &a.Username, &a.IsOwner); err != nil {
			continue
		}
		result = append(result, a)
	}
	// An access list that silently loses entries denies a user their own server.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// ==========================================
// REGIONS
// ==========================================

func scanRegion(scan func(dest ...interface{}) error) (*models.Region, error) {
	var (
		r         models.Region
		color     sql.NullString
		createdAt sql.NullTime
	)
	err := scan(&r.ID, &r.DisplayName, &r.Enabled, &color, &createdAt)
	if err != nil {
		return nil, err
	}
	if color.Valid {
		r.Color = color.String
	}
	if createdAt.Valid {
		r.CreatedAt = createdAt.Time
	}
	return &r, nil
}

func (s *PostgresStore) ListRegions(includeDisabled bool) ([]models.Region, error) {
	query := `SELECT id, display_name, enabled, color, created_at FROM regions`
	if !includeDisabled {
		query += ` WHERE enabled = TRUE`
	}
	query += ` ORDER BY id ASC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Region
	for rows.Next() {
		r, err := scanRegion(rows.Scan)
		if err != nil {
			continue
		}
		out = append(out, *r)
	}
	return out, nil
}

func (s *PostgresStore) GetRegion(id string) (*models.Region, error) {
	query := `SELECT id, display_name, enabled, color, created_at FROM regions WHERE id = $1`
	return scanRegion(s.db.QueryRow(query, id).Scan)
}

func (s *PostgresStore) CreateRegion(r *models.Region) error {
	_, err := s.db.Exec(
		`INSERT INTO regions (id, display_name, enabled, color) VALUES ($1, $2, $3, NULLIF($4, ''))`,
		r.ID, r.DisplayName, r.Enabled, r.Color,
	)
	return err
}

func (s *PostgresStore) UpdateRegion(r *models.Region) error {
	_, err := s.db.Exec(
		`UPDATE regions SET display_name = $1, enabled = $2, color = NULLIF($3, '') WHERE id = $4`,
		r.DisplayName, r.Enabled, r.Color, r.ID,
	)
	return err
}

func (s *PostgresStore) DeleteRegion(id string) error {
	_, err := s.db.Exec(`DELETE FROM regions WHERE id = $1`, id)
	return err
}

func (s *PostgresStore) CountServersInRegion(regionID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM servers WHERE region = $1`, regionID).Scan(&n)
	return n, err
}

func (s *PostgresStore) CountNodesInRegion(regionID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE region = $1`, regionID).Scan(&n)
	return n, err
}

// ==========================================
// USER <-> REGION M:N
// ==========================================

func (s *PostgresStore) GetUserRegionIDs(userID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT region_id FROM user_regions WHERE user_id = $1 ORDER BY region_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var rid string
		if err := rows.Scan(&rid); err == nil {
			out = append(out, rid)
		}
	}
	// A short read here silently narrows a user's region access.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// SetUserRegions replaces the user's region set in one transaction. When
// allAccess is true the user_regions rows are wiped — all-regions trumps
// the explicit list. When false, regionIDs becomes the new authoritative set.
func (s *PostgresStore) SetUserRegions(userID string, allAccess bool, regionIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE users SET all_regions_access = $1 WHERE id = $2`, allAccess, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM user_regions WHERE user_id = $1`, userID); err != nil {
		return err
	}
	if !allAccess {
		for _, rid := range regionIDs {
			if _, err := tx.Exec(`INSERT INTO user_regions (user_id, region_id) VALUES ($1, $2)`, userID, rid); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *PostgresStore) SetUserAllRegionsAccess(userID string, allAccess bool) error {
	_, err := s.db.Exec(`UPDATE users SET all_regions_access = $1 WHERE id = $2`, allAccess, userID)
	return err
}

func (s *PostgresStore) GetUserAllRegionsAccess(userID string) (bool, error) {
	var b bool
	err := s.db.QueryRow(`SELECT COALESCE(all_regions_access, FALSE) FROM users WHERE id = $1`, userID).Scan(&b)
	return b, err
}

// ==========================================
// IDENTITY AUDIT (append-only)
// ==========================================

func (s *PostgresStore) InsertAuditIdentity(ev *models.AuditEventIdentity) error {
	var metaJSON []byte
	if ev.Metadata != nil {
		metaJSON, _ = json.Marshal(ev.Metadata)
	}
	_, err := s.db.Exec(
		`INSERT INTO audit_events_identity
			(event_type, actor_user_id, target_user_id, metadata, ip_address, user_agent)
		 VALUES ($1, $2, $3, NULLIF($4, '')::jsonb, NULLIF($5, '')::inet, NULLIF($6, ''))`,
		ev.EventType, ev.ActorUserID, ev.TargetUserID,
		string(metaJSON), ev.IPAddress, ev.UserAgent,
	)
	return err
}

func (s *PostgresStore) ListAuditIdentity(targetUserID *string, eventType string, limit int) ([]models.AuditEventIdentity, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	query := `SELECT id, event_type, actor_user_id, target_user_id, metadata, ip_address::text, user_agent, created_at
		FROM audit_events_identity WHERE 1=1`
	args := []interface{}{}
	if targetUserID != nil {
		args = append(args, *targetUserID)
		query += fmt.Sprintf(" AND target_user_id = $%d", len(args))
	}
	if eventType != "" {
		args = append(args, eventType)
		query += fmt.Sprintf(" AND event_type = $%d", len(args))
	}
	args = append(args, limit)
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", len(args))

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.AuditEventIdentity
	for rows.Next() {
		var ev models.AuditEventIdentity
		var (
			actor, target sql.NullString
			metaJSON      []byte
			ipAddr, ua    sql.NullString
			createdAt     sql.NullTime
		)
		if err := rows.Scan(&ev.ID, &ev.EventType, &actor, &target, &metaJSON, &ipAddr, &ua, &createdAt); err != nil {
			continue
		}
		if actor.Valid {
			n := actor.String
			ev.ActorUserID = &n
		}
		if target.Valid {
			n := target.String
			ev.TargetUserID = &n
		}
		if len(metaJSON) > 0 {
			_ = json.Unmarshal(metaJSON, &ev.Metadata)
		}
		if ipAddr.Valid {
			ev.IPAddress = ipAddr.String
		}
		if ua.Valid {
			ev.UserAgent = ua.String
		}
		if createdAt.Valid {
			ev.CreatedAt = createdAt.Time
		}
		out = append(out, ev)
	}
	return out, nil
}

// SetSettingBy mirrors SetSetting but records the user who changed the value.
// Existing call sites use SetSetting (updated_by stays NULL); audit-aware
// new flows call this variant. updated_at is bumped to NOW() on every write.
func (s *PostgresStore) SetSettingBy(key, value string, updatedBy string) error {
	stored, err := s.encodeSettingValue(key, value)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO settings (key, value, updated_at, updated_by)
		VALUES ($1, $2, NOW(), $3)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW(), updated_by = EXCLUDED.updated_by
	`, key, stored, updatedBy)
	return err
}

// ==========================================
// EMAIL VERIFICATION + LOGIN TRACKING
// ==========================================

func (s *PostgresStore) GetUserByEmail(email string) (*models.User, error) {
	// Case-insensitive lookup — emails normalized to lowercase at write
	// time but be defensive on reads in case older rows escaped that.
	query := `SELECT ` + userSelectCols + ` FROM users WHERE LOWER(email) = LOWER($1)`
	return scanUser(s.db.QueryRow(query, email).Scan)
}

func (s *PostgresStore) GetUserByEmailVerificationToken(token string) (*models.User, error) {
	query := `SELECT ` + userSelectCols + ` FROM users WHERE email_verification_token = $1 AND email_verification_token IS NOT NULL`
	return scanUser(s.db.QueryRow(query, hashAuthToken(token)).Scan)
}

// SetEmailVerificationToken stores a freshly generated token + sent timestamp.
// Pass an empty token to clear (e.g. after a manual admin override).
func (s *PostgresStore) SetEmailVerificationToken(userID string, token string) error {
	if token == "" {
		_, err := s.db.Exec(
			`UPDATE users SET email_verification_token = NULL, email_verification_sent_at = NULL WHERE id = $1`,
			userID,
		)
		return err
	}
	_, err := s.db.Exec(
		`UPDATE users SET email_verification_token = $1, email_verification_sent_at = NOW() WHERE id = $2`,
		hashAuthToken(token), userID,
	)
	return err
}

// MarkEmailVerified stamps the verification time and clears the token in one
// statement so the row can't be replayed against a stale token.
func (s *PostgresStore) MarkEmailVerified(userID string) error {
	_, err := s.db.Exec(
		`UPDATE users SET email_verified_at = NOW(), email_verification_token = NULL, email_verification_sent_at = NULL WHERE id = $1`,
		userID,
	)
	return err
}

// SetUserEmail changes an account's address and un-verifies it in the same
// statement.
//
// The two belong together. An admin typing a new address has not proved the
// account can receive mail there, so leaving email_verified_at in place would
// hand out a verified badge for an address nobody has answered - and password
// reset, the one recovery path that exists while security questions are off,
// aims at exactly that address.
func (s *PostgresStore) SetUserEmail(userID, email string) error {
	_, err := s.db.Exec(
		`UPDATE users SET email = $2, email_verified_at = NULL,
		   email_verification_token = NULL, email_verification_sent_at = NULL
		 WHERE id = $1`,
		userID, email,
	)
	return err
}

func (s *PostgresStore) UpdateLastLoginAt(userID string) error {
	_, err := s.db.Exec(`UPDATE users SET last_login_at = NOW() WHERE id = $1`, userID)
	return err
}

// ==========================================
// PASSWORD RESET
// ==========================================

// GetUserByPasswordResetToken returns the user row whose reset token matches
// AND hasn't expired yet. Expiry is enforced at the SQL level so a leaked
// token can't be replayed after the configured TTL even if the handler
// somehow forgets to check.
func (s *PostgresStore) GetUserByPasswordResetToken(token string) (*models.User, error) {
	query := `SELECT ` + userSelectCols + ` FROM users
		WHERE password_reset_token = $1
		  AND password_reset_token IS NOT NULL
		  AND (password_reset_expires_at IS NULL OR password_reset_expires_at > NOW())`
	return scanUser(s.db.QueryRow(query, hashAuthToken(token)).Scan)
}

// SetPasswordResetToken issues a reset token for userID, but only when any
// token already on the row is no longer fresh, and reports whether it issued.
//
// The freshness test is expressed against password_reset_expires_at rather than
// a send timestamp because a reset token's expiry IS its send time plus the
// policy TTL, so the column already carries the information and no new one is
// needed. issueWhenExpiryAtOrBefore is that boundary: pass expiresAt minus the
// cooldown and the comparison reduces to "the previous send was at least a
// cooldown ago". (Changing the TTL policy between two sends skews the window by
// the difference; the effect is bounded by that and self-corrects on the next
// send.)
//
// Doing it in the UPDATE's WHERE rather than as a read-then-write also makes it
// atomic, so two simultaneous requests cannot both decide they are due.
func (s *PostgresStore) SetPasswordResetToken(userID string, token string, expiresAt, issueWhenExpiryAtOrBefore time.Time) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE users SET password_reset_token = $1, password_reset_expires_at = $2
		 WHERE id = $3
		   AND (password_reset_expires_at IS NULL OR password_reset_expires_at <= $4)`,
		hashAuthToken(token), expiresAt, userID, issueWhenExpiryAtOrBefore,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *PostgresStore) ClearPasswordResetToken(userID string) error {
	_, err := s.db.Exec(
		`UPDATE users SET password_reset_token = NULL, password_reset_expires_at = NULL WHERE id = $1`,
		userID,
	)
	return err
}

// ==========================================
// SECURITY QUESTIONS
// ==========================================

// securityQAStored mirrors the on-disk shape: one entry per question, hashed
// answer kept here, plaintext question text kept inline so admin edits to
// the question pool can't desync existing users from their original wording.
type securityQAStored struct {
	Question   string `json:"question"`
	AnswerHash string `json:"answer_hash"`
}

func (s *PostgresStore) GetUserSecurityQuestions(userID string) ([]string, error) {
	var raw string
	err := s.db.QueryRow(`SELECT COALESCE(security_questions::text, '[]') FROM users WHERE id = $1`, userID).Scan(&raw)
	if err != nil {
		return nil, err
	}
	var rows []securityQAStored
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return []string{}, nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Question)
	}
	return out, nil
}

func (s *PostgresStore) GetUserSecurityQuestionsRaw(userID string) (string, error) {
	var raw string
	err := s.db.QueryRow(`SELECT COALESCE(security_questions::text, '[]') FROM users WHERE id = $1`, userID).Scan(&raw)
	return raw, err
}

// CountUsersMissingSecurityQuestions returns how many accounts have no security
// questions on file, and how many accounts there are.
//
// "Require during password reset" skips any user who has none stored, so the
// policy does not lock out accounts that predate it - a deliberate choice the
// settings screen already spells out. What it did NOT say is the SCALE, and the
// moment the toggle is flipped that is every existing account: "some users skip
// this step" and "37 of 40 accounts skip this step" are the same sentence and
// very different facts. This is the number that tells them apart.
//
// Deleted accounts are excluded from both figures - they cannot reset anything,
// so counting them would only make the ratio look better than it is.
func (s *PostgresStore) CountUsersMissingSecurityQuestions() (missing int, total int, err error) {
	err = s.db.QueryRow(`
		SELECT
			COUNT(*) FILTER (WHERE security_questions IS NULL OR security_questions::text IN ('[]', 'null')),
			COUNT(*)
		FROM users
		WHERE COALESCE(deletion_status, 'active') <> 'deleted'`).Scan(&missing, &total)
	return missing, total, err
}

func (s *PostgresStore) SetUserSecurityQuestions(userID string, qaJSON string) error {
	if qaJSON == "" {
		qaJSON = "[]"
	}
	_, err := s.db.Exec(
		`UPDATE users SET security_questions = $1::jsonb WHERE id = $2`,
		qaJSON, userID,
	)
	return err
}

// ==========================================
// AUTO-DELETE INACTIVE USERS
// ==========================================

func (s *PostgresStore) ListInactiveCandidates(idleSince time.Time) ([]InactiveCandidate, error) {
	// "history" today = owns or is invited to any server. Tickets are
	// intentionally NOT counted as history — a stale support ticket should not
	// block auto-delete.
	query := `
		SELECT u.id, u.username, COALESCE(u.email, ''), u.last_login_at,
		       EXISTS (SELECT 1 FROM servers       WHERE owner_id = u.id)
		    OR EXISTS (SELECT 1 FROM server_invites WHERE user_id  = u.id)
		    AS has_history
		FROM users u
		WHERE u.is_admin = FALSE
		  AND COALESCE(u.deletion_status, 'active') = 'active'
		  AND COALESCE(u.last_login_at, u.created_at) < $1
	`
	rows, err := s.db.Query(query, idleSince)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InactiveCandidate
	for rows.Next() {
		var c InactiveCandidate
		var lastLogin sql.NullTime
		if err := rows.Scan(&c.ID, &c.Username, &c.Email, &lastLogin, &c.HasHistory); err != nil {
			continue
		}
		if lastLogin.Valid {
			t := lastLogin.Time
			c.LastLogin = &t
		}
		out = append(out, c)
	}
	// The retention job acts on this list; a short read silently spares some
	// accounts and warns the wrong set of users.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PostgresStore) MarkUserPendingDeletion(userID string, scheduledAt time.Time) error {
	_, err := s.db.Exec(
		`UPDATE users
		    SET deletion_status = 'pending_deletion',
		        deletion_warning_sent_at = NOW(),
		        deletion_scheduled_at = $1
		  WHERE id = $2`,
		scheduledAt, userID,
	)
	return err
}

func (s *PostgresStore) ListUsersDueForDeletion(now time.Time) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT id FROM users
		  WHERE deletion_status = 'pending_deletion'
		    AND deletion_scheduled_at IS NOT NULL
		    AND deletion_scheduled_at <= $1`,
		now,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	// Same reason as ListInactiveCandidates: this drives deletions.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

func (s *PostgresStore) CancelUserDeletion(userID string) error {
	_, err := s.db.Exec(
		`UPDATE users
		    SET deletion_status = 'active',
		        deletion_warning_sent_at = NULL,
		        deletion_scheduled_at = NULL
		  WHERE id = $1`,
		userID,
	)
	return err
}

func (s *PostgresStore) AnonymizeUser(userID string) error {
	// Wipe PII while keeping the row. The username becomes a sentinel that
	// won't collide with anything a human would pick. Password gets a
	// random-bytes bcrypt that nobody knows — login becomes impossible
	// without a separate admin-reset.
	// Note: keep is_admin as-is so anonymized accounts can't be re-elevated
	// just by no-longer-being-an-admin; the job only ever targets non-admins
	// in the first place.
	tag := fmt.Sprintf("<deleted_user_%s>", userID)
	_, err := s.db.Exec(`
		UPDATE users
		   SET username = $1,
		       email = NULL,
		       password = '!disabled!' || md5(random()::text),
		       minecraft_username = NULL,
		       totp_secret = '',
		       totp_backup_codes = '[]'::jsonb,
		       is_2fa_enabled = FALSE,
		       security_questions = '[]'::jsonb,
		       email_verified_at = NULL,
		       email_verification_token = NULL,
		       email_verification_sent_at = NULL,
		       password_reset_token = NULL,
		       password_reset_expires_at = NULL,
		       deletion_status = 'anonymized',
		       deletion_scheduled_at = NULL,
		       deletion_warning_sent_at = NULL
		 WHERE id = $2
	`, tag, userID)
	return err
}

// ==========================================
// PHASE 17 — Setup wizard
// ==========================================

// CountAdmins returns the number of users currently flagged as admin. Used by
// the first-run setup wizard + the Lost-Admin recovery flow to detect "no
// admin present" state.
func (s *PostgresStore) CountAdmins() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin = true`).Scan(&n)
	return n, err
}

// CreateFirstAdmin atomically inserts the first admin via a guarded CTE so
// N Cores can race without producing duplicate admins. The guard fires only
// when no admin exists yet (covers both Fresh-Install and Lost-Admin modes).
// Recovery is now enforced at the handler layer by ADMIN_SECRET, not a DB
// token, so the CTE has no token condition and no recoveryToken parameter.
//
// Returns ErrSetupAlreadyComplete when an admin already exists at insert time
// (the guarded CTE then inserts zero rows -> sql.ErrNoRows).
func (s *PostgresStore) CreateFirstAdmin(username, passwordHash, totpSecret string) (*models.User, error) {
	const q = `
		WITH guard AS (
			SELECT 1 WHERE NOT EXISTS (SELECT 1 FROM users WHERE is_admin = true)
		)
		INSERT INTO users (id, username, password, is_admin, role, totp_secret, created_at)
		SELECT gen_random_uuid(), $1, $2, true, 'admin', $3, NOW()
		FROM guard
		RETURNING id, username, is_admin, role, totp_secret, created_at
	`
	var u models.User
	err := s.db.QueryRow(q, username, passwordHash, totpSecret).
		Scan(&u.ID, &u.Username, &u.IsAdmin, &u.Role, &u.TOTPSecret, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrSetupAlreadyComplete
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// CreateAdditionalAdmin unconditionally inserts a new admin row into the real
// password column. It backs the break-glass path (a matching ADMIN_SECRET) when
// an admin already exists; the caller has already authorized the request. The
// username UNIQUE constraint is the only guard - a collision maps to
// ErrUsernameTaken so the handler answers 409 instead of a raw 500.
func (s *PostgresStore) CreateAdditionalAdmin(username, passwordHash, totpSecret string) (*models.User, error) {
	const q = `
		INSERT INTO users (id, username, password, is_admin, role, totp_secret, created_at)
		VALUES (gen_random_uuid(), $1, $2, true, 'admin', $3, NOW())
		RETURNING id, username, is_admin, role, totp_secret, created_at
	`
	var u models.User
	err := s.db.QueryRow(q, username, passwordHash, totpSecret).
		Scan(&u.ID, &u.Username, &u.IsAdmin, &u.Role, &u.TOTPSecret, &u.CreatedAt)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			return nil, ErrUsernameTaken
		}
		return nil, err
	}
	return &u, nil
}
