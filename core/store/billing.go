package store

import (
	"database/sql"
	"fmt"
	"time"

	"dylaris-core/models"
)

// UserBilling is one tenant's billing/lifecycle state. A missing row means
// "active with no overrides" — the default for every user, so reads never error
// on not-found. Retention overrides are specs like "3d"/"2w"/"3m"; empty = use
// the platform default.
type UserBilling struct {
	UserID        string     `json:"userId"`
	Status        string     `json:"status"` // active | past_due | suspended
	GraceUntil    *time.Time `json:"graceUntil,omitempty"`
	SuspendedAt   *time.Time `json:"suspendedAt,omitempty"`
	GracePeriod   string     `json:"gracePeriod,omitempty"`
	R2Retention   string     `json:"r2Retention,omitempty"`
	NodeRetention string     `json:"nodeRetention,omitempty"`
	R2QuotaGB     *int64     `json:"r2QuotaGb,omitempty"`
	// Per-user LIMIT overrides, on the platform limit convention: nil = no cap,
	// 0 = none, n = the cap. See services.Limits.
	//
	// These used to read "nil = use the plan value, 0 = unlimited". BOTH halves
	// are now wrong - plans are gone, and a stored 0 is a real cap of none. The
	// comment outlived the code it described, which is how a reader ends up
	// writing the old semantics back in.
	MaxNodes          *int64 `json:"maxNodes,omitempty"`
	MaxLinks          *int64 `json:"maxLinks,omitempty"`
	TrafficEdgeGB     *int64 `json:"trafficEdgeGb,omitempty"`
	TrafficRelayGB    *int64 `json:"trafficRelayGb,omitempty"`
	TrafficCombinedGB *int64 `json:"trafficCombinedGb,omitempty"`
	// Admin-granted entitlement, independent of any plan or store subscription:
	// "" | "byon" | "route_only" | "both", valid until ManualEntitlementExpiresAt.
	// Resolved by services.EffectiveEntitlement, which ignores it once expired -
	// so a stale row is inert rather than quietly still granting.
	ManualEntitlement          string     `json:"manualEntitlement,omitempty"`
	ManualEntitlementExpiresAt *time.Time `json:"manualEntitlementExpiresAt,omitempty"`
	// Per-kind expiry, and the ACTUAL truth about what is granted. The pair
	// above is the legacy shape: one string, one clock, so the two kinds could
	// not be held with different deadlines and granting one replaced the other.
	// ManualEntitlement is still written in step with these, for the partial
	// index it carries and for an older Core reading the same row.
	//
	// nil means that kind is not granted. A past time means it was and has run
	// out, which reads as not granted and is deliberately not cleaned up: the
	// admin screen shows when it lapsed.
	ManualByonExpiresAt        *time.Time `json:"manualByonExpiresAt,omitempty"`
	ManualRouteExpiresAt       *time.Time `json:"manualRouteExpiresAt,omitempty"`
	ManualEntitlementGrantedAt *time.Time `json:"manualEntitlementGrantedAt,omitempty"`
	ManualEntitlementGrantedBy string     `json:"manualEntitlementGrantedBy,omitempty"`
	// OverLimitSince is when the tenant was first seen holding more than they
	// bought - normally after a downgrade. Nil means they are within their caps.
	// Separate from GraceUntil on purpose: being over a cap and being behind on
	// payment are different problems with different clocks.
	OverLimitSince *time.Time `json:"overLimitSince,omitempty"`
	// TrafficCeilingGB is where the tenant's free traffic ends, in DECIMAL GB
	// (10^9), and TrafficBillingEnabled whether they have agreed to be charged
	// past it. Both are told to us by the store and are never decided here - Core
	// only reads them to warn the tenant before the store stops them. Zero /
	// false is what a self-hosted install with no store reads, and shows nothing.
	TrafficCeilingGB      int64 `json:"trafficCeilingGb,omitempty"`
	TrafficBillingEnabled bool  `json:"trafficBillingEnabled,omitempty"`
	// BackupBillingEnabled is the tenant's separate consent to be charged for
	// backup STORAGE beyond what their purchase includes. Its own flag rather
	// than riding on the traffic one: agreeing to pay for a terabyte of player
	// traffic is not agreeing to pay for stored backups, and a single switch
	// would enrol somebody in a charge they never saw.
	//
	// Told to us by the store, like the traffic flag, and never decided here.
	BackupBillingEnabled bool      `json:"backupBillingEnabled,omitempty"`
	UpdatedAt            time.Time `json:"updatedAt"`
}

