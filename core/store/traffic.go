package store

import (
	"database/sql"
	"time"
)

// TrafficUsage is one tenant's metered usage for one billing month. edge_bytes
// is the billable player traffic; relay_bytes and backup_bytes are tracked for
// observability and (later) overage. A zero-value row (all counters 0) is the
// correct answer for a tenant with no traffic yet, so reads never error on
// "not found".
type TrafficUsage struct {
	UserID      string    `json:"userId"`
	Username    string    `json:"username,omitempty"` // filled by the admin list, not the per-user read
	Period      time.Time `json:"period"`             // first day of the billing month (UTC)
	EdgeBytes   int64     `json:"edgeBytes"`
	RelayBytes  int64     `json:"relayBytes"`
	BackupBytes int64     `json:"backupBytes"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// TenantServerOwners returns serverUUID -> owning tenant (user id) for every
// server that sits on a tenant-owned (BYON) node. Platform nodes (owner_id NULL)
// are excluded, so their traffic is never billed. The traffic aggregator builds
// this map once per tick to attribute per-server byte counters to a tenant.
func (s *PostgresStore) TenantServerOwners() (map[string]string, error) {
	rows, err := s.db.Query(`
		SELECT s.uuid, n.owner_id
		FROM servers s
		JOIN nodes n ON n.id = s.node_id
		WHERE n.owner_id IS NOT NULL AND s.uuid <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var uuid, owner string
		if err := rows.Scan(&uuid, &owner); err != nil {
			return nil, err
		}
		out[uuid] = owner
	}
	return out, rows.Err()
}

