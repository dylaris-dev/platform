package crypto

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"
)

// Passphrase-derived keys for platform backup bundles.
//
// A platform bundle carries the database, and the database holds values
// encrypted under CLUSTER_SECRET: node secrets, storage credentials, the
// Modrinth token, secret settings. Those values are re-encrypted under a
// passphrase the operator sets, so the bundle stops depending on the secret
// that happened to be current when it was written - CLUSTER_SECRET is on this
// platform's own rotation list, and a backup that a rotation invalidates turns
// security hygiene into a data-loss event.
//
// DeriveKey (sha256, next door) is deliberately NOT used here. It is the right
// tool for CLUSTER_SECRET, which is a long random value nobody guesses. A
// passphrase a human types and writes down is the opposite, and a single sha256
// puts an offline attacker holding a downloaded bundle at billions of guesses
// per second. argon2id costs the attacker the same as it costs us, which is the
// only defence a low-entropy input has.

// MinPassphraseLength is the floor enforced where a passphrase is SET. Low
// enough to be typed at 3am off a piece of paper, high enough that the argon2
// cost below is the binding constraint rather than the search space.
const MinPassphraseLength = 12

// Current argon2id parameters. They travel with each bundle rather than being
// compiled into the reader, so raising them later does not make every existing
// bundle unreadable.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 1
	argonSaltLen = 16
)

// One argon2id run yields both halves: the encryption key and the verifier that
// lets a restore refuse a wrong passphrase BEFORE it writes anything. Splitting
// one output rather than running the KDF twice costs an attacker exactly the
// same either way - testing a guess against the verifier is no cheaper than
// testing it against the ciphertext - and saves an honest restore a second
// 64 MiB pass.
const (
	bundleKeyLen      = 32
	bundleVerifierLen = 32
	bundleDerivedLen  = bundleKeyLen + bundleVerifierLen
)

// Bounds on the parameters read out of an untrusted bundle header. A bundle is
// a file someone hands us; without a ceiling, a header claiming 64 GiB of
// memory is a one-line way to kill Core. The floor is the other half: a header
// claiming 8 KiB and one pass would silently downgrade the KDF to something an
// attacker can brute-force, on a bundle they wrote themselves.
const (
	minArgonMemory  = 16 * 1024   // KiB
	maxArgonMemory  = 1024 * 1024 // KiB, 1 GiB
	minArgonTime    = 1
	maxArgonTime    = 10
	maxArgonThreads = 8
)

// ErrWrongPassphrase is what a restore shows the operator. It is deliberately
// indistinguishable from a corrupted verifier: both mean "this passphrase does
// not open this bundle", and there is nothing useful to tell apart.
var ErrWrongPassphrase = errors.New("crypto: wrong backup passphrase")

// ErrPassphraseTooShort is returned where a passphrase is set, never where one
// is checked - an existing bundle keeps working if the floor is raised later.
var ErrPassphraseTooShort = fmt.Errorf("crypto: backup passphrase must be at least %d characters", MinPassphraseLength)

// BundleKDF is the plaintext header a bundle carries so that any Dylaris can
// open it, including one that has never seen this passphrase. It holds no
// secret: the salt and the parameters are public inputs, and the verifier is
// the half of the derivation that decrypts nothing.
type BundleKDF struct {
	Algorithm string `json:"algorithm"` // "argon2id"
	Salt      string `json:"salt"`      // hex
	Memory    uint32 `json:"memory"`    // KiB
	Time      uint32 `json:"time"`
	Threads   uint8  `json:"threads"`
	Verifier  string `json:"verifier"` // hex
}

// NewBundleKDF picks a fresh salt for one bundle and returns the header to
// store alongside it plus the 32-byte key its contents are sealed with.
func NewBundleKDF(passphrase string) (*BundleKDF, []byte, error) {
	if len(passphrase) < MinPassphraseLength {
		return nil, nil, ErrPassphraseTooShort
	}
	salt := make([]byte, argonSaltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, nil, fmt.Errorf("crypto: salt: %w", err)
	}
	h := &BundleKDF{
		Algorithm: "argon2id",
		Salt:      hex.EncodeToString(salt),
		Memory:    argonMemory,
		Time:      argonTime,
		Threads:   argonThreads,
	}
	derived := argon2.IDKey([]byte(passphrase), salt, h.Time, h.Memory, h.Threads, bundleDerivedLen)
	h.Verifier = hex.EncodeToString(derived[bundleKeyLen:])
	return h, derived[:bundleKeyLen], nil
}

// DeriveKey re-derives the bundle key from a passphrase, or reports that this
// is not the passphrase this bundle was written with.
//
// The verifier is checked in constant time. It is not a secret, so a timing
// leak here reveals nothing an attacker holding the bundle does not already
// have - but the same routine is what a hosted panel would call on an operator
// request, and a comparison that returns early on the first wrong byte is a
// habit, not a special case.
func (h *BundleKDF) DeriveKey(passphrase string) ([]byte, error) {
	if h == nil {
		return nil, errors.New("crypto: bundle has no key header")
	}
	if h.Algorithm != "argon2id" {
		return nil, fmt.Errorf("crypto: unsupported bundle key algorithm %q", h.Algorithm)
	}
	salt, err := hex.DecodeString(h.Salt)
	if err != nil || len(salt) == 0 {
		return nil, errors.New("crypto: bundle salt is unreadable")
	}
	want, err := hex.DecodeString(h.Verifier)
	if err != nil || len(want) != bundleVerifierLen {
		return nil, errors.New("crypto: bundle verifier is unreadable")
	}
	if h.Memory < minArgonMemory || h.Memory > maxArgonMemory ||
		h.Time < minArgonTime || h.Time > maxArgonTime ||
		h.Threads < 1 || h.Threads > maxArgonThreads {
		return nil, fmt.Errorf("crypto: bundle key parameters out of range (m=%d t=%d p=%d)", h.Memory, h.Time, h.Threads)
	}

	derived := argon2.IDKey([]byte(passphrase), salt, h.Time, h.Memory, h.Threads, bundleDerivedLen)
	if subtle.ConstantTimeCompare(derived[bundleKeyLen:], want) != 1 {
		return nil, ErrWrongPassphrase
	}
	return derived[:bundleKeyLen], nil
}

// Reseal moves one at-rest value from the key it is stored under to another.
//
// This is the whole re-encryption step, and it is the same operation in three
// places: writing a bundle (cluster secret -> passphrase), restoring one
// (passphrase -> the TARGET instance's cluster secret), and rotating
// CLUSTER_SECRET (old -> new). An empty ciphertext stays empty: a column that
// was never set must not come back holding an encryption of "".
func Reseal(fromKey, toKey []byte, encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	pt, err := Decrypt(fromKey, encoded)
	if err != nil {
		return "", fmt.Errorf("crypto: reseal decrypt: %w", err)
	}
	return Encrypt(toKey, pt)
}
