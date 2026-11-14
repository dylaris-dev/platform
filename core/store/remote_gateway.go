package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"dylaris-core/pkg/crypto"
)

// remoteKitPurpose scopes the at-rest key for remote_link_kits.link_key.
const remoteKitPurpose = "remote-link-kit-key"

var errNoRemoteKitKey = errors.New("remote gateway: no at-rest key for remote link kits")

// outboxLease is how long a claimed outbox row stays invisible to other
// claimers. The drain finishes or fails a row well inside it; if the Core dies
// mid-call the row simply becomes due again.
const outboxLease = 2 * time.Minute

// RemoteKit is the provider-issued link for one node.
type RemoteKit struct {
	NodeID  uint
	LinkID  string
	LinkKey string
	Region  string
	// RetireAt is when the provider revokes this retired kit; nil while it is
	// in normal use.
	RetireAt *time.Time
	// CreatedAt is read, never written: when the row was inserted.
	CreatedAt time.Time
}

// RemoteRoute is one route the provider holds on our behalf.
type RemoteRoute struct {
	Domain        string
	ServerUUID    string
	NodeID        uint
	Port          int
	ProxyProtocol bool
	// CustomDomain: the domain is the customer's own, not under a provider
	// hoster domain, so its route also needs a verified claim to stay.
	CustomDomain bool
	// UpdatedAt is read, never written: the column follows every write.
	UpdatedAt time.Time
}

// OutboxItem is one pending provider write.
type OutboxItem struct {
	ID        int64
	Op        string
	Domain    string
	Payload   json.RawMessage
	Attempts  int
	LastError string
}

// SetRemoteKitEncryptionKey installs the at-rest key, derived from
// CLUSTER_SECRET. Called once at boot; without it SaveRemoteKit fails closed.
func (s *PostgresStore) SetRemoteKitEncryptionKey(clusterSecret string) {
	if clusterSecret == "" {
		return
	}
	s.remoteKitKey = crypto.DeriveKey(clusterSecret, remoteKitPurpose)
}

func (s *PostgresStore) GetRemoteKit(nodeID uint) (*RemoteKit, error) {
	row := s.db.QueryRow(`SELECT node_id, link_id, link_key, region, retire_at, created_at FROM remote_link_kits WHERE node_id = $1`, nodeID)
	k, err := s.scanRemoteKit(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("remote gateway: get kit for node %d: %w", nodeID, err)
	}
	return k, nil
}