// userBillingCols is the column list (and order) shared by every UserBilling
// SELECT so scanUserBilling can stay in lockstep.
const userBillingCols = `user_id, status, grace_until, suspended_at, grace_period, r2_retention, node_retention, r2_quota_gb, max_nodes, max_links, traffic_edge_gb, traffic_relay_gb, traffic_combined_gb, manual_entitlement, manual_entitlement_expires_at, manual_entitlement_byon_expires_at, manual_entitlement_route_expires_at, manual_entitlement_granted_at, manual_entitlement_granted_by, overlimit_since, COALESCE(traffic_ceiling_gb, 0), traffic_billing_enabled, COALESCE(backup_billing_enabled, FALSE), updated_at`

// SetUserOverLimitSince stamps (or clears, with nil) when a tenant was first seen
// over a purchased cap. Touches ONLY that column: the row also carries the
// payment status and the per-user overrides, and a sweep must not rewrite either.
func (s *PostgresStore) SetUserOverLimitSince(userID string, at *time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO user_billing (user_id, overlimit_since, updated_at)
		VALUES ($1,$2,NOW())
		ON CONFLICT (user_id) DO UPDATE SET overlimit_since = $2, updated_at = NOW()`,
		userID, at)
	return err
}

// ListUserBilling returns every billing row. Bounded by design: a row exists only
// for a tenant an admin or a purchase has touched, never for the whole user table.
func (s *PostgresStore) ListUserBilling() ([]UserBilling, error) {
	rows, err := s.db.Query(`SELECT ` + userBillingCols + ` FROM user_billing`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserBilling
	for rows.Next() {
		b, err := scanUserBilling(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

func scanUserBilling(row interface {
	Scan(dest ...any) error
}) (*UserBilling, error) {
	var b UserBilling
	var grace, susp sql.NullTime
	var gp, r2, nr sql.NullString
	var quota, maxNodes, maxLinks, tEdge, tRelay, tComb sql.NullInt64
	var meKind, meBy sql.NullString
	var meExp, meByon, meRoute, meAt, overLimit sql.NullTime
	if err := row.Scan(&b.UserID, &b.Status, &grace, &susp, &gp, &r2, &nr, &quota,
		&maxNodes, &maxLinks, &tEdge, &tRelay, &tComb,
		&meKind, &meExp, &meByon, &meRoute, &meAt, &meBy, &overLimit,
		&b.TrafficCeilingGB, &b.TrafficBillingEnabled, &b.BackupBillingEnabled, &b.UpdatedAt); err != nil {
		return nil, err
	}
	if overLimit.Valid {
		b.OverLimitSince = &overLimit.Time
	}
	b.ManualEntitlement = meKind.String
	if meExp.Valid {
		b.ManualEntitlementExpiresAt = &meExp.Time
	}
	if meByon.Valid {
		b.ManualByonExpiresAt = &meByon.Time
	}
	if meRoute.Valid {
		b.ManualRouteExpiresAt = &meRoute.Time
	}
	if meAt.Valid {
		b.ManualEntitlementGrantedAt = &meAt.Time
	}
	b.ManualEntitlementGrantedBy = meBy.String
	if grace.Valid {
		b.GraceUntil = &grace.Time
	}
	if susp.Valid {
		b.SuspendedAt = &susp.Time
	}
	b.GracePeriod = gp.String
	b.R2Retention = r2.String
	b.NodeRetention = nr.String
	if quota.Valid {
		b.R2QuotaGB = &quota.Int64
	}
	if maxNodes.Valid {
		b.MaxNodes = &maxNodes.Int64
	}
	if maxLinks.Valid {
		b.MaxLinks = &maxLinks.Int64
	}
	if tEdge.Valid {
		b.TrafficEdgeGB = &tEdge.Int64
	}
	if tRelay.Valid {
		b.TrafficRelayGB = &tRelay.Int64
	}
	if tComb.Valid {
		b.TrafficCombinedGB = &tComb.Int64
	}
	return &b, nil
}

// GetUserBilling returns a tenant's billing row, or a zero-value active row when
// none exists.
func (s *PostgresStore) GetUserBilling(userID string) (*UserBilling, error) {
	row := s.db.QueryRow(`SELECT `+userBillingCols+` FROM user_billing WHERE user_id = $1`, userID)
	b, err := scanUserBilling(row)
	if err == sql.ErrNoRows {
		return &UserBilling{UserID: userID, Status: "active"}, nil
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

// SetUserBillingStatus upserts the status + lifecycle timestamps, leaving the
// per-user retention overrides untouched.
func (s *PostgresStore) SetUserBillingStatus(userID, status string, graceUntil, suspendedAt *time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO user_billing (user_id, status, grace_until, suspended_at, updated_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (user_id) DO UPDATE SET
			status       = EXCLUDED.status,
			grace_until  = EXCLUDED.grace_until,
			suspended_at = EXCLUDED.suspended_at,
			updated_at   = NOW()`,
		userID, status, graceUntil, suspendedAt)
	return err
}

