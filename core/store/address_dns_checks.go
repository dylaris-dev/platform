package store

import (
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// AddressDNSCheck is the last DNS pointing check of an address on a customer's
// own domain. Status is a verdict of services.Pointing (Known), never
// lookup_failed or rate_limited. FailingSince and Misses describe the current
// run of definitive misses; both are reset by an ok.
type AddressDNSCheck struct {
	Name, Status, Region string
	CheckedAt            time.Time
	FailingSince         *time.Time
	Misses               int
}

// addressDNSVerdicts mirrors services.Pointing.Known (store cannot import
// services): a whitelist, so nothing a caller makes up is ever stored.
var addressDNSVerdicts = map[string]bool{
	"ok": true, "not_found": true, "elsewhere": true, "partial": true,
	"multi_region": true, "ipv6": true, "srv_only": true, "private": true,
}

// scanAddressDNSCheck reads the columns every query here selects, in order:
// name, status, region, checked_at, failing_since, misses.
func scanAddressDNSCheck(row interface{ Scan(...any) error }) (AddressDNSCheck, error) {
	var c AddressDNSCheck
	err := row.Scan(&c.Name, &c.Status, &c.Region, &c.CheckedAt, &c.FailingSince, &c.Misses)
	return c, err
}

// RecordAddressDNSCheck stores a verdict for name. ok resets the run of misses
// (misses 0, failing_since NULL); a miss adds one and keeps the start of the
// run, or starts it now. A status that is not a verdict ("could not check")
// is an error and stores nothing.
func (s *PostgresStore) RecordAddressDNSCheck(name, status, region string, ok bool) (AddressDNSCheck, error) {
	if !addressDNSVerdicts[status] {
		return AddressDNSCheck{}, fmt.Errorf("address dns check %s: %q is not a verdict", name, status)
	}
	return scanAddressDNSCheck(s.db.QueryRow(`
		INSERT INTO address_dns_checks (name, status, region, checked_at, failing_since, misses)
		VALUES ($1, $2, $3, NOW(), CASE WHEN $4::boolean THEN NULL ELSE NOW() END, CASE WHEN $4::boolean THEN 0 ELSE 1 END)
		ON CONFLICT (name) DO UPDATE SET
			status        = EXCLUDED.status,
			region        = EXCLUDED.region,
			checked_at    = NOW(),
			failing_since = CASE WHEN $4::boolean THEN NULL ELSE COALESCE(address_dns_checks.failing_since, NOW()) END,
			misses        = CASE WHEN $4::boolean THEN 0 ELSE address_dns_checks.misses + 1 END
		RETURNING name, status, region, checked_at, failing_since, misses`,
		strings.ToLower(name), status, region, ok))
}

// GetAddressDNSChecks is the stored check of each of names that has one, keyed
// by the stored (lower case) name. One query for a whole list.
func (s *PostgresStore) GetAddressDNSChecks(names []string) (map[string]AddressDNSCheck, error) {
	out := map[string]AddressDNSCheck{}
	if len(names) == 0 {
		return out, nil
	}
	lower := make([]string, len(names))
	for i, n := range names {
		lower[i] = strings.ToLower(n)
	}
	rows, err := s.db.Query(`SELECT name, status, region, checked_at, failing_since, misses FROM address_dns_checks WHERE name = ANY($1)`, pq.Array(lower))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanAddressDNSCheck(rows)
		if err != nil {
			return nil, err
		}
		out[c.Name] = c
	}
	return out, rows.Err()
}

// ListAddressDNSChecksDue is up to limit rows last checked before before,
// oldest first.
func (s *PostgresStore) ListAddressDNSChecksDue(before time.Time, limit int) ([]AddressDNSCheck, error) {
	rows, err := s.db.Query(`SELECT name, status, region, checked_at, failing_since, misses FROM address_dns_checks
		WHERE checked_at < $1 ORDER BY checked_at, name LIMIT $2`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AddressDNSCheck
	for rows.Next() {
		c, err := scanAddressDNSCheck(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteAddressDNSChecksNotIn removes the rows of every name not in names (an
// address that no longer needs a check). An empty names removes every row, so
// a caller must not pass the result of a failed read.
func (s *PostgresStore) DeleteAddressDNSChecksNotIn(names []string) (int64, error) {
	lower := make([]string, len(names))
	for i, n := range names {
		lower[i] = strings.ToLower(n)
	}
	res, err := s.db.Exec(`DELETE FROM address_dns_checks WHERE NOT (name = ANY($1))`, pq.Array(lower))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
