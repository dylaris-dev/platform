package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// LinkRouteVoice is a route-only address's public voice-chat port, with what
// the customer's link needs to carry it: the route's link and target host from
// core_link_routes, the voice mod's port on that host, and the uplink key.
type LinkRouteVoice struct {
	Domain     string
	OwnerID    string
	LinkToken  string
	TargetHost string
	Region     string
	Port       int
	TargetPort int
	UplinkKey  []byte
}

const linkRouteVoiceSelect = `
	SELECT v.domain, r.owner_id, r.link_token, r.target_host, v.region, v.port, v.target_port, v.uplink_key
	FROM link_route_voice v JOIN core_link_routes r ON r.domain = v.domain`

func scanLinkRouteVoice(row interface{ Scan(...any) error }) (LinkRouteVoice, error) {
	var v LinkRouteVoice
	err := row.Scan(&v.Domain, &v.OwnerID, &v.LinkToken, &v.TargetHost, &v.Region, &v.Port, &v.TargetPort, &v.UplinkKey)
	return v, err
}

// AllocateLinkRouteVoice gives a route-only domain a voice port in region, the
// same way AllocateVoicePort does for a server: an existing port in the same
// region keeps its port and key (only target_port follows), one in another
// region is released for a fresh one.
func (s *PostgresStore) AllocateLinkRouteVoice(domain, region string, targetPort int, key []byte, min, max int) (LinkRouteVoice, error) {
	for attempt := 0; ; attempt++ {
		got, err := s.allocateLinkRouteVoiceOnce(domain, region, targetPort, key, min, max)
		if err != nil && isUniqueViolation(err) && attempt < 3 {
			continue
		}
		return got, err
	}
}

func (s *PostgresStore) allocateLinkRouteVoiceOnce(domain, region string, targetPort int, key []byte, min, max int) (LinkRouteVoice, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return LinkRouteVoice{}, err
	}
	defer tx.Rollback()
	if err := lockVoicePool(tx, region); err != nil {
		return LinkRouteVoice{}, err
	}
	var cur string
	err = tx.QueryRow(`SELECT region FROM link_route_voice WHERE domain = $1 FOR UPDATE`, domain).Scan(&cur)
	switch {
	case err == nil && cur == region:
		if _, err := tx.Exec(`UPDATE link_route_voice SET target_port = $2 WHERE domain = $1`, domain, targetPort); err != nil {
			return LinkRouteVoice{}, err
		}
	case err == nil || errors.Is(err, sql.ErrNoRows):
		if err == nil {
			if _, err := tx.Exec(`DELETE FROM link_route_voice WHERE domain = $1`, domain); err != nil {
				return LinkRouteVoice{}, err
			}
		}
		port, err := freeVoicePort(tx, region, min, max)
		if err != nil {
			return LinkRouteVoice{}, err
		}
		if _, err := tx.Exec(`INSERT INTO link_route_voice (domain, region, port, target_port, uplink_key) VALUES ($1, $2, $3, $4, $5)`,
			domain, region, port, targetPort, key); err != nil {
			return LinkRouteVoice{}, err
		}
	default:
		return LinkRouteVoice{}, err
	}
	got, err := scanLinkRouteVoice(tx.QueryRow(linkRouteVoiceSelect+` WHERE v.domain = $1`, domain))
	if err != nil {
		return LinkRouteVoice{}, fmt.Errorf("read back link route voice: %w", err)
	}
	return got, tx.Commit()
}

// GetLinkRouteVoice returns the domain's voice port, or nil, nil when it has none.
func (s *PostgresStore) GetLinkRouteVoice(domain string) (*LinkRouteVoice, error) {
	v, err := scanLinkRouteVoice(s.db.QueryRow(linkRouteVoiceSelect+` WHERE v.domain = $1`, domain))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// DeleteLinkRouteVoice reports whether a row was removed.
func (s *PostgresStore) DeleteLinkRouteVoice(domain string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM link_route_voice WHERE domain = $1`, domain)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *PostgresStore) ListLinkRouteVoice() ([]LinkRouteVoice, error) {
	rows, err := s.db.Query(linkRouteVoiceSelect + ` ORDER BY v.region, v.port`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LinkRouteVoice
	for rows.Next() {
		v, err := scanLinkRouteVoice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
