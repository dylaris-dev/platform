package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"dylaris-core/pkg/crypto"
)

// bedrockFloodgatePurpose scopes the at-rest key for per-server Floodgate keys.
const bedrockFloodgatePurpose = "bedrock-floodgate-key"

var errNoBedrockKey = errors.New("bedrock: no encryption key configured; refusing to store a plaintext key")

// BedrockServer is one server with Bedrock joins enabled.
type BedrockServer struct {
	ServerID   int
	ServerUUID string
	Key        []byte
}

// SetBedrockEncryptionKey installs the at-rest key, derived from
// CLUSTER_SECRET. Called once at boot; without it writes fail closed.
func (s *PostgresStore) SetBedrockEncryptionKey(clusterSecret string) {
	if clusterSecret == "" {
		return
	}
	s.bedrockKey = crypto.DeriveKey(clusterSecret, bedrockFloodgatePurpose)
}

// EnableBedrock stores the server's Floodgate key. An existing key is kept:
// re-enabling must not rotate a key the server (and its players' links) already use.
func (s *PostgresStore) EnableBedrock(serverID int, key []byte) error {
	if s.bedrockKey == nil {
		return errNoBedrockKey
	}
	enc, err := crypto.Encrypt(s.bedrockKey, key)
	if err != nil {
		return fmt.Errorf("bedrock: encrypt key: %w", err)
	}
	if _, err := s.db.Exec(`INSERT INTO bedrock_servers (server_id, floodgate_key_enc) VALUES ($1, $2)
		ON CONFLICT (server_id) DO NOTHING`, serverID, enc); err != nil {
		return fmt.Errorf("bedrock: enable server %d: %w", serverID, err)
	}
	return nil
}

// GetBedrockKey returns the server's key; ok is false when Bedrock is not enabled.
func (s *PostgresStore) GetBedrockKey(serverID int) ([]byte, bool, error) {
	var enc string
	err := s.db.QueryRow(`SELECT floodgate_key_enc FROM bedrock_servers WHERE server_id = $1`, serverID).Scan(&enc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("bedrock: get key for server %d: %w", serverID, err)
	}
	key, err := s.decodeBedrockKey(enc)
	if err != nil {
		return nil, false, fmt.Errorf("bedrock: server %d: %w", serverID, err)
	}
	return key, true, nil
}

func (s *PostgresStore) DisableBedrock(serverID int) error {
	if _, err := s.db.Exec(`DELETE FROM bedrock_servers WHERE server_id = $1`, serverID); err != nil {
		return fmt.Errorf("bedrock: disable server %d: %w", serverID, err)
	}
	return nil
}

func (s *PostgresStore) ListBedrockServers() ([]BedrockServer, error) {
	rows, err := s.db.Query(`SELECT b.server_id, sv.uuid, b.floodgate_key_enc
		FROM bedrock_servers b JOIN servers sv ON sv.id = b.server_id ORDER BY b.server_id`)
	if err != nil {
		return nil, fmt.Errorf("bedrock: list: %w", err)
	}
	defer rows.Close()
	var out []BedrockServer
	for rows.Next() {
		var b BedrockServer
		var enc string
		if err := rows.Scan(&b.ServerID, &b.ServerUUID, &enc); err != nil {
			return nil, fmt.Errorf("bedrock: scan: %w", err)
		}
		if b.Key, err = s.decodeBedrockKey(enc); err != nil {
			return nil, fmt.Errorf("bedrock: server %d: %w", b.ServerID, err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BedrockServerRef is a Bedrock-enabled server without its key.
type BedrockServerRef struct {
	ServerID   int
	ServerUUID string
}

// ListBedrockServerRefs lists the Bedrock-enabled servers without decrypting
// their keys, so one unreadable key cannot stop metering or the health check.
func (s *PostgresStore) ListBedrockServerRefs() ([]BedrockServerRef, error) {
	rows, err := s.db.Query(`SELECT b.server_id, sv.uuid
		FROM bedrock_servers b JOIN servers sv ON sv.id = b.server_id ORDER BY b.server_id`)
	if err != nil {
		return nil, fmt.Errorf("bedrock: list refs: %w", err)
	}
	defer rows.Close()
	var out []BedrockServerRef
	for rows.Next() {
		var r BedrockServerRef
		if err := rows.Scan(&r.ServerID, &r.ServerUUID); err != nil {
			return nil, fmt.Errorf("bedrock: scan ref: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PostgresStore) decodeBedrockKey(enc string) ([]byte, error) {
	if s.bedrockKey == nil {
		return nil, errNoBedrockKey
	}
	key, err := crypto.Decrypt(s.bedrockKey, enc)
	if err != nil {
		return nil, fmt.Errorf("decrypt key: %w", err)
	}
	return key, nil
}

// BedrockUsageRow is one server's Bedrock traffic in one region for a period.
type BedrockUsageRow struct {
	ServerID                       int
	ServerUUID, ServerName, Region string
	RxBytes, TxBytes               int64
}

// AddBedrockUsage adds a delta onto one (server, period, region) row.
func (s *PostgresStore) AddBedrockUsage(serverID int, period time.Time, region string, rx, tx int64) error {
	_, err := s.db.Exec(`
		INSERT INTO bedrock_usage (server_id, period, region, rx_bytes, tx_bytes, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (server_id, period, region) DO UPDATE SET
			rx_bytes   = bedrock_usage.rx_bytes + EXCLUDED.rx_bytes,
			tx_bytes   = bedrock_usage.tx_bytes + EXCLUDED.tx_bytes,
			updated_at = NOW()`,
		serverID, period, region, rx, tx)
	if err != nil {
		return fmt.Errorf("bedrock: add usage for server %d: %w", serverID, err)
	}
	return nil
}

func (s *PostgresStore) ListBedrockUsage(period time.Time) ([]BedrockUsageRow, error) {
	rows, err := s.db.Query(`
		SELECT u.server_id, s.uuid, s.name, u.region, u.rx_bytes, u.tx_bytes
		FROM bedrock_usage u JOIN servers s ON s.id = u.server_id
		WHERE u.period = $1
		ORDER BY u.rx_bytes + u.tx_bytes DESC, u.server_id, u.region`, period)
	if err != nil {
		return nil, fmt.Errorf("bedrock: list usage: %w", err)
	}
	defer rows.Close()
	var out []BedrockUsageRow
	for rows.Next() {
		var r BedrockUsageRow
		if err := rows.Scan(&r.ServerID, &r.ServerUUID, &r.ServerName, &r.Region, &r.RxBytes, &r.TxBytes); err != nil {
			return nil, fmt.Errorf("bedrock: scan usage: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddTrafficUsageRegionBedrock adds to the Bedrock SUBSET of one
// traffic_usage_region row; the same bytes also go through AddTrafficUsageRegion.
func (s *PostgresStore) AddTrafficUsageRegionBedrock(userID string, period time.Time, region, kind, product string, bytes int64) error {
	return addTrafficUsageRegionBedrock(s.db, userID, period, region, kind, product, bytes)
}

func addTrafficUsageRegionBedrock(x trafficExecer, userID string, period time.Time, region, kind, product string, bytes int64) error {
	_, err := x.Exec(`
		INSERT INTO traffic_usage_region (user_id, period, region, kind, product, bedrock_bytes, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (user_id, period, region, kind, product) DO UPDATE SET
			bedrock_bytes = traffic_usage_region.bedrock_bytes + EXCLUDED.bedrock_bytes,
			updated_at    = NOW()`,
		userID, period, region, kind, product, bytes)
	return err
}