// SetUserBillingOverrides upserts the per-user retention overrides, leaving the
// status/timestamps untouched. An empty spec clears the override (NULL = default).
// r2QuotaGB is a pointer so the caller can distinguish "use platform default"
// (nil -> NULL) from an explicit 0, which is a real cap of NONE under the
// platform limit convention. It read "unlimited for this user" here, which is
// what the convention inverted.
func (s *PostgresStore) SetUserBillingOverrides(userID, gracePeriod, r2Retention, nodeRetention string, r2QuotaGB *int64) error {
	_, err := s.db.Exec(`
		INSERT INTO user_billing (user_id, grace_period, r2_retention, node_retention, r2_quota_gb, updated_at)
		VALUES ($1, NULLIF($2,''), NULLIF($3,''), NULLIF($4,''), $5, NOW())
		ON CONFLICT (user_id) DO UPDATE SET
			grace_period   = NULLIF($2,''),
			r2_retention   = NULLIF($3,''),
			node_retention = NULLIF($4,''),
			r2_quota_gb    = $5,
			updated_at     = NOW()`,
		userID, gracePeriod, r2Retention, nodeRetention, r2QuotaGB)
	return err
}

// SetUserManualEntitlement upserts the admin-granted entitlement, leaving status
// and every override untouched.
//
// kind is "byon" | "route_only" | "both"; expiresAt is when it lapses. Pass an
// empty kind (and a nil expiry) to REVOKE: the row is cleared rather than left
// with a past date, so "no grant" and "an expired grant" are the same state in
// the database instead of two that read differently in the admin UI.
//
// grantedBy is the acting admin, kept for the audit trail; empty writes NULL.
func (s *PostgresStore) SetUserManualEntitlement(userID, kind string, expiresAt *time.Time, grantedBy string) error {
	if kind == "" {
		expiresAt = nil
	}
	var by any
	if grantedBy != "" {
		by = grantedBy
	}
	// granted_at is only stamped when a grant is actually present, so a revoke
	// does not leave a timestamp for something that no longer exists. Decided in
	// Go rather than with a CASE on $2: reusing one placeholder as both an
	// assigned value and a literal comparison makes Postgres refuse to prepare the
	// statement (it cannot infer one type for both uses).
	var grantedAt any
	if kind != "" {
		grantedAt = time.Now()
	}
	_, err := s.db.Exec(`
		INSERT INTO user_billing (user_id, manual_entitlement, manual_entitlement_expires_at,
			manual_entitlement_granted_at, manual_entitlement_granted_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (user_id) DO UPDATE SET
			manual_entitlement            = EXCLUDED.manual_entitlement,
			manual_entitlement_expires_at = EXCLUDED.manual_entitlement_expires_at,
			manual_entitlement_granted_at = EXCLUDED.manual_entitlement_granted_at,
			manual_entitlement_granted_by = EXCLUDED.manual_entitlement_granted_by,
			updated_at                    = NOW()`,
		userID, kind, expiresAt, grantedAt, by)
	return err
}

