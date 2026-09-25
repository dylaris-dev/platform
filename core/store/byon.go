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

// CreatePlatformNodeEnrollToken stores the enroll token of an External node: a
// machine the PLATFORM runs outside the datacenter. The node it enrols is born
// unowned (Handshake.Enroll), so minterID only records which admin minted it.
// warpKeyNodeID is the owner-less node key minted in the same call; a platform
// token is redeemable only while that key is live (ConsumeNodeEnrollToken).
//
// Its own method rather than a flag on CreateNodeEnrollToken, so the one place
// that can set platform is the admin endpoint calling this, and no tenant path
// reaches it by passing a boolean.
func (s *PostgresStore) CreatePlatformNodeEnrollToken(minterID, plaintext, label string, expiresAt *time.Time, warpKeyNodeID string) error {
	_, err := s.db.Exec(
		`INSERT INTO node_enroll_tokens (user_id, token_hash, label, expires_at, warp_key_node_id, platform)
		 VALUES ($1, $2, $3, $4, $5, TRUE)`,
		minterID, hashAuthToken(plaintext), label, expiresAt, warpKeyNodeID)
	return err
}

// BindWarpKeyFromEnrollToken binds the node key an enroll token was minted for
// to the node that token just created. true when a key was bound; false (no
// error) for a token minted without one, and for a key that was revoked, bound
// elsewhere or never the token owner's since.
//
// Every condition is in the WHERE rather than read first, so the statement is
// safe to repeat and safe under a race: a second run finds bound_node_id set
// and changes nothing.
//
// Ownership has two shapes and one statement. A tenant's token binds only its
// owner's key to its owner's machine, checked on the key AND the node. A
// platform token binds only an owner-less key to an owner-less machine. The
// cross cases bind nothing: a platform token naming a tenant's key would hand
// that tenant's Link to a machine they do not hold, and a tenant's token naming
// an owner-less key would put a platform credential on a customer's machine.
// owner_id is a nullable UUID on both tables, so "no owner" is IS NULL, and an
// equality against NULL is never true - which is what keeps the first arm from
// matching an owner-less pair.
func (s *PostgresStore) BindWarpKeyFromEnrollToken(plaintext string, nodeID int) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE warp_api_keys k SET bound_node_id = $2
		 FROM node_enroll_tokens t, nodes n
		 WHERE t.token_hash = $1 AND t.warp_key_node_id = k.node_id AND n.id = $2
		   AND ((NOT t.platform AND k.owner_id = t.user_id AND n.owner_id = t.user_id)
		     OR (t.platform AND k.owner_id IS NULL AND n.owner_id IS NULL))
		   AND k.node_id LIKE 'node-%' AND k.bound_node_id IS NULL AND k.revoked_at IS NULL`,
		hashAuthToken(plaintext), nodeID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ResolveNodeEnrollToken returns the owning user id for a valid, unexpired,
// NON-RECOVERY enroll token, and whether it is a platform token (see
// CreatePlatformNodeEnrollToken). Nothing updates that flag after the insert,
// so reading it here cannot race the consume. ok=false (no error) when the token is unknown,
// expired, or is a recovery token (recovers_node_token set): a recovery token
// is minted for ONE specific existing node identity (see CreateRecoveryToken)
// and must only be redeemable via the recovery branch (ResolveRecoveryToken +
// ConsumeNodeEnrollToken in the gRPC recovery flow), never as a generic
// new-node enroll token - otherwise it could mint a brand-new rogue node
// instead of re-pairing the identity it was scoped to.
func (s *PostgresStore) ResolveNodeEnrollToken(plaintext string) (userID string, platform bool, ok bool, err error) {
	err = s.db.QueryRow(
		`SELECT user_id, platform FROM node_enroll_tokens
		 WHERE token_hash = $1 AND consumed_at IS NULL AND recovers_node_token IS NULL
		   AND (expires_at IS NULL OR expires_at > NOW())`,
		hashAuthToken(plaintext)).Scan(&userID, &platform)
	if err == sql.ErrNoRows {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, err
	}
	return userID, platform, true, nil
}

// ConsumeNodeEnrollToken atomically marks a valid, unexpired, still-unconsumed
// token as used and returns its owner + the node token it recovers (empty for a
// normal enroll token). Single-use.
//
// A platform token is redeemable only while the owner-less node key minted with
// it is live. That is what makes revoking or deleting the key in the admin Warp
// list also kill a token nobody has used yet: the admin sees one row for the
// machine, and there is no second thing to find and revoke. Asked here rather
// than done at revoke time, so every way a key stops being live covers the
// token without having to know the token exists.
//
// Only a token not yet consumed is affected: one the machine already redeemed
// is spent, and the node it enrolled is removed separately. And deleting the
// admin who minted a platform token removes it while it is unredeemed
// (node_enroll_tokens.user_id is ON DELETE CASCADE), whereas the owner-less key
// minted with it stays, since it has no owner to cascade from.
func (s *PostgresStore) ConsumeNodeEnrollToken(plaintext string) (userID string, recoversNodeToken string, ok bool, err error) {
	err = s.db.QueryRow(
		`UPDATE node_enroll_tokens SET consumed_at = NOW()
		 WHERE token_hash = $1 AND consumed_at IS NULL AND (expires_at IS NULL OR expires_at > NOW())
		   AND (NOT platform OR EXISTS (SELECT 1 FROM warp_api_keys k
		        WHERE k.node_id = node_enroll_tokens.warp_key_node_id
		          AND k.owner_id IS NULL AND k.revoked_at IS NULL))
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
//
// Platform tokens are excluded as well: user_id on one is the admin who minted
// it, and the machine it enrols belongs to nobody, so it must not take a slot of
// that admin's own plan.
func (s *PostgresStore) CountPendingNodeEnrollTokens(userID string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM node_enroll_tokens
		 WHERE user_id = $1 AND NOT platform AND consumed_at IS NULL AND recovers_node_token IS NULL
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
//
// Platform tokens are not listed: this is the caller's own "My infrastructure"
// list, and an admin's External node token would sit there as a pending machine
// of their own, with a delete button beside it.
func (s *PostgresStore) ListNodeEnrollTokens(userID string) ([]NodeEnrollToken, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, label, created_at, expires_at, consumed_at
		 FROM node_enroll_tokens
		 WHERE user_id = $1 AND NOT platform AND consumed_at IS NULL
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
// DeleteNodeEnrollToken removes one of THIS user's enroll tokens and reports
// whether there was one to remove.
//
// The bool is the point. The delete has always been scoped by user id, so a
// stranger's id never matched and nothing was ever removed across accounts -
// but the handler answered "success" all the same, to anyone, for any id. A
// caller who mistyped an id was told their token was revoked while it was
// still live, which in a credential-revocation path is the wrong way round.
func (s *PostgresStore) DeleteNodeEnrollToken(id, userID string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM node_enroll_tokens WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		// The driver could not say. Reporting "removed" would be a guess in the
		// direction that lets somebody believe a live token is gone.
		return false, nil
	}
	return n > 0, nil
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
