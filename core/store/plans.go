package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/lib/pq"
)

// ErrPlanInUse means users are still assigned to this plan. users.plan_id is
// REFERENCES plans(id) with no ON DELETE clause, so Postgres refuses rather than
// leaving accounts pointing at a plan that no longer exists. Typed for the same
// reason as ErrUserOwnsServers and ErrNodeHasServers: a bare 500 tells an
// operator nothing about what to do next.
var ErrPlanInUse = errors.New("plan is still assigned to users")

// Plan is an admin-defined BYON tier. 0 means unlimited for every limit. Traffic
// limits are monthly and WARN-only (edge + relay tracked separately, combined is
// edge+relay). A plan's value is the baseline; a per-user override in user_billing
// wins (see services.EffectiveLimits).
type Plan struct {
	ID                int    `json:"id"`
	Name              string `json:"name"`
	PriceLabel        string `json:"priceLabel"`
	MaxNodes          int64  `json:"maxNodes"`
	MaxLinks          int64  `json:"maxLinks"`
	R2QuotaGB         int64  `json:"r2QuotaGb"`
	TrafficEdgeGB     int64  `json:"trafficEdgeGb"`
	TrafficRelayGB    int64  `json:"trafficRelayGb"`
	TrafficCombinedGB int64  `json:"trafficCombinedGb"`
	IsDefault         bool   `json:"isDefault"`
	// Kind is WHAT the plan grants: "byon" | "route_only" | "both". Defaults to
	// "both" so existing plans keep allowing everything (see the entitlement
	// migration); narrow it per plan deliberately. An unrecognised value grants
	// nothing - services.kindGrants fails closed rather than open.
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"createdAt"`
}

const planCols = `id, name, price_label, max_nodes, max_links, r2_quota_gb, traffic_edge_gb, traffic_relay_gb, traffic_combined_gb, is_default, kind, created_at`

// normalizePlanKind keeps an unknown or empty kind from being written. Empty
// means "the caller did not say", and the behaviour-preserving answer there is
// "both" - the same default the column carries - not "nothing".
func normalizePlanKind(kind string) string {
	switch kind {
	case "byon", "route_only", "both":
		return kind
	default:
		return "both"
	}
}

func scanPlan(row interface{ Scan(dest ...any) error }) (*Plan, error) {
	var p Plan
	if err := row.Scan(&p.ID, &p.Name, &p.PriceLabel, &p.MaxNodes, &p.MaxLinks, &p.R2QuotaGB,
		&p.TrafficEdgeGB, &p.TrafficRelayGB, &p.TrafficCombinedGB, &p.IsDefault, &p.Kind, &p.CreatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *PostgresStore) ListPlans() ([]Plan, error) {
	rows, err := s.db.Query(`SELECT ` + planCols + ` FROM plans ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *PostgresStore) GetPlan(id int) (*Plan, error) {
	return scanPlan(s.db.QueryRow(`SELECT `+planCols+` FROM plans WHERE id = $1`, id))
}

