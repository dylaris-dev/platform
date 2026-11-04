package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// Server domain modes.
const (
	DomainModeManual  = "manual"
	DomainModeManaged = "managed"
)

// DNSAccount is one provider login the platform may write DNS through.
type DNSAccount struct {
	ID       int
	Name     string
	Provider string
	// EncCredentials stays encrypted in the store layer; only
	// DecryptDNSCredentials turns it back.
	EncCredentials string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LastError      string
	LastErrorAt    *time.Time
}

// DNSZone is a zone an account exposes. Name is lower-case, no trailing dot.
type DNSZone struct {
	ID        int
	AccountID int
	Name      string
	Manual    bool
	// Listed is false once the provider's zone listing stopped naming the zone
	// while a domain or an owned record still uses it.
	Listed bool
}

// DNSZoneAccess grants a principal ("user:<id>" or "role:<id>") use of a zone.
type DNSZoneAccess struct {
	ZoneID    int
	Principal string
}

// ServerDomain is one domain attached to a server.
type ServerDomain struct {
	ID       int
	ServerID int
	FQDN     string
	Mode     string // DomainModeManual | DomainModeManaged
	// ZoneID and Label are set for managed domains only.
	ZoneID     *int
	Label      string
	State      string
	Problems   []string
	ObservedAt *time.Time
	CreatedBy  string
	CreatedAt  time.Time
}

// DNSOwnedRecord is a record the platform created at a provider.
type DNSOwnedRecord struct {
	ID        int
	AccountID int
	Zone      string
	Name      string
	Type      string
	// Value and Port are what Core wrote, normalised (A: canonical IP, SRV: lower
	// case target without trailing dot, plus its port; Port is 0 for A). Only a
	// provider record with this exact value is Core's.
	Value      string
	Port       uint16
	ProviderID string
	// ServerDomainID is NULL once the domain is gone; the reconciler then
	// deletes the record.
	ServerDomainID *int
}

// DNSSubdomainLimit follows the platform limit convention: nil Max is no
// limit, 0 is none.
type DNSSubdomainLimit struct {
	Scope string
	Max   *int64
}

const dnsAccountCols = `id, name, provider, enc_credentials, created_at, updated_at, last_error, last_error_at`

func scanDNSAccount(row interface{ Scan(...interface{}) error }) (*DNSAccount, error) {
	var a DNSAccount
	var errAt sql.NullTime
	if err := row.Scan(&a.ID, &a.Name, &a.Provider, &a.EncCredentials, &a.CreatedAt, &a.UpdatedAt, &a.LastError, &errAt); err != nil {
		return nil, err
	}
	if errAt.Valid {
		a.LastErrorAt = &errAt.Time
	}
	return &a, nil
}

func (s *PostgresStore) CreateDNSAccount(a DNSAccount) (int, error) {
	var id int
	err := s.db.QueryRow(
		`INSERT INTO dns_accounts (name, provider, enc_credentials) VALUES ($1, $2, $3) RETURNING id`,
		a.Name, a.Provider, a.EncCredentials).Scan(&id)
	return id, err
}

// UpdateDNSAccount rewrites name, provider and credentials, and clears the last
// error: new credentials deserve a fresh verdict.
func (s *PostgresStore) UpdateDNSAccount(a DNSAccount) error {
	_, err := s.db.Exec(
		`UPDATE dns_accounts SET name = $2, provider = $3, enc_credentials = $4,
		        last_error = '', last_error_at = NULL, updated_at = NOW()
		  WHERE id = $1`, a.ID, a.Name, a.Provider, a.EncCredentials)
	return err
}

func (s *PostgresStore) DeleteDNSAccount(id int) error {
	_, err := s.db.Exec(`DELETE FROM dns_accounts WHERE id = $1`, id)
	return err
}

