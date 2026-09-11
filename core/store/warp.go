package store

import (
	"database/sql"
	"errors"
	"time"
)

func (s *PostgresStore) CreateWarpAPIKey(k WarpAPIKey) (int, error) {
	var id int
	err := s.db.QueryRow(`
		INSERT INTO warp_api_keys (name, key_hash, policy, max_conns, on_new_conn, fixed_wg_ip, node_id, region, owner_id)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),NULLIF($9,'')::uuid)
		RETURNING id`,
		k.Name, k.KeyHash, k.Policy, k.MaxConns, k.OnNewConn, k.FixedWGIP, k.NodeID, k.Region, k.OwnerID,
	).Scan(&id)
	return id, err
}

func (s *PostgresStore) GetWarpAPIKeyByHash(hash string) (*WarpAPIKey, error) {
	var k WarpAPIKey
	var fixedIP, nodeID, region, ownerID sql.NullString
	err := s.db.QueryRow(`
		SELECT id, name, key_hash, policy, max_conns, on_new_conn,
		       COALESCE(fixed_wg_ip,''), COALESCE(node_id,''), COALESCE(region,''),
		       COALESCE(owner_id::text,''), COALESCE(bound_node_id, 0), revoked_at, created_at
		FROM warp_api_keys WHERE key_hash = $1`, hash).
		Scan(&k.ID, &k.Name, &k.KeyHash, &k.Policy, &k.MaxConns, &k.OnNewConn,
			&fixedIP, &nodeID, &region, &ownerID, &k.BoundNodeID, &k.RevokedAt, &k.CreatedAt)
	if err != nil {
		return nil, err
	}
	k.FixedWGIP, k.NodeID, k.Region, k.OwnerID = fixedIP.String, nodeID.String, region.String, ownerID.String
	return &k, nil
}

// ListWarpAPIKeysByOwner returns the non-revoked warp keys minted for a tenant —
// the link/route-only kits they own. Used by the panel to list "my links".
func (s *PostgresStore) ListWarpAPIKeysByOwner(ownerID string) ([]WarpAPIKey, error) {
	rows, err := s.db.Query(`
		SELECT id, name, key_hash, policy, max_conns, on_new_conn,
		       COALESCE(fixed_wg_ip,''), COALESCE(node_id,''), COALESCE(region,''),
		       COALESCE(owner_id::text,''), COALESCE(bound_node_id, 0), revoked_at, created_at
		FROM warp_api_keys
		WHERE owner_id = $1::uuid AND revoked_at IS NULL
		ORDER BY created_at DESC`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WarpAPIKey
	for rows.Next() {
		var k WarpAPIKey
		var fixedIP, nodeID, region, owner sql.NullString
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyHash, &k.Policy, &k.MaxConns, &k.OnNewConn,
			&fixedIP, &nodeID, &region, &owner, &k.BoundNodeID, &k.RevokedAt, &k.CreatedAt); err != nil {
			return nil, err
		}
		k.FixedWGIP, k.NodeID, k.Region, k.OwnerID = fixedIP.String, nodeID.String, region.String, owner.String
		out = append(out, k)
	}
	return out, rows.Err()
}

