package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// ServerPort is one extra port a server publishes on its node in direct
// routing, next to the game port. HostPort is unique per node and protocol.
type ServerPort struct {
	ID            int       `json:"id"`
	ServerID      int       `json:"serverId"`
	NodeID        int       `json:"nodeId"`
	Protocol      string    `json:"protocol"`
	HostPort      int       `json:"hostPort"`
	ContainerPort int       `json:"containerPort"`
	Name          string    `json:"name"`
	Note          string    `json:"note"`
	Preset        string    `json:"preset,omitempty"`
	CreatedBy     string    `json:"-"`
	CreatedAt     time.Time `json:"createdAt"`
}

// ServerPortRow is a ServerPort with what the admin overview searches by.
type ServerPortRow struct {
	ServerPort
	ServerUUID string `json:"serverUuid"`
	ServerName string `json:"serverName"`
	OwnerID    string `json:"ownerId"`
	OwnerName  string `json:"ownerName"`
	NodeName   string `json:"nodeName"`
}

var (
	// ErrPortTaken is a host port another server already holds on that node.
	ErrPortTaken = errors.New("host port already taken on this node")
	// ErrPortRangeFull is a range with no free port left for the protocol.
	ErrPortRangeFull = errors.New("no free port left in the range")
)

const serverPortCols = `p.id, p.server_id, p.node_id, p.protocol, p.host_port, p.container_port,
	p.name, p.note, COALESCE(p.preset, ''), COALESCE(p.created_by::text, ''), p.created_at`

func scanServerPort(r interface{ Scan(...any) error }, extra ...any) (ServerPort, error) {
	var p ServerPort
	dest := append([]any{&p.ID, &p.ServerID, &p.NodeID, &p.Protocol, &p.HostPort, &p.ContainerPort,
		&p.Name, &p.Note, &p.Preset, &p.CreatedBy, &p.CreatedAt}, extra...)
	return p, r.Scan(dest...)
}

