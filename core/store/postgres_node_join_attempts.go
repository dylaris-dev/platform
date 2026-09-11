package store

import (
	"database/sql"
	"time"

	"dylaris-core/models"
)

// The window an admission stays armed for.
//
// Short on purpose. A refused node retries every thirty seconds and forever, so
// the operator does not need a long window - they need one that closes on its
// own if they walk away. The identity in a join attempt is self-claimed, so an
// approval left standing is a door held open for whoever knocks with that id
// next.
const nodeJoinApprovalWindow = 15 * time.Minute

// RecordNodeJoinAttempt writes a refused connection, or folds it into the row
// that is already there.
//
// One row per identity, not one per attempt. A node refused at 03:00 is still
// being refused at 09:00 having tried seven hundred times, and seven hundred
// rows would say nothing the count and the two timestamps do not.
//
// A recorded attempt never clears an armed approval: the approval is consumed by
// a SUCCESSFUL admission, and a node that is refused once more while the window
// is open (the wrong address, say) must not close its own door.
func (s *PostgresStore) RecordNodeJoinAttempt(a models.NodeJoinAttempt) error {
	_, err := s.db.Exec(`
		INSERT INTO node_join_attempts (
			node_token, peer_ip, reported_public_ip, reported_private_ips,
			hostname, cpu_cores, cpu_model, memory_bytes, release_version,
			reason, attempts, first_seen_at, last_seen_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,1,NOW(),NOW())
		ON CONFLICT (node_token) DO UPDATE SET
			peer_ip = EXCLUDED.peer_ip,
			reported_public_ip = EXCLUDED.reported_public_ip,
			reported_private_ips = EXCLUDED.reported_private_ips,
			hostname = EXCLUDED.hostname,
			cpu_cores = EXCLUDED.cpu_cores,
			cpu_model = EXCLUDED.cpu_model,
			memory_bytes = EXCLUDED.memory_bytes,
			release_version = EXCLUDED.release_version,
			reason = EXCLUDED.reason,
			attempts = node_join_attempts.attempts + 1,
			last_seen_at = NOW()`,
		a.NodeToken, a.PeerIP, a.ReportedPublicIP, a.ReportedPrivateIPs,
		a.Hostname, a.CPUCores, a.CPUModel, a.MemoryBytes, a.ReleaseVersion, a.Reason)
	return err
}

