package models

import "time"

// BeamIssuerKey is an Ed25519 public key a managed-gateway customer's own Core
// signs beam tickets with. ID is the kid in the ticket header (16 random bytes,
// hex). Our Core publishes the live ones to beam:issuer:<kid> for the relay.
type BeamIssuerKey struct {
	ID        string
	OwnerID   string
	PublicKey []byte // 32 bytes
	CreatedAt time.Time
	RevokedAt *time.Time
}