func (s *PostgresStore) ListServerPorts(serverID int) ([]ServerPort, error) {
	rows, err := s.db.Query(`SELECT `+serverPortCols+` FROM server_ports p WHERE p.server_id = $1 ORDER BY p.id`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ServerPort{}
	for rows.Next() {
		p, err := scanServerPort(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListAllServerPorts is the admin overview. q, when set, matches the port
// number exactly or the server name, uuid, owner name or port name/note as a
// substring, case-insensitively.
func (s *PostgresStore) ListAllServerPorts(q string) ([]ServerPortRow, error) {
	query := `SELECT ` + serverPortCols + `, s.uuid, s.name, COALESCE(s.owner_id::text, ''),
			COALESCE(u.username, ''), COALESCE(n.name, '')
		FROM server_ports p
		JOIN servers s ON s.id = p.server_id
		LEFT JOIN users u ON u.id = s.owner_id
		LEFT JOIN nodes n ON n.id = p.node_id`
	var args []any
	if q != "" {
		like := "%" + q + "%"
		args = append(args, like)
		cond := `s.name ILIKE $1 OR s.uuid::text ILIKE $1 OR u.username ILIKE $1 OR p.name ILIKE $1 OR p.note ILIKE $1`
		if n, err := strconv.Atoi(q); err == nil {
			args = append(args, n)
			cond += ` OR p.host_port = $2 OR p.container_port = $2`
		}
		query += ` WHERE ` + cond
	}
	query += ` ORDER BY p.node_id, p.protocol, p.host_port`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ServerPortRow{}
	for rows.Next() {
		var r ServerPortRow
		p, err := scanServerPort(rows, &r.ServerUUID, &r.ServerName, &r.OwnerID, &r.OwnerName, &r.NodeName)
		if err != nil {
			return nil, err
		}
		r.ServerPort = p
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PostgresStore) GetServerPort(id int) (*ServerPort, error) {
	p, err := scanServerPort(s.db.QueryRow(`SELECT `+serverPortCols+` FROM server_ports p WHERE p.id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// CreateServerPort inserts p with the host port it names; ErrPortTaken when
// that port is held on the node already.
func (s *PostgresStore) CreateServerPort(p ServerPort) (ServerPort, error) {
	var createdBy any
	if p.CreatedBy != "" {
		createdBy = p.CreatedBy
	}
	var preset any
	if p.Preset != "" {
		preset = p.Preset
	}
	err := s.db.QueryRow(`
		INSERT INTO server_ports (server_id, node_id, protocol, host_port, container_port, name, note, preset, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id, created_at`,
		p.ServerID, p.NodeID, p.Protocol, p.HostPort, p.ContainerPort, p.Name, p.Note, preset, createdBy,
	).Scan(&p.ID, &p.CreatedAt)
	if isUniqueViolation(err) {
		return ServerPort{}, ErrPortTaken
	}
	return p, err
}

// FreeServerPort returns the lowest port in [min, max] that the node does not
// hold for the protocol, skipping the ports in skip (for example the game
// ports). ErrPortRangeFull when there is none. It does not reserve the port:
// the insert's unique key is the lock, and the caller retries on ErrPortTaken.
func (s *PostgresStore) FreeServerPort(nodeID int, protocol string, min, max int, skip []int) (int, error) {
	taken := map[int]bool{}
	for _, p := range skip {
		taken[p] = true
	}
	rows, err := s.db.Query(`SELECT host_port FROM server_ports WHERE node_id = $1 AND protocol = $2 AND host_port BETWEEN $3 AND $4`,
		nodeID, protocol, min, max)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return 0, err
		}
		taken[p] = true
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for p := min; p <= max; p++ {
		if !taken[p] {
			return p, nil
		}
	}
	return 0, ErrPortRangeFull
}

func (s *PostgresStore) UpdateServerPortMeta(id int, name, note string) error {
	_, err := s.db.Exec(`UPDATE server_ports SET name = $2, note = $3 WHERE id = $1`, id, name, note)
	return err
}

// MoveServerPort re-takes a port on another node (the server moved);
// ErrPortTaken when that host port is held there.
func (s *PostgresStore) MoveServerPort(id, nodeID, hostPort int) error {
	_, err := s.db.Exec(`UPDATE server_ports SET node_id = $2, host_port = $3 WHERE id = $1`, id, nodeID, hostPort)
	if isUniqueViolation(err) {
		return ErrPortTaken
	}
	return err
}

func (s *PostgresStore) DeleteServerPort(id int) error {
	_, err := s.db.Exec(`DELETE FROM server_ports WHERE id = $1`, id)
	return err
}

// CountServerPortsByOwner counts the extra ports on every server ownerID owns.
func (s *PostgresStore) CountServerPortsByOwner(ownerID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM server_ports p JOIN servers s ON s.id = p.server_id WHERE s.owner_id = $1`,
		ownerID).Scan(&n)
	return n, err
}

// ListServerIDsWithPorts is every server that has at least one extra port.
func (s *PostgresStore) ListServerIDsWithPorts() ([]int, error) {
	rows, err := s.db.Query(`SELECT DISTINCT server_id FROM server_ports ORDER BY server_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PortPermission is one scope's answer to "may this server have extra ports".
type PortPermission struct {
	Scope     string    `json:"scope"`
	Allowed   bool      `json:"allowed"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (s *PostgresStore) GetPortPermission(scope string) (*bool, error) {
	var allowed bool
	err := s.db.QueryRow(`SELECT allowed FROM port_permissions WHERE scope = $1`, scope).Scan(&allowed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &allowed, nil
}

func (s *PostgresStore) SetPortPermission(scope string, allowed bool) error {
	_, err := s.db.Exec(`
		INSERT INTO port_permissions (scope, allowed, updated_at) VALUES ($1, $2, NOW())
		ON CONFLICT (scope) DO UPDATE SET allowed = EXCLUDED.allowed, updated_at = NOW()`, scope, allowed)
	return err
}

func (s *PostgresStore) DeletePortPermission(scope string) error {
	_, err := s.db.Exec(`DELETE FROM port_permissions WHERE scope = $1`, scope)
	return err
}

func (s *PostgresStore) ListPortPermissions() ([]PortPermission, error) {
	rows, err := s.db.Query(`SELECT scope, allowed, updated_at FROM port_permissions ORDER BY scope`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PortPermission{}
	for rows.Next() {
		var p PortPermission
		if err := rows.Scan(&p.Scope, &p.Allowed, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PortLimit is one scope's cap on extra ports per owner. MaxPorts nil means
// no limit; a scope without a row is not a PortLimit at all (it defers).
type PortLimit struct {
	Scope     string    `json:"scope"`
	MaxPorts  *int      `json:"maxPorts"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// GetPortLimit returns the scope's row, or nil, nil when the scope says nothing.
func (s *PostgresStore) GetPortLimit(scope string) (*PortLimit, error) {
	l := PortLimit{Scope: scope}
	var max sql.NullInt64
	err := s.db.QueryRow(`SELECT max_ports, updated_at FROM port_limits WHERE scope = $1`, scope).Scan(&max, &l.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if max.Valid {
		v := int(max.Int64)
		l.MaxPorts = &v
	}
	return &l, nil
}

func (s *PostgresStore) SetPortLimit(scope string, max *int) error {
	_, err := s.db.Exec(`
		INSERT INTO port_limits (scope, max_ports, updated_at) VALUES ($1, $2, NOW())
		ON CONFLICT (scope) DO UPDATE SET max_ports = EXCLUDED.max_ports, updated_at = NOW()`, scope, max)
	return err
}

func (s *PostgresStore) DeletePortLimit(scope string) error {
	_, err := s.db.Exec(`DELETE FROM port_limits WHERE scope = $1`, scope)
	return err
}

func (s *PostgresStore) ListPortLimits() ([]PortLimit, error) {
	rows, err := s.db.Query(`SELECT scope, max_ports, updated_at FROM port_limits ORDER BY scope`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PortLimit{}
	for rows.Next() {
		l := PortLimit{}
		var max sql.NullInt64
		if err := rows.Scan(&l.Scope, &max, &l.UpdatedAt); err != nil {
			return nil, err
		}
		if max.Valid {
			v := int(max.Int64)
			l.MaxPorts = &v
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// GetNodeExtraPortRanges maps node id -> its own extra port range, for the
// nodes that override the global one.
func (s *PostgresStore) GetNodeExtraPortRanges() (map[int]string, error) {
	rows, err := s.db.Query(`SELECT id, extra_port_range FROM nodes WHERE extra_port_range IS NOT NULL AND extra_port_range <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var id int
		var r string
		if err := rows.Scan(&id, &r); err != nil {
			return nil, err
		}
		out[id] = r
	}
	return out, rows.Err()
}

// SetNodeExtraPortRange sets the node's own range; "" goes back to the global one.
func (s *PostgresStore) SetNodeExtraPortRange(nodeID int, r string) error {
	var v any
	if r != "" {
		v = r
	}
	res, err := s.db.Exec(`UPDATE nodes SET extra_port_range = $2 WHERE id = $1`, nodeID, v)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("node %d: %w", nodeID, sql.ErrNoRows)
	}
	return nil
}
