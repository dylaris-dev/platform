package store

import (
	"database/sql"
	"errors"
	"time"
)

// Custom-domain claim states.
const (
	ClaimPending      = "pending"
	ClaimVerified     = "verified"
	ClaimBlocked      = "blocked"
	ClaimPermablocked = "permablocked"
)

// MaxClaimAttempts is how many times a user may fail to prove ONE domain before
// the block becomes permanent. Two: the first miss is a mistake, the second is
// a pattern.
const MaxClaimAttempts = 2

// CustomDomainClaim is one user's standing claim on one domain.
type CustomDomainClaim struct {
	ID         int
	UserID     string
	Domain     string
	State      string
	Attempts   int
	DeadlineAt *time.Time
	TXTToken   string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	// CheckedAt is the last re-check of a verified claim; FailingSince the
	// first miss in the current run of them, nil while the record is there.
	CheckedAt    *time.Time
	FailingSince *time.Time
}

// ErrNoClaim is returned when a (user, domain) pair has no row yet.
var ErrNoClaim = errors.New("no custom domain claim")

func scanClaim(row interface{ Scan(...interface{}) error }) (*CustomDomainClaim, error) {
	var c CustomDomainClaim
	var deadline, checked, failing sql.NullTime
	err := row.Scan(&c.ID, &c.UserID, &c.Domain, &c.State, &c.Attempts, &deadline,
		&c.TXTToken, &c.CreatedAt, &c.UpdatedAt, &checked, &failing)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoClaim
	}
	if err != nil {
		return nil, err
	}
	if deadline.Valid {
		c.DeadlineAt = &deadline.Time
	}
	if checked.Valid {
		c.CheckedAt = &checked.Time
	}
	if failing.Valid {
		c.FailingSince = &failing.Time
	}
	return &c, nil
}

const claimCols = `id, user_id, domain, state, attempts, deadline_at, txt_token, created_at, updated_at, checked_at, failing_since`

// GetCustomDomainClaim returns one user's claim on one domain.
func (s *PostgresStore) GetCustomDomainClaim(userID, domain string) (*CustomDomainClaim, error) {
	return scanClaim(s.db.QueryRow(
		`SELECT `+claimCols+` FROM custom_domain_claims WHERE user_id = $1 AND domain = $2`,
		userID, domain))
}

// StartCustomDomainClaim records a fresh pending claim, or re-arms an existing
// one that is allowed another try.
//
// It refuses to re-arm a permanently blocked claim: that is what the TXT
// self-service path is for, and letting a plain retry clear it would make the
// permanent block a speed bump.
func (s *PostgresStore) StartCustomDomainClaim(userID, domain string, deadline time.Time) (*CustomDomainClaim, error) {
	_, err := s.db.Exec(`
		INSERT INTO custom_domain_claims (user_id, domain, state, deadline_at, updated_at)
		VALUES ($1, $2, 'pending', $3, NOW())
		ON CONFLICT (user_id, domain) DO UPDATE
		   SET state = 'pending', deadline_at = $3, updated_at = NOW()
		 WHERE custom_domain_claims.state <> 'permablocked'`,
		userID, domain, deadline)
	if err != nil {
		return nil, err
	}
	return s.GetCustomDomainClaim(userID, domain)
}

// MarkCustomDomainVerified records a proven claim and clears the deadline. The
// attempt counter is reset too: the domain is proven, so an older miss should
// not count toward a future permanent block.
//
// The token is KEPT. It used to be cleared here, which left nothing to re-check
// the claim against, so a domain proven once stayed proven for that account
// after it expired or was sold. A successful re-check comes through here too
// and ends a run of misses.
func (s *PostgresStore) MarkCustomDomainVerified(id int) error {
	_, err := s.db.Exec(
		`UPDATE custom_domain_claims
		    SET state = 'verified', deadline_at = NULL, attempts = 0,
		        checked_at = NOW(), failing_since = NULL, updated_at = NOW()
		  WHERE id = $1`, id)
	return err
}

// ListClaimsDueRecheck returns verified claims whose last re-check is older
// than every, or which are failing (those are looked at on every pass, so a
// fixed record ends the run of misses quickly). Oldest first, at most limit.
func (s *PostgresStore) ListClaimsDueRecheck(every time.Duration, limit int) ([]CustomDomainClaim, error) {
	return s.queryClaims(
		`SELECT `+claimCols+` FROM custom_domain_claims
		  WHERE state = 'verified'
		    AND (failing_since IS NOT NULL OR checked_at IS NULL
		         OR checked_at <= NOW() - make_interval(secs => $1))
		  ORDER BY (failing_since IS NULL), checked_at NULLS FIRST, id
		  LIMIT $2`, every.Seconds(), limit)
}