// GetDefaultPlan returns the default plan, or nil when none is set (sql.ErrNoRows
// is swallowed to nil so callers can treat "no default" as "unlimited").
func (s *PostgresStore) GetDefaultPlan() (*Plan, error) {
	p, err := scanPlan(s.db.QueryRow(`SELECT ` + planCols + ` FROM plans WHERE is_default LIMIT 1`))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

// CreatePlan inserts a plan. When isDefault, it clears any existing default first
// (single-default invariant, also guarded by a partial unique index).
func (s *PostgresStore) CreatePlan(p Plan) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if p.IsDefault {
		if _, err = tx.Exec(`UPDATE plans SET is_default = FALSE WHERE is_default`); err != nil {
			return 0, err
		}
	}
	var id int
	err = tx.QueryRow(`
		INSERT INTO plans (name, price_label, max_nodes, max_links, r2_quota_gb, traffic_edge_gb, traffic_relay_gb, traffic_combined_gb, is_default, kind)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		p.Name, p.PriceLabel, p.MaxNodes, p.MaxLinks, p.R2QuotaGB, p.TrafficEdgeGB, p.TrafficRelayGB, p.TrafficCombinedGB, p.IsDefault, normalizePlanKind(p.Kind)).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// UpdatePlan updates a plan in place, honoring the single-default invariant.
func (s *PostgresStore) UpdatePlan(p Plan) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if p.IsDefault {
		if _, err = tx.Exec(`UPDATE plans SET is_default = FALSE WHERE is_default AND id <> $1`, p.ID); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`
		UPDATE plans SET name=$2, price_label=$3, max_nodes=$4, max_links=$5, r2_quota_gb=$6,
			traffic_edge_gb=$7, traffic_relay_gb=$8, traffic_combined_gb=$9, is_default=$10, kind=$11
		WHERE id=$1`,
		p.ID, p.Name, p.PriceLabel, p.MaxNodes, p.MaxLinks, p.R2QuotaGB, p.TrafficEdgeGB, p.TrafficRelayGB, p.TrafficCombinedGB, p.IsDefault, normalizePlanKind(p.Kind)); err != nil {
		return err
	}
	return tx.Commit()
}

// DeletePlan removes a plan. users.plan_id is ON DELETE SET NULL-free (plain FK),
// so callers should ensure no user references it, or rely on the resolver falling
// back to the default plan once the row is gone (GetPlan errors -> default).
func (s *PostgresStore) DeletePlan(id int) error {
	_, err := s.db.Exec(`DELETE FROM plans WHERE id = $1`, id)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23503" && pqErr.Constraint == "users_plan_id_fkey" {
			return ErrPlanInUse
		}
	}
	return err
}

// GetUserPlanID returns the user's plan_id, or nil when unset (-> default plan).
func (s *PostgresStore) GetUserPlanID(userID string) (*int, error) {
	var pid sql.NullInt64
	if err := s.db.QueryRow(`SELECT plan_id FROM users WHERE id = $1`, userID).Scan(&pid); err != nil {
		return nil, err
	}
	if !pid.Valid {
		return nil, nil
	}
	v := int(pid.Int64)
	return &v, nil
}

// SetUserPlan assigns (planID non-nil) or clears (nil -> default plan) a user's plan.
func (s *PostgresStore) SetUserPlan(userID string, planID *int) error {
	_, err := s.db.Exec(`UPDATE users SET plan_id = $1 WHERE id = $2`, planID, userID)
	return err
}

// SetUserLimitOverrides upserts the per-user LIMIT overrides in user_billing
// (nil clears one -> use the plan value). It does NOT touch r2_quota_gb (owned by
// SetUserBillingOverrides) or the lifecycle/retention columns.
func (s *PostgresStore) SetUserLimitOverrides(userID string, adminNodes, adminLinks string, trafficEdge, trafficRelay, trafficCombined *int64) error {
	_, err := s.db.Exec(`
		INSERT INTO user_billing (user_id, admin_max_nodes, admin_max_links, traffic_edge_gb, traffic_relay_gb, traffic_combined_gb, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,NOW())
		ON CONFLICT (user_id) DO UPDATE SET
			admin_max_nodes = $2, admin_max_links = $3, traffic_edge_gb = $4, traffic_relay_gb = $5,
			traffic_combined_gb = $6, updated_at = NOW()`,
		userID, adminNodes, adminLinks, trafficEdge, trafficRelay, trafficCombined)
	return err
}

// MoveOperatorLimitsToAdminLayer is for an install with no store, where every
// per-user node, link and address limit was set by an operator: they move to
// the operator's layer, where the panel shows and clears them. Left as a
// "purchase" they would still be enforced and no screen could reset them.
// Idempotent; a value already in the operator's layer wins.
func (s *PostgresStore) MoveOperatorLimitsToAdminLayer() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		UPDATE user_billing SET
			admin_max_nodes = CASE WHEN COALESCE(admin_max_nodes, '') = '' AND max_nodes IS NOT NULL THEN max_nodes::text ELSE admin_max_nodes END,
			admin_max_links = CASE WHEN COALESCE(admin_max_links, '') = '' AND max_links IS NOT NULL THEN max_links::text ELSE admin_max_links END,
			max_nodes = NULL, max_links = NULL
		WHERE max_nodes IS NOT NULL OR max_links IS NOT NULL`); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO gateway_route_limits (scope, max_routes)
		SELECT 'admin:' || substr(scope, 6), max_routes FROM gateway_route_limits
		WHERE scope LIKE 'user:%'
		ON CONFLICT (scope) DO NOTHING`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM gateway_route_limits WHERE scope LIKE 'user:%'`); err != nil {
		return err
	}
	return tx.Commit()
}

