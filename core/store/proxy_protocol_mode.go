package store

import (
	"database/sql"
	"errors"
)

// Proxy protocol modes (servers.proxy_protocol_mode). servers.proxy_protocol
// is the forwarding actually applied; the mode is the owner's choice.
const (
	ProxyModeAuto = "auto"
	ProxyModeOn   = "on"
	ProxyModeOff  = "off"
)

// ValidProxyMode reports whether m is one of the three modes.
func ValidProxyMode(m string) bool {
	return m == ProxyModeAuto || m == ProxyModeOn || m == ProxyModeOff
}

// ProxyAutoServer is a server whose mode is auto, with what is applied now.
type ProxyAutoServer struct {
	ID     int
	UUID   string
	Active bool
}

func (s *PostgresStore) GetServerProxyMode(id int) (string, error) {
	var m string
	err := s.db.QueryRow(`SELECT proxy_protocol_mode FROM servers WHERE id = $1`, id).Scan(&m)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return m, err
}

func (s *PostgresStore) SetServerProxyMode(id int, mode string) error {
	_, err := s.db.Exec(`UPDATE servers SET proxy_protocol_mode = $2 WHERE id = $1`, id, mode)
	return err
}

// ListProxyAutoServers is every server in auto mode, for the reconcile.
func (s *PostgresStore) ListProxyAutoServers() ([]ProxyAutoServer, error) {
	rows, err := s.db.Query(`SELECT id, uuid, proxy_protocol FROM servers WHERE proxy_protocol_mode = 'auto' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProxyAutoServer{}
	for rows.Next() {
		var p ProxyAutoServer
		if err := rows.Scan(&p.ID, &p.UUID, &p.Active); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
