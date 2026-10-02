package models

import "time"

// SSHKey is a public key an account signs in to SFTP with.
type SSHKey struct {
	ID          int       `json:"id"`
	UserID      string    `json:"-"`
	Name        string    `json:"name"`
	PublicKey   string    `json:"publicKey"`
	Fingerprint string    `json:"fingerprint"`
	CreatedAt   time.Time `json:"createdAt"`
}
