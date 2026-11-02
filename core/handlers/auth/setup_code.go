package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"strings"
)

// SetupCodeSettingKey holds the SHA-256 of the one-time setup code. Only the
// hash is stored: the plaintext exists in the Core log of the boot that
// generated it and nowhere else. Deleting this row and restarting Core is the
// documented way to get a new code.
const SetupCodeSettingKey = "setup.code_hash"

// Uppercase letters and digits without the pairs people misread from a log
// (0/O, 1/I/L). 31 symbols, 12 of them: about 59 bits, behind a rate limit.
const setupCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

const (
	setupCodeGroups   = 3
	setupCodeGroupLen = 4
)

// newSetupCode returns a fresh code formatted as XXXX-XXXX-XXXX.
func newSetupCode() (string, error) {
	max := big.NewInt(int64(len(setupCodeAlphabet)))
	var b strings.Builder
	for i := 0; i < setupCodeGroups*setupCodeGroupLen; i++ {
		if i > 0 && i%setupCodeGroupLen == 0 {
			b.WriteByte('-')
		}
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("setup code: %w", err)
		}
		b.WriteByte(setupCodeAlphabet[n.Int64()])
	}
	return b.String(), nil
}

// normalizeSetupCode forgives what a person copying from a log does: lower
// case, missing or extra dashes, surrounding spaces.
func normalizeSetupCode(s string) string {
	s = strings.ToUpper(s)
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
}

func hashSetupCode(code string) string {
	sum := sha256.Sum256([]byte(normalizeSetupCode(code)))
	return hex.EncodeToString(sum[:])
}

// setupCodeMatches compares in constant time. An empty stored hash never
// matches, so a missing row cannot be satisfied by an empty code.
func setupCodeMatches(storedHash, provided string) bool {
	if storedHash == "" || normalizeSetupCode(provided) == "" {
		return false
	}
	want := []byte(storedHash)
	got := []byte(hashSetupCode(provided))
	return subtle.ConstantTimeCompare(want, got) == 1
}

// setupCodeStore is the slice of store.Store the boot step needs.
type setupCodeStore interface {
	CountUsers() (int, error)
	InsertSettingIfAbsent(key, value string) (bool, error)
	DeleteSetting(key string) error
}

// EnsureSetupCode runs once per Core boot. On an install without any user it
// makes sure exactly one setup code exists; once users exist it removes any
// leftover code.
//
// Several replicas can boot at once: each tries an insert-if-absent and only
// the one whose insert landed logs its plaintext. The others see the stored
// hash and log that a code exists, never a code of their own that would not
// work.
func EnsureSetupCode(st setupCodeStore) error {
	users, err := st.CountUsers()
	if err != nil {
		return fmt.Errorf("setup code: count users: %w", err)
	}
	if users > 0 {
		if err := st.DeleteSetting(SetupCodeSettingKey); err != nil {
			return fmt.Errorf("setup code: delete: %w", err)
		}
		return nil
	}
	code, err := newSetupCode()
	if err != nil {
		return err
	}
	inserted, err := st.InsertSettingIfAbsent(SetupCodeSettingKey, hashSetupCode(code))
	if err != nil {
		return fmt.Errorf("setup code: store: %w", err)
	}
	if inserted {
		log.Printf("SETUP CODE: %s (needed once to create the first administrator)", code)
		return nil
	}
	log.Printf("setup: a setup code was generated on an earlier start and is still unused; find it with: docker compose logs core | grep 'SETUP CODE'. Lost it? Delete the settings row %q and restart Core to get a new one.", SetupCodeSettingKey)
	return nil
}
