package models

import "time"

// APIKey is an external automation credential. Plaintext key is
// shown once on creation; storage only ever holds sha256(plaintext).
//
// Scope shape (JSONB column):
//
//	{
//	  "servers":     ["uuid-1", "uuid-2"],   // empty = no servers
//	  "permissions": ["rcon.exec"]            // capabilities
//	}
//
// Capabilities live in JSONB so adding modpack.publish / server.power / etc.
// needs no schema migration. Today only rcon.exec is honored server-side.
type APIKey struct {
	ID         int         `json:"id"`
	UserID     string      `json:"userId"`
	Name       string      `json:"name"`
	KeyHash    string      `json:"-"`
	Scope      APIKeyScope `json:"scope"`
	LastUsedAt *time.Time  `json:"lastUsedAt,omitempty"`
	ExpiresAt  *time.Time  `json:"expiresAt,omitempty"`
	RevokedAt  *time.Time  `json:"revokedAt,omitempty"`
	RatePerMin int         `json:"ratePerMin"`
	CreatedAt  time.Time   `json:"createdAt"`
	// ViaPrevious is set by the store when the request presented the old hash of
	// a rolled key (still inside its grace). Never persisted or serialised.
	ViaPrevious bool `json:"-"`
}

type APIKeyScope struct {
	Servers     []string `json:"servers"`
	Permissions []string `json:"permissions"`
}

// AllowsServer returns true when the key's scope covers a given server UUID.
// Empty Servers list = no servers (deny by default).
func (s APIKeyScope) AllowsServer(serverUUID string) bool {
	for _, u := range s.Servers {
		if u == serverUUID {
			return true
		}
	}
	return false
}

// HasPermission returns true when perm is in the key's permission list.
func (s APIKeyScope) HasPermission(perm string) bool {
	for _, p := range s.Permissions {
		if p == perm {
			return true
		}
	}
	return false
}