func (s *PostgresStore) ListDNSAccounts() ([]DNSAccount, error) {
	rows, err := s.db.Query(`SELECT ` + dnsAccountCols + ` FROM dns_accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DNSAccount{}
	for rows.Next() {
		a, err := scanDNSAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// GetDNSAccount returns nil, nil when there is no such account.
func (s *PostgresStore) GetDNSAccount(id int) (*DNSAccount, error) {
	a, err := scanDNSAccount(s.db.QueryRow(`SELECT `+dnsAccountCols+` FROM dns_accounts WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// SetDNSAccountError records the last provider failure; an empty msg clears it.
func (s *PostgresStore) SetDNSAccountError(id int, msg string) error {
	_, err := s.db.Exec(
		`UPDATE dns_accounts SET last_error = $2::text,
		        last_error_at = CASE WHEN $2::text = '' THEN NULL ELSE NOW() END
		  WHERE id = $1`, id, msg)
	return err
}

// ReplaceDNSZones makes names the account's zone set: missing ones are added,
// and manual is applied to every listed zone. An absent zone is removed (its
// access rows go with it) only while nothing uses it; one a server domain or
// an owned record still uses is kept and marked not listed. A provider listing
// can be incomplete (libdns/cloudflare reads one page of zones), and dropping a
// used zone would orphan its domains for good. A manual zone that names does
// not list is kept as it is: a listing does not know about the zones an admin
// typed in.
func (s *PostgresStore) ReplaceDNSZones(accountID int, names []string, manual bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, n := range names {
		if _, err := tx.Exec(
			`INSERT INTO dns_zones (account_id, name, manual) VALUES ($1, $2, $3)
			 ON CONFLICT (account_id, name) DO UPDATE SET manual = EXCLUDED.manual, listed = TRUE`,
			accountID, n, manual); err != nil {
			return fmt.Errorf("upsert zone %s: %w", n, err)
		}
	}
	if names == nil {
		names = []string{}
	}
	if _, err := tx.Exec(
		`DELETE FROM dns_zones z WHERE z.account_id = $1 AND NOT z.manual AND z.name <> ALL($2)
		   AND NOT EXISTS (SELECT 1 FROM server_domains d WHERE d.zone_id = z.id)
		   AND NOT EXISTS (SELECT 1 FROM dns_owned_records o
		                    WHERE o.account_id = z.account_id AND lower(o.zone) = z.name)`,
		accountID, pq.Array(names)); err != nil {
		return fmt.Errorf("drop stale zones: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE dns_zones SET listed = FALSE WHERE account_id = $1 AND NOT manual AND name <> ALL($2)`,
		accountID, pq.Array(names)); err != nil {
		return fmt.Errorf("mark unlisted zones: %w", err)
	}
	return tx.Commit()
}

// AddDNSZone adds a zone an admin typed in; adding one that is already there
// changes nothing, except that a zone the listing stopped naming becomes a
// manual one, which is how an admin keeps it once the listing misses it.
func (s *PostgresStore) AddDNSZone(accountID int, name string) error {
	_, err := s.db.Exec(
		`INSERT INTO dns_zones (account_id, name, manual) VALUES ($1, $2, TRUE)
		 ON CONFLICT (account_id, name) DO UPDATE SET manual = TRUE, listed = TRUE
		 WHERE NOT dns_zones.listed`, accountID, name)
	return err
}

func (s *PostgresStore) ListDNSZones() ([]DNSZone, error) {
	rows, err := s.db.Query(`SELECT id, account_id, name, manual, listed FROM dns_zones ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DNSZone{}
	for rows.Next() {
		var z DNSZone
		if err := rows.Scan(&z.ID, &z.AccountID, &z.Name, &z.Manual, &z.Listed); err != nil {
			return nil, err
		}
		out = append(out, z)
	}
	return out, rows.Err()
}

// SetDNSZoneAccess replaces the zone's principals.
func (s *PostgresStore) SetDNSZoneAccess(zoneID int, principals []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM dns_zone_access WHERE zone_id = $1`, zoneID); err != nil {
		return err
	}
	for _, p := range principals {
		if _, err := tx.Exec(
			`INSERT INTO dns_zone_access (zone_id, principal) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			zoneID, p); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *PostgresStore) ListDNSZoneAccess() ([]DNSZoneAccess, error) {
	rows, err := s.db.Query(`SELECT zone_id, principal FROM dns_zone_access ORDER BY zone_id, principal`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DNSZoneAccess{}
	for rows.Next() {
		var a DNSZoneAccess
		if err := rows.Scan(&a.ZoneID, &a.Principal); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

const serverDomainCols = `id, server_id, fqdn, mode, zone_id, label, state, problems, observed_at, created_by, created_at`

func (s *PostgresStore) queryServerDomains(q string, args ...interface{}) ([]ServerDomain, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ServerDomain{}
	for rows.Next() {
		var d ServerDomain
		var zone sql.NullInt64
		var observed sql.NullTime
		var problems []byte
		if err := rows.Scan(&d.ID, &d.ServerID, &d.FQDN, &d.Mode, &zone, &d.Label, &d.State,
			&problems, &observed, &d.CreatedBy, &d.CreatedAt); err != nil {
			return nil, err
		}
		if zone.Valid {
			z := int(zone.Int64)
			d.ZoneID = &z
		}
		if observed.Valid {
			d.ObservedAt = &observed.Time
		}
		if err := json.Unmarshal(problems, &d.Problems); err != nil {
			return nil, fmt.Errorf("decode problems of domain %d: %w", d.ID, err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CreateServerDomain inserts a domain in d.State; an empty State is grey (the
// column default 'pending' is no state the panel knows). A managed FQDN already taken, or the same FQDN twice on one
// server, is ErrNameTaken.
func (s *PostgresStore) CreateServerDomain(d ServerDomain) (int, error) {
	var id int
	err := s.db.QueryRow(
		`INSERT INTO server_domains (server_id, fqdn, mode, zone_id, label, created_by, state)
		 VALUES ($1, $2, $3, $4, $5, $6, COALESCE(NULLIF($7, ''), 'grey')) RETURNING id`,
		d.ServerID, d.FQDN, d.Mode, d.ZoneID, d.Label, d.CreatedBy, d.State).Scan(&id)
	if isUniqueViolation(err) {
		return 0, ErrNameTaken
	}
	return id, err
}

func (s *PostgresStore) DeleteServerDomain(id int) error {
	_, err := s.db.Exec(`DELETE FROM server_domains WHERE id = $1`, id)
	return err
}

func (s *PostgresStore) ListServerDomains() ([]ServerDomain, error) {
	return s.queryServerDomains(`SELECT ` + serverDomainCols + ` FROM server_domains ORDER BY id`)
}

func (s *PostgresStore) ListServerDomainsByServer(serverID int) ([]ServerDomain, error) {
	return s.queryServerDomains(
		`SELECT `+serverDomainCols+` FROM server_domains WHERE server_id = $1 ORDER BY id`, serverID)
}

// CountServerDomainsByOwner counts domains in both modes on servers the user owns.
func (s *PostgresStore) CountServerDomainsByOwner(ownerID string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM server_domains d JOIN servers sv ON sv.id = d.server_id
		  WHERE sv.owner_id = $1`, ownerID).Scan(&n)
	return n, err
}

func (s *PostgresStore) SetServerDomainState(id int, state string, problems []string, at time.Time) error {
	if problems == nil {
		problems = []string{}
	}
	raw, err := json.Marshal(problems)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`UPDATE server_domains SET state = $2, problems = $3, observed_at = $4 WHERE id = $1`,
		id, state, string(raw), at)
	return err
}

// AddDNSOwnedRecord records a record Core wrote. The same record already in
// the ledger (an orphan row, after its domain was removed and the name taken
// again) moves to r's domain instead of being duplicated.
func (s *PostgresStore) AddDNSOwnedRecord(r DNSOwnedRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO dns_owned_records (account_id, zone, name, type, value, port, provider_id, server_domain_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (account_id, zone, name, type, value, port)
		 DO UPDATE SET server_domain_id = EXCLUDED.server_domain_id`,
		r.AccountID, r.Zone, r.Name, r.Type, r.Value, int(r.Port), r.ProviderID, r.ServerDomainID)
	return err
}

func (s *PostgresStore) ListDNSOwnedRecords() ([]DNSOwnedRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, account_id, zone, name, type, value, port, provider_id, server_domain_id FROM dns_owned_records ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DNSOwnedRecord{}
	for rows.Next() {
		var r DNSOwnedRecord
		var sd sql.NullInt64
		if err := rows.Scan(&r.ID, &r.AccountID, &r.Zone, &r.Name, &r.Type, &r.Value, &r.Port, &r.ProviderID, &sd); err != nil {
			return nil, err
		}
		if sd.Valid {
			v := int(sd.Int64)
			r.ServerDomainID = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PostgresStore) DeleteDNSOwnedRecord(id int) error {
	_, err := s.db.Exec(`DELETE FROM dns_owned_records WHERE id = $1`, id)
	return err
}

// GetDNSSubdomainLimit returns nil, nil when the scope has no row, which means
// "this scope says nothing"; a row with a nil Max answers "no limit".
func (s *PostgresStore) GetDNSSubdomainLimit(scope string) (*DNSSubdomainLimit, error) {
	var max sql.NullInt64
	err := s.db.QueryRow(`SELECT max_subdomains FROM dns_subdomain_limits WHERE scope = $1`, scope).Scan(&max)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	l := &DNSSubdomainLimit{Scope: scope}
	if max.Valid {
		l.Max = &max.Int64
	}
	return l, nil
}

func (s *PostgresStore) SetDNSSubdomainLimit(scope string, max *int64) error {
	_, err := s.db.Exec(
		`INSERT INTO dns_subdomain_limits (scope, max_subdomains) VALUES ($1, $2)
		 ON CONFLICT (scope) DO UPDATE SET max_subdomains = EXCLUDED.max_subdomains`, scope, max)
	return err
}
