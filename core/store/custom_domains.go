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
}

// ErrNoClaim is returned when a (user, domain) pair has no row yet.
var ErrNoClaim = errors.New("no custom domain claim")

func scanClaim(row *sql.Row) (*CustomDomainClaim, error) {
	var c CustomDomainClaim
	var deadline sql.NullTime
	err := row.Scan(&c.ID, &c.UserID, &c.Domain, &c.State, &c.Attempts, &deadline,
		&c.TXTToken, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoClaim
	}
	if err != nil {
		return nil, err
	}
	if deadline.Valid {
		c.DeadlineAt = &deadline.Time
	}
	return &c, nil
}

const claimCols = `id, user_id, domain, state, attempts, deadline_at, txt_token, created_at, updated_at`

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
func (s *PostgresStore) MarkCustomDomainVerified(id int) error {
	_, err := s.db.Exec(
		`UPDATE custom_domain_claims
		    SET state = 'verified', deadline_at = NULL, attempts = 0, txt_token = '', updated_at = NOW()
		  WHERE id = $1`, id)
	return err
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
		var c CustomDomainClaim
		var deadline sql.NullTime
		if err := rows.Scan(&c.ID, &c.UserID, &c.Domain, &c.State, &c.Attempts, &deadline,
			&c.TXTToken, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		if deadline.Valid {
			c.DeadlineAt = &deadline.Time
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetCustomDomainTXTToken stores the self-service unblock token.
func (s *PostgresStore) SetCustomDomainTXTToken(id int, token string) error {
	_, err := s.db.Exec(
		`UPDATE custom_domain_claims SET txt_token = $2, updated_at = NOW() WHERE id = $1`, id, token)
	return err
}