// SetUserManualEntitlementKind grants or clears ONE kind, leaving the other
// exactly as it was.
//
// This is what "grant BYON" and "grant route-only" have to mean if a tenant can
// hold both. SetUserManualEntitlement writes the whole grant at once, so calling
// it for route-only silently ended a BYON grant that was still running - the
// "it just switches between the two" report.
//
// expiresAt nil clears that kind. The legacy manual_entitlement string is
// recomputed IN THE SAME STATEMENT from this kind's new value and the other
// kind's stored one, so the two shapes cannot drift apart and no read-modify-
// write window exists for a second admin to land in.
func (s *PostgresStore) SetUserManualEntitlementKind(userID, kind string, expiresAt *time.Time, grantedBy string) error {
	var by any
	if grantedBy != "" {
		by = grantedBy
	}
	// Only stamped when something is actually granted, for the same reason the
	// whole-grant writer gives: a revoke must not leave a timestamp behind for a
	// grant that no longer exists.
	var grantedAt any
	if expiresAt != nil {
		grantedAt = time.Now()
	}

	// Two statements rather than one with an interpolated column name. The pair
	// is symmetrical and short, and building SQL identifiers from a parameter is
	// how a switch like this turns into an injection point later.
	var q string
	switch kind {
	case "byon":
		q = `
		INSERT INTO user_billing (user_id, manual_entitlement, manual_entitlement_byon_expires_at,
			manual_entitlement_granted_at, manual_entitlement_granted_by, updated_at)
		VALUES ($1, CASE WHEN $2::timestamptz IS NULL THEN '' ELSE 'byon' END, $2, $3, $4, NOW())
		ON CONFLICT (user_id) DO UPDATE SET
			manual_entitlement_byon_expires_at = $2,
			manual_entitlement = CASE
				WHEN $2::timestamptz IS NOT NULL AND user_billing.manual_entitlement_route_expires_at IS NOT NULL THEN 'both'
				WHEN $2::timestamptz IS NOT NULL THEN 'byon'
				WHEN user_billing.manual_entitlement_route_expires_at IS NOT NULL THEN 'route_only'
				ELSE '' END,
			manual_entitlement_granted_at = COALESCE($3, user_billing.manual_entitlement_granted_at),
			manual_entitlement_granted_by = COALESCE($4, user_billing.manual_entitlement_granted_by),
			updated_at = NOW()`
	case "route_only":
		q = `
		INSERT INTO user_billing (user_id, manual_entitlement, manual_entitlement_route_expires_at,
			manual_entitlement_granted_at, manual_entitlement_granted_by, updated_at)
		VALUES ($1, CASE WHEN $2::timestamptz IS NULL THEN '' ELSE 'route_only' END, $2, $3, $4, NOW())
		ON CONFLICT (user_id) DO UPDATE SET
			manual_entitlement_route_expires_at = $2,
			manual_entitlement = CASE
				WHEN $2::timestamptz IS NOT NULL AND user_billing.manual_entitlement_byon_expires_at IS NOT NULL THEN 'both'
				WHEN $2::timestamptz IS NOT NULL THEN 'route_only'
				WHEN user_billing.manual_entitlement_byon_expires_at IS NOT NULL THEN 'byon'
				ELSE '' END,
			manual_entitlement_granted_at = COALESCE($3, user_billing.manual_entitlement_granted_at),
			manual_entitlement_granted_by = COALESCE($4, user_billing.manual_entitlement_granted_by),
			updated_at = NOW()`
	default:
		return fmt.Errorf("unknown entitlement kind %q", kind)
	}
	_, err := s.db.Exec(q, userID, expiresAt, grantedAt, by)
	return err
}