// TenantBackupBytes returns the storage a tenant's backups occupy per tenant
// (user id), for servers on tenant-owned nodes. This is a storage gauge (what is
// held right now), not a flow, so the aggregator overwrites the snapshot each
// tick. Tenants with no backups are absent from the map; the caller resets those
// to 0.
//
// Like BackupBytesByOwner it counts successful runs plus any run with a nonzero
// size, so an abandoned run whose archive the reaper confirmed present is
// included. Failed runs are size 0 unless the reaper found a real object.
//
// And like BackupBytesByOwner it counts only what sits on OUR storage. This is
// the gauge the bill is computed from, so it has to agree with the gate that
// refuses a backup - a tenant whose own bucket counted here but not there would
// be billed for storage nothing was enforcing, which is the worse of the two
// directions to disagree in. LEFT JOIN, so a run with no storage recorded still
// counts: NULL means "before this column", and those archives are ours.
func (s *PostgresStore) TenantBackupBytes() (map[string]int64, error) {
	rows, err := s.db.Query(`
		SELECT n.owner_id, COALESCE(SUM(br.size_bytes), 0)
		FROM backup_runs br
		JOIN backup_jobs bj ON bj.id = br.job_id
		JOIN servers s ON s.id = bj.server_id
		JOIN nodes n ON n.id = s.node_id
		LEFT JOIN backup_storages bst ON bst.id = br.storage_id
		WHERE n.owner_id IS NOT NULL AND (br.status = 'success' OR (br.status = 'failed' AND br.size_bytes > 0))
		  AND bst.owner_id IS NULL
		GROUP BY n.owner_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var owner string
		var total int64
		if err := rows.Scan(&owner, &total); err != nil {
			return nil, err
		}
		out[owner] = total
	}
	return out, rows.Err()
}

// AddTrafficUsage adds (not sets) byte deltas onto a tenant's current-period row,
// creating it on first write. The aggregator computes deltas from monotonic
// Redis counters, so this accumulates the running monthly total.
func (s *PostgresStore) AddTrafficUsage(userID string, period time.Time, edgeBytes, relayBytes int64) error {
	_, err := s.db.Exec(`
		INSERT INTO traffic_usage (user_id, period, edge_bytes, relay_bytes, updated_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (user_id, period) DO UPDATE SET
			edge_bytes  = traffic_usage.edge_bytes  + EXCLUDED.edge_bytes,
			relay_bytes = traffic_usage.relay_bytes + EXCLUDED.relay_bytes,
			updated_at  = NOW()`,
		userID, period, edgeBytes, relayBytes)
	return err
}

// RegionUsage is one (region, kind) share of a tenant's traffic in a period.
//
// Kind separates PLAYER traffic from DATA traffic, which is not a cosmetic
// split: a customer whose players connect through a US edge still moves their
// file transfers through an EU relay, so the two land in different regions with
// different costs and need different allowances.
type RegionUsage struct {
	Region string `json:"region"`
	Kind   string `json:"kind"`
	// Product is which product moved the bytes - see the TrafficProduct
	// constants. A breakdown dimension only: the allowance is pooled across
	// products, so every limit decision folds this away first.
	Product string `json:"product"`
	Bytes   int64  `json:"bytes"`
}

// Traffic kinds. Open set on purpose - see the traffic_usage_region comment in
// database/db_traffic.go for why this is a row value and not a column.
const (
	TrafficKindEdge  = "edge"  // player traffic, at the edge that served it
	TrafficKindRelay = "relay" // beam file transfers, at the relay that carried them
)

// Traffic products. Which of the two things a tenant can buy carried the bytes.
//
// Derived from the counter subject in the aggregator, not reported by any
// producer. TrafficProductUnknown is the empty string on purpose: it is what
// rows written before this dimension existed already hold, so they read as
// "we do not know" rather than being silently attributed to a product.
const (
	TrafficProductBYON    = "byon"  // a server on a machine the tenant owns
	TrafficProductRoute   = "route" // a protected address pointing at their own server
	TrafficProductUnknown = ""
)

// AddTrafficUsageRegion adds a delta onto one (tenant, period, region, kind) row.
//
// Separate from AddTrafficUsage on purpose: traffic_usage stays THE billing
// total, and this is a breakdown of it. A breakdown write that fails leaves the
// total correct and only the split incomplete, which is the right way round -
// the reverse would let a broken breakdown change what a tenant is charged.
func (s *PostgresStore) AddTrafficUsageRegion(userID string, period time.Time, region, kind, product string, bytes int64) error {
	_, err := s.db.Exec(`
		INSERT INTO traffic_usage_region (user_id, period, region, kind, product, bytes, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (user_id, period, region, kind, product) DO UPDATE SET
			bytes      = traffic_usage_region.bytes + EXCLUDED.bytes,
			updated_at = NOW()`,
		userID, period, region, kind, product, bytes)
	return err
}

// GetTrafficUsageRegions returns the per-region, per-kind split for one tenant
// and period, largest first. An empty result is not an error: a tenant can have
// a total and no breakdown yet, which is exactly what a producer that predates
// region tagging leaves behind.
func (s *PostgresStore) GetTrafficUsageRegions(userID string, period time.Time) ([]RegionUsage, error) {
	rows, err := s.db.Query(`
		SELECT region, kind, product, bytes FROM traffic_usage_region
		WHERE user_id = $1 AND period = $2
		ORDER BY bytes DESC, region, kind, product`, userID, period)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RegionUsage
	for rows.Next() {
		var r RegionUsage
		if err := rows.Scan(&r.Region, &r.Kind, &r.Product, &r.Bytes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetTrafficBackupBytes overwrites (not adds) the R2 backup-storage snapshot for
// a tenant's period. Storage is a gauge (current total), not a cumulative flow,
// so each aggregation tick replaces the value.
func (s *PostgresStore) SetTrafficBackupBytes(userID string, period time.Time, backupBytes int64) error {
	_, err := s.db.Exec(`
		INSERT INTO traffic_usage (user_id, period, backup_bytes, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (user_id, period) DO UPDATE SET
			backup_bytes = EXCLUDED.backup_bytes,
			updated_at   = NOW()`,
		userID, period, backupBytes)
	return err
}

// GetTrafficUsage returns one tenant's usage for a period. A missing row is not
// an error: it returns a zero-value usage so callers render 0 instead of failing.
func (s *PostgresStore) GetTrafficUsage(userID string, period time.Time) (*TrafficUsage, error) {
	u := &TrafficUsage{UserID: userID, Period: period}
	err := s.db.QueryRow(`
		SELECT edge_bytes, relay_bytes, backup_bytes, updated_at
		FROM traffic_usage WHERE user_id = $1 AND period = $2`,
		userID, period).Scan(&u.EdgeBytes, &u.RelayBytes, &u.BackupBytes, &u.UpdatedAt)
	if err == sql.ErrNoRows {
		return u, nil
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// ListTrafficUsage returns every tenant's usage for a period, busiest first.
// Used by the admin usage overview.
func (s *PostgresStore) ListTrafficUsage(period time.Time) ([]TrafficUsage, error) {
	rows, err := s.db.Query(`
		SELECT t.user_id, COALESCE(u.username, ''), t.period, t.edge_bytes, t.relay_bytes, t.backup_bytes, t.updated_at
		FROM traffic_usage t
		LEFT JOIN users u ON u.id = t.user_id
		WHERE t.period = $1
		ORDER BY t.edge_bytes DESC`, period)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrafficUsage
	for rows.Next() {
		var u TrafficUsage
		if err := rows.Scan(&u.UserID, &u.Username, &u.Period, &u.EdgeBytes, &u.RelayBytes, &u.BackupBytes, &u.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