// ListNodeJoinAttempts returns the refused connections, newest first, joined to
// the node row so the panel can name the machine the operator already knows.
func (s *PostgresStore) ListNodeJoinAttempts() ([]models.NodeJoinAttempt, error) {
	rows, err := s.db.Query(`
		SELECT a.node_token, COALESCE(n.name, ''), COALESCE(n.display_name, ''),
			a.peer_ip, a.reported_public_ip, a.reported_private_ips,
			a.hostname, a.cpu_cores, a.cpu_model, a.memory_bytes, a.release_version,
			a.reason, a.attempts, a.first_seen_at, a.last_seen_at,
			a.approved_until, a.approved_from_ip, a.approved_by
		FROM node_join_attempts a
		LEFT JOIN nodes n ON n.token = a.node_token
		ORDER BY a.last_seen_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []models.NodeJoinAttempt{}
	for rows.Next() {
		var a models.NodeJoinAttempt
		var approvedUntil sql.NullTime
		if err := rows.Scan(&a.NodeToken, &a.NodeName, &a.DisplayName,
			&a.PeerIP, &a.ReportedPublicIP, &a.ReportedPrivateIPs,
			&a.Hostname, &a.CPUCores, &a.CPUModel, &a.MemoryBytes, &a.ReleaseVersion,
			&a.Reason, &a.Attempts, &a.FirstSeenAt, &a.LastSeenAt,
			&approvedUntil, &a.ApprovedFromIP, &a.ApprovedBy); err != nil {
			continue
		}
		if approvedUntil.Valid {
			t := approvedUntil.Time
			a.ApprovedUntil = &t
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ApproveNodeJoinAttempt arms an admission for one identity from the address it
// was last seen at.
//
// The address is taken from the ROW rather than from the request, so an operator
// approves the machine they were shown. Passing it in would let the two drift
// between the render and the click - and it is the only field on that screen
// that cannot be forged, so it is the one that must not be re-supplied.
func (s *PostgresStore) ApproveNodeJoinAttempt(nodeToken, approvedBy string) (bool, error) {
	res, err := s.db.Exec(`
		UPDATE node_join_attempts
		SET approved_until = NOW() + $2::interval,
			approved_from_ip = peer_ip,
			approved_by = $3
		WHERE node_token = $1 AND peer_ip <> ''`,
		nodeToken, nodeJoinApprovalWindow.String(), approvedBy)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ArmNodeJoinApproval arms the same admission ApproveNodeJoinAttempt does, for
// a node that has NOT been refused yet: the roll-key action clears a working
// node's secret and lets it straight back in from where it last authenticated.
//
// fromIP is the address of that last successful authentication, which Core read
// off the socket, so it is as unforgeable as the peer_ip an approval binds to.
// Same window, same one-shot consume, same address binding - an admission armed
// here is never weaker than one granted from the list. An empty address arms
// nothing: an admission for any source is exactly what this must not produce.
//
// An existing row keeps what it recorded about the refusals; only the admission
// is written. A new row is not a refusal, so it says so and counts none.
func (s *PostgresStore) ArmNodeJoinApproval(nodeToken, fromIP, approvedBy string) (bool, error) {
	if fromIP == "" {
		return false, nil
	}
	res, err := s.db.Exec(`
		INSERT INTO node_join_attempts (
			node_token, peer_ip, reason, attempts,
			approved_until, approved_from_ip, approved_by)
		VALUES ($1, $2, 'Its key was rolled in the panel. Waiting for it to reconnect.', 0,
			NOW() + $3::interval, $2, $4)
		ON CONFLICT (node_token) DO UPDATE SET
			approved_until = EXCLUDED.approved_until,
			approved_from_ip = EXCLUDED.approved_from_ip,
			approved_by = EXCLUDED.approved_by`,
		nodeToken, fromIP, nodeJoinApprovalWindow.String(), approvedBy)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ConsumeNodeJoinApproval answers whether this identity may be admitted from
// this address, and closes the door behind it.
//
// One statement, not a read then a write: Core runs as several replicas and a
// node dials whichever answers, so a check-then-clear could admit the same
// approval twice. The UPDATE ... RETURNING makes the clear the same act as the
// check, and only the replica whose UPDATE matched a row gets a true.
func (s *PostgresStore) ConsumeNodeJoinApproval(nodeToken, peerIP string) (bool, error) {
	var token string
	err := s.db.QueryRow(`
		UPDATE node_join_attempts
		SET approved_until = NULL, approved_from_ip = ''
		WHERE node_token = $1
			-- The emptiness check is on the COLUMN, not on the parameter. Asking
			-- it of $2 as well made the placeholder compare to a column in one
			-- clause and to an untyped literal in another, which Postgres cannot
			-- type and refuses to prepare. It says the same thing: a row with no
			-- observed address matches nothing, so an unidentifiable caller
			-- cannot be admitted by a blank on both sides.
			AND approved_from_ip <> '' AND approved_from_ip = $2
			AND approved_until IS NOT NULL AND approved_until > NOW()
		RETURNING node_token`, nodeToken, peerIP).Scan(&token)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// DeleteNodeJoinAttempt removes a row. Called when a node connects successfully,
// so a list of refusals does not keep showing a machine that is back, and by the
// panel's dismiss action.
func (s *PostgresStore) DeleteNodeJoinAttempt(nodeToken string) error {
	_, err := s.db.Exec(`DELETE FROM node_join_attempts WHERE node_token = $1`, nodeToken)
	return err
}
