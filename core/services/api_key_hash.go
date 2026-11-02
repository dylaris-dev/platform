package services

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashAPIKey returns the canonical storage form of an API-key plaintext.
// Exported so API-key creation paths and external auth middleware agree.
func HashAPIKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