// ListWarpAPIKeys returns every ADMIN-minted enrollment key (owner_id IS NULL),
// including revoked ones, newest first - the panel's external-node inventory.
// Tenant link kits are deliberately excluded: those are listed per-owner as
// "Protected Addresses" and revoking them takes the full route/ACL teardown,
// not the plain key revoke this list feeds.
func (s *PostgresStore) ListWarpAPIKeys() ([]WarpAPIKey, error) {
	rows, err := s.db.Query(`
		SELECT id, name, key_hash, policy, max_conns, on_new_conn,
		       COALESCE(fixed_wg_ip,''), COALESCE(node_id,''), COALESCE(region,''),
		       COALESCE(owner_id::text,''), COALESCE(bound_node_id, 0), revoked_at, created_at
		FROM warp_api_keys
		WHERE owner_id IS NULL
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WarpAPIKey
	for rows.Next() {
		var k WarpAPIKey
		var fixedIP, nodeID, region, owner sql.NullString
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyHash, &k.Policy, &k.MaxConns, &k.OnNewConn,
			&fixedIP, &nodeID, &region, &owner, &k.BoundNodeID, &k.RevokedAt, &k.CreatedAt); err != nil {
			return nil, err
		}
		k.FixedWGIP, k.NodeID, k.Region, k.OwnerID = fixedIP.String, nodeID.String, region.String, owner.String
		out = append(out, k)
	}
	return out, rows.Err()
}

// GetWarpAPIKeyByID fetches one key regardless of owner or revocation state.
func (s *PostgresStore) GetWarpAPIKeyByID(id int) (*WarpAPIKey, error) {
	var k WarpAPIKey
	var fixedIP, nodeID, region, owner sql.NullString
	err := s.db.QueryRow(`
		SELECT id, name, key_hash, policy, max_conns, on_new_conn,
		       COALESCE(fixed_wg_ip,''), COALESCE(node_id,''), COALESCE(region,''),
		       COALESCE(owner_id::text,''), COALESCE(bound_node_id, 0), revoked_at, created_at
		FROM warp_api_keys WHERE id = $1`, id).
		Scan(&k.ID, &k.Name, &k.KeyHash, &k.Policy, &k.MaxConns, &k.OnNewConn,
			&fixedIP, &nodeID, &region, &owner, &k.BoundNodeID, &k.RevokedAt, &k.CreatedAt)
	if err != nil {
		return nil, err
	}
	k.FixedWGIP, k.NodeID, k.Region, k.OwnerID = fixedIP.String, nodeID.String, region.String, owner.String
	return &k, nil
}

// RevokeWarpAPIKeyByID marks one key revoked (idempotent). Blocking future
// enrolls only; disconnecting its live peers is the WarpService's job.
func (s *PostgresStore) RevokeWarpAPIKeyByID(id int) error {
	_, err := s.db.Exec(`UPDATE warp_api_keys SET revoked_at = NOW() WHERE id = $1 AND revoked_at IS NULL`, id)
	return err
}

// DeleteWarpAPIKeyByID removes the row outright. warp_peers has ON DELETE
// CASCADE, so its peer rows go with it - which is exactly why the caller MUST
// disconnect those peers from the leaders first: once the rows are gone, the
// resync that would otherwise clean them up no longer knows they existed, and a
// live WireGuard peer would keep forwarding with nothing left to remove it.
func (s *PostgresStore) DeleteWarpAPIKeyByID(id int) error {
	_, err := s.db.Exec(`DELETE FROM warp_api_keys WHERE id = $1`, id)
	return err
}

// OwnerCutOff reports whether a tenant has been cut off: their enforcement has
// persisted past its grace, and this is the point at which route-only links
// actually stop. A nil billing row, an active status, or a grace still running
// all return false and the link may boot.
//
// Two ways to get here, and only the first used to be checked. Non-payment is
// one. Holding more than was bought is the other, and enforceEntitlementLimits
// cuts a tenant off for it after 72 hours and an email - but this gate never
// asked, so the client re-enrolled within minutes and the sweep fought it every
// hour, forever. A route-only tenant has no servers to stop and no warp peers to
// drop, so for them the over-limit cutoff did nothing whatsoever.
//
// MUST stay equivalent to ownerCutOffSQL below, which is the same two arms in
// SQL; LinkBoot, the warp enroll gate and the two reconciler queries all have to
// agree about a given kit or it flaps every 60 seconds. It lives HERE, beside
// that SQL, rather than beside the handlers that call it: two spellings of one
// rule drifting apart is the failure mode, and a package boundary between them
// made the equivalence untestable. An integration test can now put the same row
// through both.
func OwnerCutOff(b *UserBilling, suspendGrace, overLimitGrace time.Duration, now time.Time) bool {
	if b == nil {
		return false
	}
	if b.Status == "suspended" && b.SuspendedAt != nil && !now.Before(b.SuspendedAt.Add(suspendGrace)) {
		return true
	}
	return b.OverLimitSince != nil && !now.Before(b.OverLimitSince.Add(overLimitGrace))
}

// ownerCutOffSQL is the ONE spelling of "this link's owner has been cut off",
// shared by the two queries below so they can never disagree about a given kit -
// one says who KEEPS an ACL and the other says whose must be TORN DOWN, and a
// kit that satisfied neither, or both, would flap every 60s.
//
// $1 is now-minus-suspension-grace, $2 now-minus-over-limit-grace. Both arms are
// "past the grace", never "right now": the grace is what makes a suspension a
// warning first.
//
// Two arms, because there are two ways to be cut off and only one of them used
// to be here. A tenant holding more than they bought is stopped by
// enforceEntitlementLimits after its own grace - but that pass had no
// counterpart in this predicate, so the reconciler re-provisioned every link it
// dropped within the next tick, and the cutoff was undone before anyone noticed
// it had happened. For a route-only tenant, who has no servers and no warp
// peers, that made the entire over-limit cutoff a no-op.
//
// Admin-minted kits (owner_id NULL -> no billing row) are never cut off: the
// LEFT JOIN yields NULL billing, both "IS NOT NULL" atoms are FALSE, and the
// whole predicate is FALSE.
//
// MUST stay equivalent to handlers.ownerCutOff.
const ownerCutOffSQL = `(
    (ub.status = 'suspended' AND ub.suspended_at IS NOT NULL AND ub.suspended_at <= $1)
    OR (ub.overlimit_since IS NOT NULL AND ub.overlimit_since <= $2)
)`

// ListLinkKitsForACLReconcile returns the non-revoked route-only link kits the
// ACL reconciler should keep provisioned: every link EXCEPT those whose owner has
// been cut off (see ownerCutOffSQL). A link whose owner is active, or still
// inside either grace, is kept. Mirrors ListWarpAPIKeysByOwner's column list +
// scan.
func (s *PostgresStore) ListLinkKitsForACLReconcile(hardSuspendedBefore, overLimitBefore time.Time) ([]WarpAPIKey, error) {
	rows, err := s.db.Query(`
		SELECT w.id, w.name, w.key_hash, w.policy, w.max_conns, w.on_new_conn,
		       COALESCE(w.fixed_wg_ip,''), COALESCE(w.node_id,''), COALESCE(w.region,''),
		       COALESCE(w.owner_id::text,''), COALESCE(w.bound_node_id, 0), w.revoked_at, w.created_at
		FROM warp_api_keys w
		LEFT JOIN user_billing ub ON ub.user_id = w.owner_id
		WHERE w.node_id LIKE 'link-%' AND w.revoked_at IS NULL
		  AND NOT `+ownerCutOffSQL+`
		ORDER BY w.created_at DESC`, hardSuspendedBefore, overLimitBefore)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WarpAPIKey
	for rows.Next() {
		var k WarpAPIKey
		var fixedIP, nodeID, region, owner sql.NullString
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyHash, &k.Policy, &k.MaxConns, &k.OnNewConn,
			&fixedIP, &nodeID, &region, &owner, &k.BoundNodeID, &k.RevokedAt, &k.CreatedAt); err != nil {
			return nil, err
		}
		k.FixedWGIP, k.NodeID, k.Region, k.OwnerID = fixedIP.String, nodeID.String, region.String, owner.String
		out = append(out, k)
	}
	return out, rows.Err()
}

// ListLinkKitsForACLTeardown returns route-only link kits that MUST NOT have a
// Redis ACL user right now: either revoked (recently - see below) or owned by a
// tenant who has been cut off (ownerCutOffSQL, the same predicate the reconcile
// query negates). It is the self-heal counterpart to
// ListLinkKitsForACLReconcile: that query says who KEEPS an ACL, this one says
// who must have theirs TORN DOWN, so a DELUSER that failed at revoke/suspend
// time (a transient Redis blip) converges within the next ~60s tick instead of
// leaving a valid scoped cred live indefinitely.
//
// The revoked arm is bounded to revokedAfter (now-24h in the reconciler) so
// revoked kits don't accumulate in this query forever - a teardown that keeps
// failing for a full 24h straight is assumed already gone (DELUSER is
// idempotent, a later retry may simply be a no-op) or a deeper problem an
// operator needs to look at, not something to retry forever.
//
// The cut-off arm has no time lower bound: reactivation clears suspended_at and
// coming back within limits clears overlimit_since, either of which removes the
// row from this result on its own.
//
// The parameter ORDER matches the reconcile query on purpose ($1 suspension,
// $2 over-limit) so both can share one predicate string; revokedAfter is $3
// because it belongs to this query alone.
func (s *PostgresStore) ListLinkKitsForACLTeardown(hardSuspendedBefore, overLimitBefore, revokedAfter time.Time) ([]WarpAPIKey, error) {
	rows, err := s.db.Query(`
		SELECT w.id, w.name, w.key_hash, w.policy, w.max_conns, w.on_new_conn,
		       COALESCE(w.fixed_wg_ip,''), COALESCE(w.node_id,''), COALESCE(w.region,''),
		       COALESCE(w.owner_id::text,''), COALESCE(w.bound_node_id, 0), w.revoked_at, w.created_at
		FROM warp_api_keys w
		LEFT JOIN user_billing ub ON ub.user_id = w.owner_id
		WHERE w.node_id LIKE 'link-%'
		  AND (
		    (w.revoked_at IS NOT NULL AND w.revoked_at >= $3)
		    OR `+ownerCutOffSQL+`
		  )
		ORDER BY w.created_at DESC`, hardSuspendedBefore, overLimitBefore, revokedAfter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WarpAPIKey
	for rows.Next() {
		var k WarpAPIKey
		var fixedIP, nodeID, region, owner sql.NullString
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyHash, &k.Policy, &k.MaxConns, &k.OnNewConn,
			&fixedIP, &nodeID, &region, &owner, &k.BoundNodeID, &k.RevokedAt, &k.CreatedAt); err != nil {
			return nil, err
		}
		k.FixedWGIP, k.NodeID, k.Region, k.OwnerID = fixedIP.String, nodeID.String, region.String, owner.String
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *PostgresStore) InsertWarpPeer(p WarpPeer) (int, error) {
	var id int
	err := s.db.QueryRow(`
		INSERT INTO warp_peers (api_key_id, pubkey, wg_ip, region)
		VALUES ($1,$2,$3,$4) RETURNING id`,
		p.APIKeyID, p.Pubkey, p.WGIP, p.Region).Scan(&id)
	return id, err
}

func (s *PostgresStore) GetWarpPeerByPubkey(pubkey string) (*WarpPeer, error) {
	var p WarpPeer
	err := s.db.QueryRow(`
		SELECT id, api_key_id, pubkey, wg_ip, region, created_at, COALESCE(assigned_leader, '')
		FROM warp_peers WHERE pubkey = $1`, pubkey).
		Scan(&p.ID, &p.APIKeyID, &p.Pubkey, &p.WGIP, &p.Region, &p.CreatedAt, &p.AssignedLeader)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *PostgresStore) ListWarpPeersByKey(apiKeyID int) ([]WarpPeer, error) {
	rows, err := s.db.Query(`
		SELECT id, api_key_id, pubkey, wg_ip, region, created_at, COALESCE(assigned_leader, '')
		FROM warp_peers WHERE api_key_id = $1 ORDER BY id`, apiKeyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWarpPeers(rows)
}

func (s *PostgresStore) ListAllWarpPeers() ([]WarpPeer, error) {
	rows, err := s.db.Query(`
		SELECT id, api_key_id, pubkey, wg_ip, region, created_at, COALESCE(assigned_leader, '')
		FROM warp_peers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWarpPeers(rows)
}

// ListWarpPeersByRegion returns every peer pinned to a region — the full peer set
// a leader of that region must hold (used by the per-leader resync push).
func (s *PostgresStore) ListWarpPeersByRegion(region string) ([]WarpPeer, error) {
	rows, err := s.db.Query(`
		SELECT id, api_key_id, pubkey, wg_ip, region, created_at, COALESCE(assigned_leader, '')
		FROM warp_peers WHERE region = $1 ORDER BY id`, region)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWarpPeers(rows)
}

// SetWarpPeerAssignedLeader pins a peer's F3 "home" leader. An empty leaderID
// unpins it (reverts to freest-first ordering). Called only by the rebalancer
// worker (armed mode) and the enroll path.
func (s *PostgresStore) SetWarpPeerAssignedLeader(pubkey, leaderID string) error {
	_, err := s.db.Exec(`UPDATE warp_peers SET assigned_leader = $2 WHERE pubkey = $1`, pubkey, leaderID)
	return err
}

// CountWarpPeersByRegion returns the peer count per region (region -> count), used
// to pick the least-loaded region at enroll.
func (s *PostgresStore) CountWarpPeersByRegion() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT region, COUNT(*) FROM warp_peers GROUP BY region`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var region string
		var n int
		if err := rows.Scan(&region, &n); err != nil {
			return nil, err
		}
		out[region] = n
	}
	return out, rows.Err()
}

func (s *PostgresStore) DeleteWarpPeerByPubkey(pubkey string) error {
	_, err := s.db.Exec(`DELETE FROM warp_peers WHERE pubkey = $1`, pubkey)
	return err
}

func scanWarpPeers(rows *sql.Rows) ([]WarpPeer, error) {
	var out []WarpPeer
	for rows.Next() {
		var p WarpPeer
		if err := rows.Scan(&p.ID, &p.APIKeyID, &p.Pubkey, &p.WGIP, &p.Region, &p.CreatedAt, &p.AssignedLeader); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- Regions ---

func (s *PostgresStore) ListWarpRegions() ([]WarpRegion, error) {
	rows, err := s.db.Query(`SELECT region, subnet, enabled, created_at FROM warp_regions ORDER BY region`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WarpRegion
	for rows.Next() {
		var r WarpRegion
		if err := rows.Scan(&r.Region, &r.Subnet, &r.Enabled, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PostgresStore) GetWarpRegion(region string) (*WarpRegion, error) {
	var r WarpRegion
	err := s.db.QueryRow(`SELECT region, subnet, enabled, created_at FROM warp_regions WHERE region = $1`, region).
		Scan(&r.Region, &r.Subnet, &r.Enabled, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// UpsertWarpRegion inserts or updates a region's subnet + enabled flag.
func (s *PostgresStore) UpsertWarpRegion(region, subnet string, enabled bool) error {
	_, err := s.db.Exec(`
		INSERT INTO warp_regions (region, subnet, enabled) VALUES ($1,$2,$3)
		ON CONFLICT (region) DO UPDATE SET subnet = EXCLUDED.subnet, enabled = EXCLUDED.enabled`,
		region, subnet, enabled)
	return err
}

func (s *PostgresStore) DeleteWarpRegion(region string) error {
	_, err := s.db.Exec(`DELETE FROM warp_regions WHERE region = $1`, region)
	return err
}

// --- Leaders ---

func (s *PostgresStore) ListWarpLeaders() ([]WarpLeader, error) {
	rows, err := s.db.Query(`SELECT leader_id, region, endpoint, enabled, created_at FROM warp_leaders ORDER BY region, leader_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWarpLeaders(rows)
}