// ListServersByOwner returns the servers a tenant owns (id, uuid, status,
// node_id), used by the billing lifecycle to stop a suspended tenant's servers
// without pulling the full server columns.
func (s *PostgresStore) ListServersByOwner(ownerID string) ([]models.Server, error) {
	rows, err := s.db.Query(`SELECT id, uuid, status, node_id FROM servers WHERE owner_id = $1`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Server
	for rows.Next() {
		var srv models.Server
		if err := rows.Scan(&srv.ID, &srv.UUID, &srv.Status, &srv.NodeID); err != nil {
			return nil, err
		}
		out = append(out, srv)
	}
	return out, rows.Err()
}

// BackupRunRef is the minimal handle the retention cleanup needs to delete a
// stored backup: the run row id, its storage object key, and which storage
// backend holds it.
type BackupRunRef struct {
	RunID      int
	StorageKey string
	StorageID  *int
}

// ListBackupRunsByOwner returns every backup run for the servers a tenant owns,
// used by the billing retention cleanup to delete R2 objects + rows after the
// r2_retention window.
func (s *PostgresStore) ListBackupRunsByOwner(ownerID string) ([]BackupRunRef, error) {
	// The RUN's own storage where it has one, the job's only as a fallback: an
	// archive written before the job was pointed elsewhere still lives where it
	// was written, and deleting against the job's current storage would delete
	// nothing while dropping the row that pointed at the real object.
	rows, err := s.db.Query(`
		SELECT br.id, br.storage_key, COALESCE(br.storage_id, bj.storage_id)
		FROM backup_runs br
		JOIN backup_jobs bj ON bj.id = br.job_id
		JOIN servers s ON s.id = bj.server_id
		WHERE s.owner_id = $1`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BackupRunRef
	for rows.Next() {
		var ref BackupRunRef
		var sid sql.NullInt64
		if err := rows.Scan(&ref.RunID, &ref.StorageKey, &sid); err != nil {
			return nil, err
		}
		if sid.Valid {
			v := int(sid.Int64)
			ref.StorageID = &v
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// BackupBytesByOwner returns the storage a tenant's backups occupy ON OURS.
// Used by the R2 quota gate before a new backup.
//
// It counts successful runs AND any run carrying a nonzero size, which is how it
// picks up an abandoned run whose archive was confirmed present by the reaper.
// The node reports every failed backup with size 0 (node/backup_worker.go), so
// the only failed rows with a size are those the reaper found an object for -
// real bytes on the backend that would otherwise sit uncounted.
//
// Archives on a storage the TENANT connected are excluded: the quota exists
// because we pay for the space, and we pay nothing for their bucket. Billing
// them for it would charge them for storage they already bought.
//
// The exclusion keys off the run's OWN storage_id, not the job's. A job with no
// storage set resolves through a chain whose answer changes - connect a bucket
// today and yesterday's archives would retroactively stop counting, or worse,
// disconnect one and archives that are physically gone would start counting
// again. A run records where it actually went. NULL is read as ours, which is
// the safe direction: it counts rather than silently exempting.
func (s *PostgresStore) BackupBytesByOwner(ownerID string) (int64, error) {
	var total sql.NullInt64
	err := s.db.QueryRow(`
		SELECT COALESCE(SUM(br.size_bytes), 0)
		FROM backup_runs br
		JOIN backup_jobs bj ON bj.id = br.job_id
		JOIN servers s ON s.id = bj.server_id
		LEFT JOIN backup_storages bst ON bst.id = br.storage_id
		WHERE s.owner_id = $1 AND (br.status = 'success' OR br.size_bytes > 0)
		  AND bst.owner_id IS NULL`, ownerID).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total.Int64, nil
}

// BackupBytesByOwnerOnOwnStorage is the counterpart: what the tenant keeps on a
// bucket of their own. Not billed and not capped, but shown, because "you are
// storing 400 GB" is a different sentence from "you are storing 400 GB that we
// charge you for" and a screen that omitted the first would look wrong.
func (s *PostgresStore) BackupBytesByOwnerOnOwnStorage(ownerID string) (int64, error) {
	var total sql.NullInt64
	err := s.db.QueryRow(`
		SELECT COALESCE(SUM(br.size_bytes), 0)
		FROM backup_runs br
		JOIN backup_jobs bj ON bj.id = br.job_id
		JOIN servers s ON s.id = bj.server_id
		JOIN backup_storages bst ON bst.id = br.storage_id
		WHERE s.owner_id = $1 AND (br.status = 'success' OR br.size_bytes > 0)
		  AND bst.owner_id IS NOT NULL`, ownerID).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total.Int64, nil
}

// ListUserBillingByStatus returns every tenant in a given lifecycle status. Used
// by the leader-gated lifecycle worker to progress past_due -> suspended ->
// retention cleanup.
func (s *PostgresStore) ListUserBillingByStatus(status string) ([]UserBilling, error) {
	rows, err := s.db.Query(`SELECT `+userBillingCols+` FROM user_billing WHERE status = $1`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserBilling
	for rows.Next() {
		b, err := scanUserBilling(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}
