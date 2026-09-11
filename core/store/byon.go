package store

import (
	"database/sql"
	"time"
)

// NodeEnrollToken is a per-user token a BYON tenant uses to enroll their own
// node. The plaintext is never stored (only its hash); these structs never carry
// the plaintext or the hash to the client.
type NodeEnrollToken struct {
	ID         string     `json:"id"`
	UserID     string     `json:"userId"`
	Label      string     `json:"label"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	ConsumedAt *time.Time `json:"consumedAt,omitempty"`
}

// CreateNodeEnrollToken stores a new enroll token (hashed) for a user.
// warpKeyNodeID is the BYON node key minted for the same machine, "" for none;
// redeeming the token binds that key to the node (BindWarpKeyFromEnrollToken).
func (s *PostgresStore) CreateNodeEnrollToken(userID, plaintext, label string, expiresAt *time.Time, warpKeyNodeID string) error {
	_, err := s.db.Exec(
		`INSERT INTO node_enroll_tokens (user_id, token_hash, label, expires_at, warp_key_node_id)
		 VALUES ($1, $2, $3, $4, NULLIF($5, ''))`,
		userID, hashAuthToken(plaintext), label, expiresAt, warpKeyNodeID)
	return err
}

// BindWarpKeyFromEnrollToken binds the node key an enroll token was minted for
// to the node that token just created. true when a key was bound; false (no
// error) for a token minted without one, and for a key that was revoked, bound
// elsewhere or never the token owner's since.
//
// Every condition is in the WHERE rather than read first, so the statement is
// safe to repeat and safe under a race: a second run finds bound_node_id set
// and changes nothing. The owner is checked on the key AND the node, so a token
// can only ever bind its own owner's key to its own owner's machine.
func (s *PostgresStore) BindWarpKeyFromEnrollToken(plaintext string, nodeID int) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE warp_api_keys k SET bound_node_id = $2
		 FROM node_enroll_tokens t, nodes n
		 WHERE t.token_hash = $1 AND t.warp_key_node_id = k.node_id
		   AND k.owner_id = t.user_id AND n.id = $2 AND n.owner_id = t.user_id
		   AND k.node_id LIKE 'node-%' AND k.bound_node_id IS NULL AND k.revoked_at IS NULL`,
		hashAuthToken(plaintext), nodeID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ResolveNodeEnrollToken returns the owning user id for a valid, unexpired,
// NON-RECOVERY enroll token. ok=false (no error) when the token is unknown,
// expired, or is a recovery token (recovers_node_token set): a recovery token
// is minted for ONE specific existing node identity (see CreateRecoveryToken)
// and must only be redeemable via the recovery branch (ResolveRecoveryToken +
// ConsumeNodeEnrollToken in the gRPC recovery flow), never as a generic
// new-node enroll token - otherwise it could mint a brand-new rogue node
// instead of re-pairing the identity it was scoped to.
func (s *PostgresStore) ResolveNodeEnrollToken(plaintext string) (userID string, ok bool, err error) {
	err = s.db.QueryRow(
		`SELECT user_id FROM node_enroll_tokens
		 WHERE token_hash = $1 AND consumed_at IS NULL AND recovers_node_token IS NULL
		   AND (expires_at IS NULL OR expires_at > NOW())`,
		hashAuthToken(plaintext)).Scan(&userID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return userID, true, nil
}

// ConsumeNodeEnrollToken atomically marks a valid, unexpired, still-unconsumed
// token as used and returns its owner + the node token it recovers (empty for a
// normal enroll token). Single-use.
func (s *PostgresStore) ConsumeNodeEnrollToken(plaintext string) (userID string, recoversNodeToken string, ok bool, err error) {
	err = s.db.QueryRow(
		`UPDATE node_enroll_tokens SET consumed_at = NOW()
		 WHERE token_hash = $1 AND consumed_at IS NULL AND (expires_at IS NULL OR expires_at > NOW())
		 RETURNING user_id, COALESCE(recovers_node_token, '')`,
		hashAuthToken(plaintext)).Scan(&userID, &recoversNodeToken)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return userID, recoversNodeToken, true, nil
}

// ResolveRecoveryToken reads a valid, unexpired, still-unconsumed RECOVERY token
// WITHOUT consuming it, returning the node token it re-pairs. ok=false if the token
// is unknown/expired/consumed or is a plain enroll token (empty recovers_node_token).
func (s *PostgresStore) ResolveRecoveryToken(plaintext string) (recoversNodeToken string, ok bool, err error) {
	err = s.db.QueryRow(
		`SELECT COALESCE(recovers_node_token, '') FROM node_enroll_tokens
		 WHERE token_hash = $1 AND consumed_at IS NULL AND (expires_at IS NULL OR expires_at > NOW())`,
		hashAuthToken(plaintext)).Scan(&recoversNodeToken)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if recoversNodeToken == "" {
		return "", false, nil
	}
	return recoversNodeToken, true, nil
}

// ListNodeEnrollTokens returns a user's tokens (metadata only, never the hash).
// CountPendingNodeEnrollTokens returns how many of a tenant's enroll tokens are
// still redeemable: not consumed and not expired. An unredeemed token is a
// pending node, which is why it counts against the same cap the node itself
// does - the twin of CountNodeWarpKeysByOwner, which counts unrevoked warp keys
// against max_nodes for exactly that reason.
//
// Recovery tokens are excluded: they re-pair a machine that already exists and
// is already counted, so counting them would refuse a re-pair to a tenant who is
// legitimately at their limit.
func (s *PostgresStore) CountPendingNodeEnrollTokens(userID string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM node_enroll_tokens
		 WHERE user_id = $1 AND consumed_at IS NULL AND recovers_node_token IS NULL
		   AND (expires_at IS NULL OR expires_at > NOW())`, userID).Scan(&n)
	return n, err
}

// Only tokens that can still be redeemed. The panel renders this list under
// "keys waiting to be used", and it used to return every row ever minted.
//
// Two things were wrong with that, and both were visible on a working account.
// Adding a machine mints an overlay key AND an enroll token under the same
// label; once the machine enrolled, the token was consumed but stayed on this
// list, so one machine showed as two entries that both offered a delete. And an
// expired token sat there forever claiming to be usable, while the cap beside it
// had already stopped counting it.
//
// Consumed and expired are exactly what CountPendingNodeEnrollTokens excludes,
// so the list and the number now agree. Recovery tokens are deliberately still
// listed: they are genuinely waiting to be used. They are excluded from the
// COUNT for a different reason - they re-pair a machine that is already counted.
func (s *PostgresStore) ListNodeEnrollTokens(userID string) ([]NodeEnrollToken, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, label, created_at, expires_at, consumed_at
		 FROM node_enroll_tokens
		 WHERE user_id = $1 AND consumed_at IS NULL
		   AND (expires_at IS NULL OR expires_at > NOW())
		 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeEnrollToken
	for rows.Next() {
		var t NodeEnrollToken
		var exp, consumed sql.NullTime
		if err := rows.Scan(&t.ID, &t.UserID, &t.Label, &t.CreatedAt, &exp, &consumed); err != nil {
			return nil, err
		}
		if exp.Valid {
			t.ExpiresAt = &exp.Time
		}
		if consumed.Valid {
			t.ConsumedAt = &consumed.Time
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteNodeEnrollToken revokes a token, scoped to its owner so a tenant can only
// delete their own.
func (s *PostgresStore) DeleteNodeEnrollToken(id, userID string) error {
	_, err := s.db.Exec(`DELETE FROM node_enroll_tokens WHERE id = $1 AND user_id = $2`, id, userID)
	return err
}

// CreateRecoveryToken stores a single-use recovery token (hashed) bound to an
// EXISTING node identity (nodeToken = nodes.token). On consume, the recovery
// branch of the gRPC handshake re-pairs that node under the same identity.
func (s *PostgresStore) CreateRecoveryToken(userID, plaintext, nodeToken string, expiresAt *time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO node_enroll_tokens (user_id, token_hash, label, expires_at, recovers_node_token)
		 VALUES ($1, $2, $3, $4, $5)`,
		userID, hashAuthToken(plaintext), "recovery", expiresAt, nodeToken)
	return err
}

// AdmissionCIDR is one global-scope allowlist entry for NEW node registrations.
type AdmissionCIDR struct {
	ID        string    `json:"id"`
	CIDR      string    `json:"cidr"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"createdAt"`
}

// AddAdmissionCIDR inserts a normalized CIDR (validated by the handler).
func (s *PostgresStore) AddAdmissionCIDR(cidr, label string) error {
	_, err := s.db.Exec(`INSERT INTO node_admission_cidrs (cidr, label) VALUES ($1, $2)`, cidr, label)
	return err
}

// ListAdmissionCIDRs returns all admission CIDRs, newest first.
func (s *PostgresStore) ListAdmissionCIDRs() ([]AdmissionCIDR, error) {
	rows, err := s.db.Query(`SELECT id, cidr, label, created_at FROM node_admission_cidrs ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdmissionCIDR
	for rows.Next() {
		var c AdmissionCIDR
		if err := rows.Scan(&c.ID, &c.CIDR, &c.Label, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteAdmissionCIDR removes one CIDR by id.
func (s *PostgresStore) DeleteAdmissionCIDR(id string) error {
	_, err := s.db.Exec(`DELETE FROM node_admission_cidrs WHERE id = $1`, id)
	return err
}
