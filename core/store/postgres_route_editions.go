package store

import "fmt"

// RouteEdition is one route's player editions. Only routes that differ from
// the default (Java only) have a row.
type RouteEdition struct {
	Domain     string
	ServerID   int
	ServerUUID string
	Java       bool
	Bedrock    bool
}

// SetRouteEdition stores which editions a route takes. Java only is the
// default and is stored as no row, so every route that predates editions
// already reads as what it is.
func (s *PostgresStore) SetRouteEdition(serverID int, domain string, java, bedrock bool) error {
	if java && !bedrock {
		return s.DeleteRouteEdition(domain)
	}
	if _, err := s.db.Exec(`INSERT INTO route_editions (domain, server_id, java, bedrock) VALUES ($1, $2, $3, $4)
		ON CONFLICT (domain) DO UPDATE SET server_id = EXCLUDED.server_id, java = EXCLUDED.java,
			bedrock = EXCLUDED.bedrock, updated_at = NOW()`, domain, serverID, java, bedrock); err != nil {
		return fmt.Errorf("route editions: set %s: %w", domain, err)
	}
	return nil
}

func (s *PostgresStore) DeleteRouteEdition(domain string) error {
	if _, err := s.db.Exec(`DELETE FROM route_editions WHERE domain = $1`, domain); err != nil {
		return fmt.Errorf("route editions: delete %s: %w", domain, err)
	}
	return nil
}

// ListRouteEditions lists every stored edition; serverID 0 means all servers.
func (s *PostgresStore) ListRouteEditions(serverID int) ([]RouteEdition, error) {
	rows, err := s.db.Query(`SELECT e.domain, e.server_id, sv.uuid, e.java, e.bedrock
		FROM route_editions e JOIN servers sv ON sv.id = e.server_id
		WHERE $1 = 0 OR e.server_id = $1 ORDER BY e.domain`, serverID)
	if err != nil {
		return nil, fmt.Errorf("route editions: list: %w", err)
	}
	defer rows.Close()
	var out []RouteEdition
	for rows.Next() {
		var e RouteEdition
		if err := rows.Scan(&e.Domain, &e.ServerID, &e.ServerUUID, &e.Java, &e.Bedrock); err != nil {
			return nil, fmt.Errorf("route editions: scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
