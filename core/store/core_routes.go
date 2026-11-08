package store

import "time"

// CoreLinkRoute is the durable record of one route-only entry.
//
// Core publishes these straight into Redis as route:<domain> rather than
// through the hub's queue, because the edge reaches the tenant's own link over
// the tenant's own tunnel and no hub row describes that. The Redis value was
// the ONLY copy, which made it the one piece of tenant configuration on the
// platform with no source of truth behind it. This row is that source; Redis is
// a cache of it.
type CoreLinkRoute struct {
	Domain     string
	OwnerID    string
	LinkToken  string
	TargetHost string
	TargetPort int
	// ProxyProtocol is the real-IP switch, published as "proxy_protocol" in
	// the route:<domain> value the edge reads.
	ProxyProtocol bool
	CreatedAt     time.Time
	// SuspendedAt is set while an admin has the route taken down; nil is the
	// normal case. The reason is shown to the owner.
	SuspendedAt     *time.Time
	SuspendedReason string
}

// UpsertCoreLinkRoute records a route-only entry, or rewrites the one already
// stored for that domain. The domain is the key for the same reason it is the
// key in Redis: one address routes to one place.
func (s *PostgresStore) UpsertCoreLinkRoute(r CoreLinkRoute) error {
	_, err := s.db.Exec(`
		INSERT INTO core_link_routes (domain, owner_id, link_token, target_host, target_port, proxy_protocol)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (domain) DO UPDATE SET
			owner_id = EXCLUDED.owner_id,
			link_token = EXCLUDED.link_token,
			target_host = EXCLUDED.target_host,
			target_port = EXCLUDED.target_port,
			proxy_protocol = EXCLUDED.proxy_protocol`,
		r.Domain, r.OwnerID, r.LinkToken, r.TargetHost, r.TargetPort, r.ProxyProtocol)
	return err
}

func (s *PostgresStore) ListCoreLinkRoutes() ([]CoreLinkRoute, error) {
	rows, err := s.db.Query(`
		SELECT domain, owner_id, link_token, target_host, target_port, proxy_protocol, created_at, suspended_at, COALESCE(suspended_reason, '')
		FROM core_link_routes ORDER BY domain`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CoreLinkRoute
	for rows.Next() {
		var r CoreLinkRoute
		if err := rows.Scan(&r.Domain, &r.OwnerID, &r.LinkToken, &r.TargetHost, &r.TargetPort, &r.ProxyProtocol, &r.CreatedAt, &r.SuspendedAt, &r.SuspendedReason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListCoreLinkRoutesByOwner is ListCoreLinkRoutes for one account, served by
// idx_core_link_routes_owner. Counting a tenant's routes used to scan every
// route in Redis, which grows with the platform instead of with the tenant.
func (s *PostgresStore) ListCoreLinkRoutesByOwner(ownerID string) ([]CoreLinkRoute, error) {
	rows, err := s.db.Query(`
		SELECT domain, owner_id, link_token, target_host, target_port, proxy_protocol, created_at, suspended_at, COALESCE(suspended_reason, '')
		FROM core_link_routes WHERE owner_id = $1 ORDER BY domain`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CoreLinkRoute
	for rows.Next() {
		var r CoreLinkRoute
		if err := rows.Scan(&r.Domain, &r.OwnerID, &r.LinkToken, &r.TargetHost, &r.TargetPort, &r.ProxyProtocol, &r.CreatedAt, &r.SuspendedAt, &r.SuspendedReason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PostgresStore) DeleteCoreLinkRoute(domain string) error {
	_, err := s.db.Exec(`DELETE FROM core_link_routes WHERE domain = $1`, domain)
	return err
}

// SetCoreLinkRouteSuspended suspends (at != nil) or unsuspends (nil) one route
// and reports whether the domain has a row. Upsert never touches these columns,
// so an owner re-saving the route cannot lift a suspension.
func (s *PostgresStore) SetCoreLinkRouteSuspended(domain string, at *time.Time, reason string) (bool, error) {
	var reasonArg any
	if at != nil {
		reasonArg = reason
	}
	res, err := s.db.Exec(`UPDATE core_link_routes SET suspended_at = $2, suspended_reason = $3 WHERE domain = $1`,
		domain, at, reasonArg)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
