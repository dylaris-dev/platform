package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// VoicePort is a server's public voice-chat UDP port in one region, with the
// key its node uses on the uplink toward the edge.
type VoicePort struct {
	ServerID     int
	ServerUUID   string
	ServerName   string
	Region       string
	Port         int
	Mod          string // "svc" or "plasmo"
	InternalPort int
	UplinkKey    []byte
	Host         string
	CreatedAt    time.Time
}

// ErrVoicePoolFull is a region with no free port left in the voice pool.
var ErrVoicePoolFull = errors.New("voice port pool exhausted")

// AllocateVoicePort gives p.ServerID a port in p.Region and returns the stored
// row.
//
// A server that already holds a port in the SAME region keeps its port and its
// key - both are already in Redis and in the players' clients - and only mod,
// internal_port and host follow p. A port in another region is released and a
// fresh one taken. A new port is the lowest free one in [min, max]. Two
// allocations racing for the same port meet on UNIQUE(region, port); the loser
// retries.
func (s *PostgresStore) AllocateVoicePort(p VoicePort, min, max int) (VoicePort, error) {
	for attempt := 0; ; attempt++ {
		got, err := s.allocateVoicePortOnce(p, min, max)
		if err != nil && isUniqueViolation(err) && attempt < 3 {
			continue
		}
		return got, err
	}
}

func (s *PostgresStore) allocateVoicePortOnce(p VoicePort, min, max int) (VoicePort, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return VoicePort{}, err
	}
	defer tx.Rollback()
	if err := lockVoicePool(tx, p.Region); err != nil {
		return VoicePort{}, err
	}

	var region string
	err = tx.QueryRow(`SELECT region FROM voice_ports WHERE server_id = $1 FOR UPDATE`, p.ServerID).Scan(&region)
	switch {
	case err == nil && region == p.Region:
		if _, err := tx.Exec(`UPDATE voice_ports SET mod = $2, internal_port = $3, host = $4 WHERE server_id = $1`,
			p.ServerID, p.Mod, p.InternalPort, p.Host); err != nil {
			return VoicePort{}, err
		}
	case err == nil || errors.Is(err, sql.ErrNoRows):
		if err == nil {
			if _, err := tx.Exec(`DELETE FROM voice_ports WHERE server_id = $1`, p.ServerID); err != nil {
				return VoicePort{}, err
			}
		}
		port, err := freeVoicePort(tx, p.Region, min, max)
		if err != nil {
			return VoicePort{}, err
		}
		if _, err := tx.Exec(`
			INSERT INTO voice_ports (server_id, region, port, mod, internal_port, uplink_key, host)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			p.ServerID, p.Region, port, p.Mod, p.InternalPort, p.UplinkKey, p.Host); err != nil {
			return VoicePort{}, err
		}
	default:
		return VoicePort{}, err
	}

	got, err := scanVoicePort(tx.QueryRow(voicePortSelect+` WHERE v.server_id = $1`, p.ServerID))
	if err != nil {
		return VoicePort{}, fmt.Errorf("read back voice port: %w", err)
	}
	return got, tx.Commit()
}

// lockVoicePool serialises allocations in one region across voice_ports and
// link_route_voice: each table's UNIQUE (region, port) cannot see the other's.
func lockVoicePool(tx *sql.Tx, region string) error {
	_, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext('voice-pool:' || $1))`, region)
	return err
}

// freeVoicePort is the lowest port in [min, max] neither table holds in region.
func freeVoicePort(tx *sql.Tx, region string, min, max int) (int, error) {
	var port int
	err := tx.QueryRow(`
		SELECT g FROM generate_series($2::int, $3::int) g
		WHERE g NOT IN (SELECT port FROM voice_ports WHERE region = $1)
		  AND g NOT IN (SELECT port FROM link_route_voice WHERE region = $1)
		ORDER BY g LIMIT 1`, region, min, max).Scan(&port)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrVoicePoolFull
	}
	return port, err
}

const voicePortSelect = `
	SELECT v.server_id, s.uuid, s.name, v.region, v.port, v.mod, v.internal_port, v.uplink_key, v.host, v.created_at
	FROM voice_ports v JOIN servers s ON s.id = v.server_id`

func scanVoicePort(row interface{ Scan(...any) error }) (VoicePort, error) {
	var p VoicePort
	err := row.Scan(&p.ServerID, &p.ServerUUID, &p.ServerName, &p.Region, &p.Port, &p.Mod, &p.InternalPort, &p.UplinkKey, &p.Host, &p.CreatedAt)
	return p, err
}