// SetUserPurchasedEntitlement upserts ONLY the node/link overrides, and only for
// the dimensions the caller actually named. The store is the source of truth for
// what a tenant BOUGHT (a node count, and routes on a manual grant); an admin may
// independently have set traffic or R2 overrides, and a purchase must not wipe
// those. Passing set=false for a dimension leaves that column exactly as it was,
// which is why this cannot reuse SetUserLimitOverrides (that one owns all five
// columns and writes NULL for anything the PATCH body omitted).
//
// A nil value with set=true CLEARS the override, dropping the tenant back to the
// plan baseline - that is how a subscription ending returns them to free limits.
func (s *PostgresStore) SetUserPurchasedEntitlement(userID string, maxNodes *int64, setNodes bool, maxLinks *int64, setLinks bool) error {
	_, err := s.db.Exec(`
		INSERT INTO user_billing (user_id, max_nodes, max_links, updated_at)
		VALUES ($1,$2,$3,NOW())
		ON CONFLICT (user_id) DO UPDATE SET
			max_nodes  = CASE WHEN $4::bool THEN $2 ELSE user_billing.max_nodes END,
			max_links  = CASE WHEN $5::bool THEN $3 ELSE user_billing.max_links END,
			updated_at = NOW()`,
		userID, maxNodes, maxLinks, setNodes, setLinks)
	return err
}

// SetUserTrafficBilling records what the store has decided about this tenant's
// traffic: where their free allowance ends and whether they have agreed to be
// charged past it. Touches ONLY those two columns - the row also carries the
// payment status and every limit override, and a provision call must not rewrite
// either. Core never decides these; it reads them to warn the tenant.
func (s *PostgresStore) SetUserTrafficBilling(userID string, ceilingGB int64, enabled bool) error {
	_, err := s.db.Exec(`
		INSERT INTO user_billing (user_id, traffic_ceiling_gb, traffic_billing_enabled, updated_at)
		VALUES ($1,$2,$3,NOW())
		ON CONFLICT (user_id) DO UPDATE SET
			traffic_ceiling_gb      = $2,
			traffic_billing_enabled = $3,
			updated_at              = NOW()`,
		userID, ceilingGB, enabled)
	return err
}

// SetUserBillingConsent records what the tenant has AGREED to be charged for.
// A nil leaves that column alone.
//
// Separate from SetUserTrafficBilling because the ceiling stopped being sent:
// the store owns the money and the consent, Core owns the allowances and works
// them out from its own settings. That change broke the old writer without
// anybody noticing, because its caller required BOTH the ceiling and the flag to
// be present and silently did nothing when only one was - so a tenant who
// switched metered billing on stayed recorded as having refused it.
func (s *PostgresStore) SetUserBillingConsent(userID string, traffic, backup *bool) error {
	if traffic == nil && backup == nil {
		return nil
	}
	// Written with COALESCE rather than a built query so a partial update cannot
	// clear the flag it was not told about.
	_, err := s.db.Exec(`
		INSERT INTO user_billing (user_id, traffic_billing_enabled, backup_billing_enabled, updated_at)
		VALUES ($1, COALESCE($2, FALSE), COALESCE($3, FALSE), NOW())
		ON CONFLICT (user_id) DO UPDATE SET
			traffic_billing_enabled = COALESCE($2, user_billing.traffic_billing_enabled),
			backup_billing_enabled  = COALESCE($3, user_billing.backup_billing_enabled),
			updated_at              = NOW()`,
		userID, traffic, backup)
	return err
}

