package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log"

	"dylaris-core/pkg/crypto"
)

// LinkRouteEdition is one route-only address's editions and its Floodgate key.
// OwnerID comes from the route, so the published key name can be derived.
type LinkRouteEdition struct {
	Domain  string
	OwnerID string
	Java    bool
	Bedrock bool
	Key     []byte
}

// SetLinkRouteEdition stores which editions a route-only address takes. key is
// used only when the row is new: an existing key is kept, because the customer
// has already copied it onto their server.
func (s *PostgresStore) SetLinkRouteEdition(domain string, java, bedrock bool, key []byte) error {
	if s.bedrockKey == nil {
		return errNoBedrockKey
	}
	enc, err := crypto.Encrypt(s.bedrockKey, key)
	if err != nil {
		return fmt.Errorf("link route editions: encrypt key: %w", err)
	}
	if _, err := s.db.Exec(`INSERT INTO link_route_editions (domain, java, bedrock, floodgate_key_enc) VALUES ($1, $2, $3, $4)
		ON CONFLICT (domain) DO UPDATE SET java = EXCLUDED.java, bedrock = EXCLUDED.bedrock, updated_at = NOW()`,
		domain, java, bedrock, enc); err != nil {
		return fmt.Errorf("link route editions: set %s: %w", domain, err)
	}
	return nil
}

// GetLinkRouteEdition returns one address's row, or nil when it has none
// (Java only, never given Bedrock).
func (s *PostgresStore) GetLinkRouteEdition(domain string) (*LinkRouteEdition, error) {
	var e LinkRouteEdition
	var enc string
	err := s.db.QueryRow(`SELECT e.domain, r.owner_id, e.java, e.bedrock, e.floodgate_key_enc
		FROM link_route_editions e JOIN core_link_routes r ON r.domain = e.domain WHERE e.domain = $1`, domain).
		Scan(&e.Domain, &e.OwnerID, &e.Java, &e.Bedrock, &enc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("link route editions: get %s: %w", domain, err)
	}
	if e.Key, err = s.decodeBedrockKey(enc); err != nil {
		return nil, fmt.Errorf("link route editions: %s: %w", domain, err)
	}
	return &e, nil
}

// ListLinkRouteEditions lists every route-only address with a row. A key that
// cannot be decrypted (a row a reseal could not read) comes back as Key nil
// and is logged, instead of failing the list: PublishEditions runs this for
// every Bedrock change on the platform, server routes included, and one bad
// row must not freeze them all.
func (s *PostgresStore) ListLinkRouteEditions() ([]LinkRouteEdition, error) {
	rows, err := s.db.Query(`SELECT e.domain, r.owner_id, e.java, e.bedrock, e.floodgate_key_enc
		FROM link_route_editions e JOIN core_link_routes r ON r.domain = e.domain ORDER BY e.domain`)
	if err != nil {
		return nil, fmt.Errorf("link route editions: list: %w", err)
	}
	defer rows.Close()
	var out []LinkRouteEdition
	for rows.Next() {
		var e LinkRouteEdition
		var enc string
		if err := rows.Scan(&e.Domain, &e.OwnerID, &e.Java, &e.Bedrock, &enc); err != nil {
			return nil, fmt.Errorf("link route editions: scan: %w", err)
		}
		if e.Key, err = s.decodeBedrockKey(enc); err != nil {
			log.Printf("link route editions: %s: key unreadable, edition kept without it: %v", e.Domain, err)
			e.Key = nil
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