// GetVoicePort returns the server's voice port, or nil, nil when it has none.
func (s *PostgresStore) GetVoicePort(serverID int) (*VoicePort, error) {
	p, err := scanVoicePort(s.db.QueryRow(voicePortSelect+` WHERE v.server_id = $1`, serverID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// RotateVoiceUplinkKey replaces the server's uplink key and returns the row, or
// nil, nil when the server has no voice port.
func (s *PostgresStore) RotateVoiceUplinkKey(serverID int, key []byte) (*VoicePort, error) {
	res, err := s.db.Exec(`UPDATE voice_ports SET uplink_key = $2 WHERE server_id = $1`, serverID, key)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil
	}
	return s.GetVoicePort(serverID)
}

// UpdateVoicePortMod refreshes mod, internal_port and host of the row only if
// it still holds (region, port). false means the row was deleted or moved since
// the caller read it; unlike AllocateVoicePort this never inserts or moves one.
func (s *PostgresStore) UpdateVoicePortMod(serverID int, region string, port int, mod string, internalPort int, host string) (bool, error) {
	res, err := s.db.Exec(`UPDATE voice_ports SET mod = $4, internal_port = $5, host = $6
		WHERE server_id = $1 AND region = $2 AND port = $3`,
		serverID, region, port, mod, internalPort, host)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *PostgresStore) DeleteVoicePort(serverID int) error {
	_, err := s.db.Exec(`DELETE FROM voice_ports WHERE server_id = $1`, serverID)
	return err
}

func (s *PostgresStore) ListVoicePorts() ([]VoicePort, error) {
	rows, err := s.db.Query(voicePortSelect + ` ORDER BY v.region, v.port`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VoicePort
	for rows.Next() {
		p, err := scanVoicePort(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// VoiceUsageRow is one server's voice traffic in one region for a period.
type VoiceUsageRow struct {
	ServerID                       int
	ServerUUID, ServerName, Region string
	RxBytes, TxBytes               int64
}

// AddVoiceUsage adds a delta onto one (server, period, region) row.
func (s *PostgresStore) AddVoiceUsage(serverID int, period time.Time, region string, rx, tx int64) error {
	_, err := s.db.Exec(`
		INSERT INTO voice_usage (server_id, period, region, rx_bytes, tx_bytes, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (server_id, period, region) DO UPDATE SET
			rx_bytes   = voice_usage.rx_bytes + EXCLUDED.rx_bytes,
			tx_bytes   = voice_usage.tx_bytes + EXCLUDED.tx_bytes,
			updated_at = NOW()`,
		serverID, period, region, rx, tx)
	return err
}

func (s *PostgresStore) ListVoiceUsage(period time.Time) ([]VoiceUsageRow, error) {
	rows, err := s.db.Query(`
		SELECT u.server_id, s.uuid, s.name, u.region, u.rx_bytes, u.tx_bytes
		FROM voice_usage u JOIN servers s ON s.id = u.server_id
		WHERE u.period = $1
		ORDER BY u.rx_bytes + u.tx_bytes DESC, u.server_id, u.region`, period)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VoiceUsageRow
	for rows.Next() {
		var r VoiceUsageRow
		if err := rows.Scan(&r.ServerID, &r.ServerUUID, &r.ServerName, &r.Region, &r.RxBytes, &r.TxBytes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddTrafficUsageRegionVoice adds to the voice SUBSET of one
// traffic_usage_region row. The same bytes also go through
// AddTrafficUsageRegion, so bytes stays the total and voice_bytes is the part
// of it that was voice.
func (s *PostgresStore) AddTrafficUsageRegionVoice(userID string, period time.Time, region, kind, product string, bytes int64) error {
	return addTrafficUsageRegionVoice(s.db, userID, period, region, kind, product, bytes)
}

func addTrafficUsageRegionVoice(x trafficExecer, userID string, period time.Time, region, kind, product string, bytes int64) error {
	_, err := x.Exec(`
		INSERT INTO traffic_usage_region (user_id, period, region, kind, product, voice_bytes, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (user_id, period, region, kind, product) DO UPDATE SET
			voice_bytes = traffic_usage_region.voice_bytes + EXCLUDED.voice_bytes,
			updated_at  = NOW()`,
		userID, period, region, kind, product, bytes)
	return err
}

// VoicePermission is one scope's answer to "may voice chat be turned on".
type VoicePermission struct {
	Scope     string    `json:"scope"`
	Allowed   bool      `json:"allowed"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// GetVoicePermission returns the scope's answer, or nil, nil when the scope has
// no row and so says nothing.
func (s *PostgresStore) GetVoicePermission(scope string) (*bool, error) {
	var allowed bool
	err := s.db.QueryRow(`SELECT allowed FROM voice_permissions WHERE scope = $1`, scope).Scan(&allowed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &allowed, nil
}

func (s *PostgresStore) SetVoicePermission(scope string, allowed bool) error {
	_, err := s.db.Exec(`
		INSERT INTO voice_permissions (scope, allowed, updated_at) VALUES ($1, $2, NOW())
		ON CONFLICT (scope) DO UPDATE SET allowed = EXCLUDED.allowed, updated_at = NOW()`,
		scope, allowed)
	return err
}

func (s *PostgresStore) DeleteVoicePermission(scope string) error {
	_, err := s.db.Exec(`DELETE FROM voice_permissions WHERE scope = $1`, scope)
	return err
}

func (s *PostgresStore) ListVoicePermissions() ([]VoicePermission, error) {
	rows, err := s.db.Query(`SELECT scope, allowed, updated_at FROM voice_permissions ORDER BY scope`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VoicePermission{}
	for rows.Next() {
		var p VoicePermission
		if err := rows.Scan(&p.Scope, &p.Allowed, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