func (s *PostgresStore) ListRemoteKits() ([]RemoteKit, error) {
	rows, err := s.db.Query(`SELECT node_id, link_id, link_key, region, retire_at, created_at FROM remote_link_kits ORDER BY node_id`)
	if err != nil {
		return nil, fmt.Errorf("remote gateway: list kits: %w", err)
	}
	defer rows.Close()
	var out []RemoteKit
	for rows.Next() {
		k, err := s.scanRemoteKit(rows)
		if err != nil {
			return nil, fmt.Errorf("remote gateway: list kits: %w", err)
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

func (s *PostgresStore) scanRemoteKit(row interface{ Scan(...any) error }) (*RemoteKit, error) {
	var k RemoteKit
	var enc string
	var retire sql.NullTime
	if err := row.Scan(&k.NodeID, &k.LinkID, &enc, &k.Region, &retire, &k.CreatedAt); err != nil {
		return nil, err
	}
	if retire.Valid {
		k.RetireAt = &retire.Time
	}
	if s.remoteKitKey == nil {
		return nil, errNoRemoteKitKey
	}
	pt, err := crypto.Decrypt(s.remoteKitKey, enc)
	if err != nil {
		return nil, fmt.Errorf("decrypt link key: %w", err)
	}
	k.LinkKey = string(pt)
	return &k, nil
}

func (s *PostgresStore) SaveRemoteKit(k RemoteKit) error {
	if s.remoteKitKey == nil {
		return errNoRemoteKitKey
	}
	enc, err := crypto.Encrypt(s.remoteKitKey, []byte(k.LinkKey))
	if err != nil {
		return fmt.Errorf("remote gateway: encrypt link key: %w", err)
	}
	if _, err := s.db.Exec(`INSERT INTO remote_link_kits (node_id, link_id, link_key, region)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (node_id) DO UPDATE SET link_id = EXCLUDED.link_id, link_key = EXCLUDED.link_key, region = EXCLUDED.region`,
		k.NodeID, k.LinkID, enc, k.Region); err != nil {
		return fmt.Errorf("remote gateway: save kit for node %d: %w", k.NodeID, err)
	}
	return nil
}

// InsertRemoteKit saves k only if the node has no kit yet. inserted=false means
// another caller (possibly another Core replica) saved first; theirs is kept.
func (s *PostgresStore) InsertRemoteKit(k RemoteKit) (inserted bool, err error) {
	if s.remoteKitKey == nil {
		return false, errNoRemoteKitKey
	}
	enc, err := crypto.Encrypt(s.remoteKitKey, []byte(k.LinkKey))
	if err != nil {
		return false, fmt.Errorf("remote gateway: encrypt link key: %w", err)
	}
	res, err := s.db.Exec(`INSERT INTO remote_link_kits (node_id, link_id, link_key, region)
		VALUES ($1, $2, $3, $4) ON CONFLICT (node_id) DO NOTHING`,
		k.NodeID, k.LinkID, enc, k.Region)
	if err != nil {
		return false, fmt.Errorf("remote gateway: insert kit for node %d: %w", k.NodeID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("remote gateway: insert kit for node %d: %w", k.NodeID, err)
	}
	return n == 1, nil
}

// SetRemoteKitRetireAt stamps the node's kit as retiring, only while it is
// still the kit with linkID: a kit minted since is not the one retired.
func (s *PostgresStore) SetRemoteKitRetireAt(nodeID uint, linkID string, at time.Time) error {
	if _, err := s.db.Exec(`UPDATE remote_link_kits SET retire_at = $3 WHERE node_id = $1 AND link_id = $2`, nodeID, linkID, at); err != nil {
		return fmt.Errorf("remote gateway: retire kit of node %d: %w", nodeID, err)
	}
	return nil
}

func (s *PostgresStore) DeleteRemoteKit(nodeID uint) error {
	if _, err := s.db.Exec(`DELETE FROM remote_link_kits WHERE node_id = $1`, nodeID); err != nil {
		return fmt.Errorf("remote gateway: delete kit for node %d: %w", nodeID, err)
	}
	return nil
}

func (s *PostgresStore) UpsertRemoteRoute(r RemoteRoute) error {
	if _, err := s.db.Exec(`INSERT INTO remote_routes (domain, server_uuid, node_id, port, proxy_protocol, custom_domain)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (domain) DO UPDATE SET server_uuid = EXCLUDED.server_uuid, node_id = EXCLUDED.node_id,
			port = EXCLUDED.port, proxy_protocol = EXCLUDED.proxy_protocol, custom_domain = EXCLUDED.custom_domain, updated_at = NOW()`,
		r.Domain, r.ServerUUID, r.NodeID, r.Port, r.ProxyProtocol, r.CustomDomain); err != nil {
		return fmt.Errorf("remote gateway: upsert route %s: %w", r.Domain, err)
	}
	return nil
}

// ClaimRemoteRoute inserts r only if no row holds its domain yet. Every tenant
// of a customer Core shares one provider account, so the provider cannot tell
// two of our servers apart: this row is the only arbiter of who holds an
// address. claimed=false hands back the row that holds it.
func (s *PostgresStore) ClaimRemoteRoute(r RemoteRoute) (bool, *RemoteRoute, error) {
	res, err := s.db.Exec(`INSERT INTO remote_routes (domain, server_uuid, node_id, port, proxy_protocol, custom_domain)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (domain) DO NOTHING`,
		r.Domain, r.ServerUUID, r.NodeID, r.Port, r.ProxyProtocol, r.CustomDomain)
	if err != nil {
		return false, nil, fmt.Errorf("remote gateway: claim route %s: %w", r.Domain, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil, fmt.Errorf("remote gateway: claim route %s: %w", r.Domain, err)
	}
	if n == 1 {
		return true, nil, nil
	}
	existing, err := s.GetRemoteRoute(r.Domain)
	if err != nil {
		return false, nil, err
	}
	if existing == nil {
		// Deleted between the insert and the read; the caller retries.
		return false, nil, fmt.Errorf("remote gateway: claim route %s: the holding row vanished, try again", r.Domain)
	}
	return false, existing, nil
}

func (s *PostgresStore) DeleteRemoteRoute(domain string) error {
	if _, err := s.db.Exec(`DELETE FROM remote_routes WHERE domain = $1`, domain); err != nil {
		return fmt.Errorf("remote gateway: delete route %s: %w", domain, err)
	}
	return nil
}

func (s *PostgresStore) ListRemoteRoutesByServer(serverUUID string) ([]RemoteRoute, error) {
	return s.listRemoteRoutes(`WHERE server_uuid = $1`, serverUUID)
}

func (s *PostgresStore) ListRemoteRoutes() ([]RemoteRoute, error) {
	return s.listRemoteRoutes(``)
}

func (s *PostgresStore) GetRemoteRoute(domain string) (*RemoteRoute, error) {
	rows, err := s.listRemoteRoutes(`WHERE domain = $1`, domain)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

func (s *PostgresStore) listRemoteRoutes(where string, args ...any) ([]RemoteRoute, error) {
	rows, err := s.db.Query(`SELECT domain, server_uuid, node_id, port, proxy_protocol, custom_domain, updated_at FROM remote_routes `+where+` ORDER BY domain`, args...)
	if err != nil {
		return nil, fmt.Errorf("remote gateway: list routes: %w", err)
	}
	defer rows.Close()
	var out []RemoteRoute
	for rows.Next() {
		var r RemoteRoute
		if err := rows.Scan(&r.Domain, &r.ServerUUID, &r.NodeID, &r.Port, &r.ProxyProtocol, &r.CustomDomain, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("remote gateway: list routes: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PostgresStore) EnqueueOutbox(op, domain string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("remote gateway: outbox payload for %s: %w", op, err)
	}
	if _, err := s.db.Exec(`INSERT INTO gateway_outbox (op, domain, payload) VALUES ($1, $2, $3)`, op, domain, string(b)); err != nil {
		return fmt.Errorf("remote gateway: enqueue %s %s: %w", op, domain, err)
	}
	return nil
}

// HasOpenOutbox reports whether a not yet finished item exists for domain; op
// "" matches any op.
func (s *PostgresStore) HasOpenOutbox(domain, op string) (bool, error) {
	var ok bool
	if err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM gateway_outbox
		WHERE domain = $1 AND done_at IS NULL AND ($2::text = '' OR op = $2::text))`, domain, op).Scan(&ok); err != nil {
		return false, fmt.Errorf("remote gateway: open outbox for %s: %w", domain, err)
	}
	return ok, nil
}

// OpenOutboxDomains is the set of domains with a not yet finished item of op,
// in one query instead of one HasOpenOutbox per domain.
func (s *PostgresStore) OpenOutboxDomains(op string) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT DISTINCT domain FROM gateway_outbox WHERE done_at IS NULL AND op = $1`, op)
	if err != nil {
		return nil, fmt.Errorf("remote gateway: open outbox domains for %s: %w", op, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, fmt.Errorf("remote gateway: open outbox domains for %s: %w", op, err)
		}
		out[d] = true
	}
	return out, rows.Err()
}

// ClaimDueOutbox takes up to limit due rows and pushes their next_attempt_at
// past the lease in the same statement, so a second leader (or a second tick)
// cannot take the same rows. SKIP LOCKED keeps two concurrent claims disjoint
// even before either commits. A row waiting behind an older open row of its
// domain is not claimed: it could not run anyway, and claiming it would take a
// slot from a domain that can.
func (s *PostgresStore) ClaimDueOutbox(now time.Time, limit int) ([]OutboxItem, error) {
	rows, err := s.db.Query(`UPDATE gateway_outbox SET next_attempt_at = $1
		WHERE id IN (
			SELECT g.id FROM gateway_outbox g
			WHERE g.done_at IS NULL AND g.next_attempt_at <= $2
				AND NOT EXISTS (SELECT 1 FROM gateway_outbox o
					WHERE o.domain = g.domain AND o.done_at IS NULL AND o.id < g.id)
			ORDER BY g.next_attempt_at, g.id LIMIT $3
			FOR UPDATE OF g SKIP LOCKED)
		RETURNING id, op, domain, payload, attempts, last_error`,
		now.Add(outboxLease), now, limit)
	if err != nil {
		return nil, fmt.Errorf("remote gateway: claim outbox: %w", err)
	}
	defer rows.Close()
	var out []OutboxItem
	for rows.Next() {
		var it OutboxItem
		var payload []byte
		if err := rows.Scan(&it.ID, &it.Op, &it.Domain, &payload, &it.Attempts, &it.LastError); err != nil {
			return nil, fmt.Errorf("remote gateway: claim outbox: %w", err)
		}
		it.Payload = json.RawMessage(payload)
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("remote gateway: claim outbox: %w", err)
	}
	// RETURNING has no order; the drain must replay writes in the order they were made.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *PostgresStore) FinishOutbox(id int64) error {
	if _, err := s.db.Exec(`UPDATE gateway_outbox SET done_at = NOW() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("remote gateway: finish outbox %d: %w", id, err)
	}
	return nil
}

func (s *PostgresStore) FailOutbox(id int64, lastError string, next time.Time) error {
	if _, err := s.db.Exec(`UPDATE gateway_outbox SET attempts = attempts + 1, last_error = $2, next_attempt_at = $3
		WHERE id = $1`, id, lastError, next); err != nil {
		return fmt.Errorf("remote gateway: fail outbox %d: %w", id, err)
	}
	return nil
}

// OldestOpenOutboxID is the lowest id still open for domain, 0 when none: the
// drain may only act on that one, because the claim orders by due time.
func (s *PostgresStore) OldestOpenOutboxID(domain string) (int64, error) {
	var id sql.NullInt64
	if err := s.db.QueryRow(`SELECT MIN(id) FROM gateway_outbox WHERE domain = $1 AND done_at IS NULL`, domain).Scan(&id); err != nil {
		return 0, fmt.Errorf("remote gateway: oldest open outbox for %s: %w", domain, err)
	}
	return id.Int64, nil
}

// DeferOutbox hands a claimed row back for next without counting an attempt,
// for a row the drain did not try.
func (s *PostgresStore) DeferOutbox(id int64, next time.Time) error {
	if _, err := s.db.Exec(`UPDATE gateway_outbox SET next_attempt_at = $2 WHERE id = $1`, id, next); err != nil {
		return fmt.Errorf("remote gateway: defer outbox %d: %w", id, err)
	}
	return nil
}

func (s *PostgresStore) PurgeDoneOutbox(olderThan time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM gateway_outbox WHERE done_at IS NOT NULL AND done_at < $1`, olderThan)
	if err != nil {
		return 0, fmt.Errorf("remote gateway: purge outbox: %w", err)
	}
	return res.RowsAffected()
}

// CountOpenOutbox returns how many writes are still pending and when the oldest
// was queued (zero time when none), for the panel's "N changes waiting" notice.
// A kit revoke is not a route change, so it is not counted.
func (s *PostgresStore) CountOpenOutbox() (int, time.Time, error) {
	var n int
	var oldest sql.NullTime
	if err := s.db.QueryRow(`SELECT COUNT(*), MIN(created_at) FROM gateway_outbox WHERE done_at IS NULL AND op <> 'revoke_kit'`).Scan(&n, &oldest); err != nil {
		return 0, time.Time{}, fmt.Errorf("remote gateway: count outbox: %w", err)
	}
	return n, oldest.Time, nil
}

// ListRefusedOutbox returns up to limit open route changes the provider
// refused, newest first: what the panel lists under "changes are waiting". A
// refusal is a 4xx other than 429, read off the stored text, which is
// remotegw.APIError's ("gateway provider answered <status> <code>: ..."); a
// network error or a 5xx only waits. Op, Domain and LastError are set.
func (s *PostgresStore) ListRefusedOutbox(limit int) ([]OutboxItem, error) {
	rows, err := s.db.Query(`SELECT op, domain, last_error FROM gateway_outbox
		WHERE done_at IS NULL AND op <> 'revoke_kit'
			AND last_error LIKE 'gateway provider answered 4%' AND last_error NOT LIKE 'gateway provider answered 429 %'
		ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("remote gateway: refused outbox: %w", err)
	}
	defer rows.Close()
	var out []OutboxItem
	for rows.Next() {
		var it OutboxItem
		if err := rows.Scan(&it.Op, &it.Domain, &it.LastError); err != nil {
			return nil, fmt.Errorf("remote gateway: refused outbox: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
