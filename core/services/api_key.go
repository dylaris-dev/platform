package services

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// HashAPIKey returns the canonical storage form of an API-key plaintext.
// Exported so API-key creation paths and external auth middleware agree.
func HashAPIKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// GeneratePlaintextKey returns a new random API key plaintext (32 bytes, hex).
func GeneratePlaintextKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