// RecheckFailedCustomDomainClaim records a re-check that did not find the
// record and returns when the current run of misses began - the first miss
// starts it, later ones keep it. ErrNoClaim when the claim is no longer
// verified.
func (s *PostgresStore) RecheckFailedCustomDomainClaim(id int) (time.Time, error) {
	var since time.Time
	err := s.db.QueryRow(`
		UPDATE custom_domain_claims
		   SET failing_since = COALESCE(failing_since, NOW()), checked_at = NOW(), updated_at = NOW()
		 WHERE id = $1 AND state = 'verified'
		 RETURNING failing_since`, id).Scan(&since)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrNoClaim
	}
	return since, err
}

// LapseCustomDomainClaim takes the proof away from a verified claim whose
// record has been missing for at least grace. It becomes blocked WITHOUT a
// strike: a domain that expired or changed hands is not a failed attempt, and
// the account may prove it again. Re-adding a route re-arms it like any other
// blocked claim.
//
// Only while it is STILL verified and still failing that long: a "check now"
// between the verifier's read and this write must win. ErrNoClaim then.
func (s *PostgresStore) LapseCustomDomainClaim(id int, grace time.Duration) error {
	res, err := s.db.Exec(`
		UPDATE custom_domain_claims
		   SET state = 'blocked', failing_since = NULL, updated_at = NOW()
		 WHERE id = $1 AND state = 'verified'
		   AND failing_since IS NOT NULL AND failing_since <= NOW() - make_interval(secs => $2)`,
		id, grace.Seconds())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoClaim
	}
	return nil
}

// FailCustomDomainClaim counts one missed deadline and returns the resulting
// state. The second failure is permanent.
//
// Only a claim that is STILL pending and past its deadline: the verifier
// decides on a list read at the start of a pass, and in between the customer
// may have verified it ("check now") or re-armed it with a fresh deadline.
// Failing it anyway overwrote a verified claim with a block. ErrNoClaim means
// the claim moved on and there is nothing to fail.
func (s *PostgresStore) FailCustomDomainClaim(id int) (string, error) {
	var state string
	err := s.db.QueryRow(`
		UPDATE custom_domain_claims
		   SET attempts = attempts + 1,
		       state = CASE WHEN attempts + 1 >= $2 THEN 'permablocked' ELSE 'blocked' END,
		       deadline_at = NULL,
		       updated_at = NOW()
		 WHERE id = $1 AND state = 'pending'
		   AND deadline_at IS NOT NULL AND deadline_at <= NOW()
		 RETURNING state`, id, MaxClaimAttempts).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoClaim
	}
	return state, err
}

// ListExpiredPendingClaims returns pending claims whose deadline has passed.
func (s *PostgresStore) ListExpiredPendingClaims(now time.Time) ([]CustomDomainClaim, error) {
	return s.queryClaims(
		`SELECT `+claimCols+` FROM custom_domain_claims
		  WHERE state = 'pending' AND deadline_at IS NOT NULL AND deadline_at <= $1`, now)
}

// ListPendingClaims returns every claim still awaiting proof.
func (s *PostgresStore) ListPendingClaims() ([]CustomDomainClaim, error) {
	return s.queryClaims(`SELECT ` + claimCols + ` FROM custom_domain_claims WHERE state = 'pending'`)
}

// ListCustomDomainClaimsByUser returns everything a user has claimed, so the
// panel can show state without the caller guessing domains.
func (s *PostgresStore) ListCustomDomainClaimsByUser(userID string) ([]CustomDomainClaim, error) {
	return s.queryClaims(
		`SELECT `+claimCols+` FROM custom_domain_claims WHERE user_id = $1 ORDER BY domain`, userID)
}

func (s *PostgresStore) queryClaims(q string, args ...interface{}) ([]CustomDomainClaim, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CustomDomainClaim{}
	for rows.Next() {
		c, err := scanClaim(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// SetCustomDomainTXTToken stores the self-service unblock token.
func (s *PostgresStore) SetCustomDomainTXTToken(id int, token string) error {
	_, err := s.db.Exec(
		`UPDATE custom_domain_claims SET txt_token = $2, updated_at = NOW() WHERE id = $1`, id, token)
	return err
}