// GetUserDataBillingEnabled: whether the tenant agreed to pay for DATA traffic
// overage. Unset follows the player consent (the store's old single flag).
func (s *PostgresStore) GetUserDataBillingEnabled(userID string) (bool, error) {
	var v bool
	err := s.db.QueryRow(`SELECT COALESCE(data_billing_enabled, traffic_billing_enabled, FALSE)
		FROM user_billing WHERE user_id = $1`, userID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return v, err
}

// SetUserDataBillingEnabled records the data overage consent.
func (s *PostgresStore) SetUserDataBillingEnabled(userID string, v bool) error {
	_, err := s.db.Exec(`
		INSERT INTO user_billing (user_id, data_billing_enabled, updated_at) VALUES ($1, $2, NOW())
		ON CONFLICT (user_id) DO UPDATE SET data_billing_enabled = $2, updated_at = NOW()`, userID, v)
	return err
}

// CountNodesByOwner returns how many nodes a tenant owns (for the max_nodes gate).
func (s *PostgresStore) CountNodesByOwner(ownerID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE owner_id = $1`, ownerID).Scan(&n)
	return n, err
}

// CountNodeWarpKeysByOwner returns how many UNREDEEMED BYON node warp keys a
// tenant holds. Counted against max_nodes alongside CountNodesByOwner, because a
// minted key is a node that has not connected YET - capping only on nodes that
// already exist would let a tenant with a one-node plan mint keys without limit
// and stand up as many machines as they liked.
//
// UNREDEEMED is the whole word, and it used to be only a word: the query counted
// every live key. A BYON machine needs BOTH secrets, as the panel that mints
// them says - a warp key to reach the overlay and an enroll token to become a
// node - and neither is retired when the machine connects, because warp
// re-enrols with that key for the life of the machine. So one running machine
// was one nodes row PLUS one live key, and every consumer of this sum counted it
// twice: the mint gate refused the second machine on a two-node plan, the panel
// showed 2 of 2 for one box, and the over-limit sweep read a tenant who was
// exactly within their plan as over it - which stops everything they own 72
// hours later. Measured against Postgres: one machine, counted as two.
//
// A key is redeemed once it has enrolled a warp peer. warp_peers.api_key_id is
// NOT NULL and written at enrolment, so this is exact rather than a heuristic,
// and it is the index idx_warp_peers_key exists for.
//
// It under-counts for the seconds between a machine joining the overlay and its
// node agent pairing: the key is redeemed and the nodes row does not exist yet.
// That direction is deliberate. Being briefly generous costs a tenant nothing;
// being briefly wrong the other way cuts off a customer who is paid up.
func (s *PostgresStore) CountNodeWarpKeysByOwner(ownerID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM warp_api_keys k
		WHERE k.owner_id = $1::uuid AND k.revoked_at IS NULL AND k.node_id LIKE 'node-%'
		  AND NOT EXISTS (SELECT 1 FROM warp_peers p WHERE p.api_key_id = k.id)`, ownerID).Scan(&n)
	return n, err
}

// CountLinkKitsByOwner returns how many route-only link kits a tenant owns (for the
// max_links gate). warp_api_keys also holds BYON node keys, so this filters to the
// 'link-' node_id prefix.
func (s *PostgresStore) CountLinkKitsByOwner(ownerID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM warp_api_keys
		WHERE owner_id = $1::uuid AND revoked_at IS NULL AND node_id LIKE 'link-%'`, ownerID).Scan(&n)
	return n, err
}