func (s *PostgresStore) ListWarpLeadersByRegion(region string) ([]WarpLeader, error) {
	rows, err := s.db.Query(`SELECT leader_id, region, endpoint, enabled, created_at FROM warp_leaders WHERE region = $1 ORDER BY leader_id`, region)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWarpLeaders(rows)
}

// UpsertWarpLeader inserts or updates a leader's region, endpoint + enabled flag.
func (s *PostgresStore) UpsertWarpLeader(leaderID, region, endpoint string, enabled bool) error {
	_, err := s.db.Exec(`
		INSERT INTO warp_leaders (leader_id, region, endpoint, enabled) VALUES ($1,$2,$3,$4)
		ON CONFLICT (leader_id) DO UPDATE SET region = EXCLUDED.region, endpoint = EXCLUDED.endpoint, enabled = EXCLUDED.enabled`,
		leaderID, region, endpoint, enabled)
	return err
}

func (s *PostgresStore) DeleteWarpLeader(leaderID string) error {
	_, err := s.db.Exec(`DELETE FROM warp_leaders WHERE leader_id = $1`, leaderID)
	return err
}

func scanWarpLeaders(rows *sql.Rows) ([]WarpLeader, error) {
	var out []WarpLeader
	for rows.Next() {
		var l WarpLeader
		if err := rows.Scan(&l.LeaderID, &l.Region, &l.Endpoint, &l.Enabled, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SeedWarpRegionIfEmpty seeds the registry with a single region + leader when no
// regions exist yet, so an existing single-hub deployment keeps working unchanged.
// The seed region id equals the old leader id ("leader-01") so the WG key derived
// from CLUSTER_SECRET+region stays byte-identical to the pre-multi-hub key.
func (s *PostgresStore) SeedWarpRegionIfEmpty(region, subnet, leaderID, endpoint string) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM warp_regions`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if err := s.UpsertWarpRegion(region, subnet, true); err != nil {
		return err
	}
	if endpoint == "" {
		// No endpoint configured yet: still create the region, but skip the
		// leader row (operator sets the endpoint from the panel).
		return nil
	}
	return s.UpsertWarpLeader(leaderID, region, endpoint, true)
}

// ErrWarpLimitReached is returned by EnrollPeerTx when the key's connection
// limit is hit under a "block" policy.
var ErrWarpLimitReached = errors.New("warp connection limit reached")

// ErrWarpIPTaken is returned when a caller-pinned fixed WG IP is already allocated
// to another peer - a graceful form of the wg_ip UNIQUE violation.
var ErrWarpIPTaken = errors.New("warp fixed IP already allocated")

// warpEnrollLock serializes all warp enrollments cluster-wide via a Postgres
// transaction advisory lock, so concurrent enrolls (across N Cores) can never
// exceed a key's max connections or collide on an allocated IP. Enroll is rare,
// so global serialization is fine.
const warpEnrollLock int64 = 0x77617270 // "warp"

// EnrollPeerTx atomically enforces the key's connection limit, allocates a WG IP
// (via allocIP, given the set of currently-taken IPs), and inserts the peer —
// all under one transaction + advisory lock. For "kill_old" at the limit it
// evicts the oldest peer for the key and returns its pubkey in `evicted`.
// Returns ErrWarpLimitReached when the limit is hit under any non-"kill_old" policy.
// The peer pins to `region`; the IP comes from that region's subnet (allocIP),
// but the taken-set is global because region subnets never overlap.
func (s *PostgresStore) EnrollPeerTx(keyID, limit int, onNewConn, pubkey, fixedIP, region string, allocIP func(taken map[string]bool) (string, error)) (wgIP string, evicted string, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", "", err
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	if _, err = tx.Exec(`SELECT pg_advisory_xact_lock($1)`, warpEnrollLock); err != nil {
		return "", "", err
	}

	var count int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM warp_peers WHERE api_key_id = $1`, keyID).Scan(&count); err != nil {
		return "", "", err
	}
	if count >= limit {
		if onNewConn != "kill_old" {
			return "", "", ErrWarpLimitReached
		}
		if err = tx.QueryRow(
			`DELETE FROM warp_peers
			 WHERE id = (SELECT id FROM warp_peers WHERE api_key_id = $1 ORDER BY id LIMIT 1)
			 RETURNING pubkey`, keyID).Scan(&evicted); err != nil {
			return "", "", err
		}
	}

	if fixedIP != "" {
		// A caller-pinned fixed IP bypasses allocIP. Reject it here - race-safe under
		// the advisory lock held above - if it is already allocated, instead of
		// surfacing a raw wg_ip UNIQUE violation from the INSERT below.
		var count int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM warp_peers WHERE wg_ip = $1`, fixedIP).Scan(&count); err != nil {
			return "", "", err
		}
		if count > 0 {
			return "", "", ErrWarpIPTaken
		}
		wgIP = fixedIP
	} else {
		taken := map[string]bool{}
		rows, qerr := tx.Query(`SELECT wg_ip FROM warp_peers`)
		if qerr != nil {
			return "", "", qerr
		}
		for rows.Next() {
			var ip string
			if serr := rows.Scan(&ip); serr != nil {
				rows.Close()
				return "", "", serr
			}
			taken[ip] = true
		}
		rows.Close()
		if rerr := rows.Err(); rerr != nil {
			return "", "", rerr
		}
		if wgIP, err = allocIP(taken); err != nil {
			return "", "", err
		}
	}

	if _, err = tx.Exec(
		`INSERT INTO warp_peers (api_key_id, pubkey, wg_ip, region) VALUES ($1,$2,$3,$4)`,
		keyID, pubkey, wgIP, region); err != nil {
		return "", "", err
	}
	if err = tx.Commit(); err != nil {
		return "", "", err
	}
	return wgIP, evicted, nil
}

// GetWarpAPIKeyByNodeID returns the warp key for a link identity (node_id),
// revoked or not. Revoked rows are included so a revoke whose best-effort Redis
// teardown failed can be retried and finish; authentication never goes through
// this lookup (WarpAPIKeyMiddleware checks revoked_at on the hash lookup).
func (s *PostgresStore) GetWarpAPIKeyByNodeID(nodeID string) (*WarpAPIKey, error) {
	var k WarpAPIKey
	var fixedIP, node, region, ownerID sql.NullString
	err := s.db.QueryRow(`
		SELECT id, name, key_hash, policy, max_conns, on_new_conn,
		       COALESCE(fixed_wg_ip,''), COALESCE(node_id,''), COALESCE(region,''),
		       COALESCE(owner_id::text,''), COALESCE(bound_node_id, 0), revoked_at, created_at
		FROM warp_api_keys WHERE node_id = $1`, nodeID).
		Scan(&k.ID, &k.Name, &k.KeyHash, &k.Policy, &k.MaxConns, &k.OnNewConn,
			&fixedIP, &node, &region, &ownerID, &k.BoundNodeID, &k.RevokedAt, &k.CreatedAt)
	if err != nil {
		return nil, err
	}
	k.FixedWGIP, k.NodeID, k.Region, k.OwnerID = fixedIP.String, node.String, region.String, ownerID.String
	return &k, nil
}

// BindWarpAPIKey ties a live, unbound key to a node. false (no error) when the
// key was revoked or bound in the meantime: the handler checked both before
// calling, and the WHERE is what makes that check hold under a race instead of
// quietly moving a key that already belonged to another machine. A second live
// key for the same node is refused by idx_warp_api_keys_bound_node and comes back
// as ErrWarpKeyNodeTaken - the handler's pre-check cannot see every such key: it
// lists the caller's own, so a key another owner bound before the machine changed
// hands, or the enrol-time bind racing this one, only shows up here.
func (s *PostgresStore) BindWarpAPIKey(keyID, nodeID int) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE warp_api_keys SET bound_node_id = $2
		 WHERE id = $1 AND bound_node_id IS NULL AND revoked_at IS NULL`, keyID, nodeID)
	if isUniqueViolation(err) {
		return false, ErrWarpKeyNodeTaken
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RevokeWarpAPIKeyByNodeID marks a link kit's warp key revoked (revoked_at = NOW),
// blocking warp re-enrollment and any future link-boot for that identity.
func (s *PostgresStore) RevokeWarpAPIKeyByNodeID(nodeID string) error {
	_, err := s.db.Exec(`UPDATE warp_api_keys SET revoked_at = NOW() WHERE node_id = $1 AND revoked_at IS NULL`, nodeID)
	return err
}

// RollWarpAPIKeyHash replaces the secret of a live key IN PLACE, keeping its
// row, its id and its node_id. sql.ErrNoRows when there is no live key with
// that identity, so a caller cannot read "rotated" off a no-op.
//
// An UPDATE and not revoke-then-insert, and that is load-bearing rather than a
// style choice: GetWarpAPIKeyByNodeID selects on node_id with NO revoked_at
// filter, so a second row carrying the same identity would make every lookup
// for that machine ambiguous - including the one warp authenticates through.
//
// Keeping the row is also the whole point of a roll. The id is what
// DisconnectKeyPeers and the ACL reconciler address, and node_id is what the
// customer has in their compose file, so only the secret has to travel. A mint
// generates a fresh identity instead, which would move the overlay address and
// force a second edit for no gain.
func (s *PostgresStore) RollWarpAPIKeyHash(nodeID, newHash string) error {
	res, err := s.db.Exec(
		`UPDATE warp_api_keys SET key_hash = $2 WHERE node_id = $1 AND revoked_at IS NULL`,
		nodeID, newHash)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
