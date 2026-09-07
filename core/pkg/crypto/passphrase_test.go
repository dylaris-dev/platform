package crypto

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

const testPassphrase = "correct horse battery staple"

func TestBundleKDF_RoundTrip(t *testing.T) {
	h, key, err := NewBundleKDF(testPassphrase)
	if err != nil {
		t.Fatalf("NewBundleKDF: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("key is %d bytes, want 32", len(key))
	}

	got, err := h.DeriveKey(testPassphrase)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Fatal("re-derived key differs from the one the bundle was sealed with")
	}
}

func TestBundleKDF_WrongPassphraseIsRefusedBeforeAnythingIsReturned(t *testing.T) {
	h, key, err := NewBundleKDF(testPassphrase)
	if err != nil {
		t.Fatalf("NewBundleKDF: %v", err)
	}

	got, err := h.DeriveKey(testPassphrase + "x")
	if !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("err = %v, want ErrWrongPassphrase", err)
	}
	// The point of the verifier is that a restore can refuse before it writes.
	// Handing back a key alongside the error would let a careless caller carry
	// on and decrypt every value into garbage.
	if got != nil {
		t.Fatal("a wrong passphrase returned a key")
	}
	if bytes.Equal(got, key) {
		t.Fatal("a wrong passphrase produced the right key")
	}
}

func TestBundleKDF_SamePassphraseGivesDifferentKeysPerBundle(t *testing.T) {
	a, keyA, err := NewBundleKDF(testPassphrase)
	if err != nil {
		t.Fatalf("NewBundleKDF: %v", err)
	}
	b, keyB, err := NewBundleKDF(testPassphrase)
	if err != nil {
		t.Fatalf("NewBundleKDF: %v", err)
	}

	if a.Salt == b.Salt {
		t.Fatal("two bundles share a salt")
	}
	// A key recovered from one bundle - by any means, including the operator
	// pasting it somewhere - must not open the next one.
	if bytes.Equal(keyA, keyB) {
		t.Fatal("two bundles with the same passphrase share a key")
	}
	if a.Verifier == b.Verifier {
		t.Fatal("two bundles with the same passphrase share a verifier")
	}
}

func TestBundleKDF_VerifierIsNotTheKey(t *testing.T) {
	h, key, err := NewBundleKDF(testPassphrase)
	if err != nil {
		t.Fatalf("NewBundleKDF: %v", err)
	}
	// The verifier is published in the bundle's plaintext header. If it were
	// the key, or contained it, the header would be the whole secret.
	ver, err := hex.DecodeString(h.Verifier)
	if err != nil {
		t.Fatalf("verifier is not hex: %v", err)
	}
	if bytes.Equal(ver, key) {
		t.Fatal("the published verifier IS the encryption key")
	}
	if bytes.Contains(ver, key) || bytes.Contains(key, ver) {
		t.Fatal("the published verifier overlaps the encryption key")
	}
}

func TestNewBundleKDF_RefusesAShortPassphrase(t *testing.T) {
	short := strings.Repeat("a", MinPassphraseLength-1)
	if _, _, err := NewBundleKDF(short); !errors.Is(err, ErrPassphraseTooShort) {
		t.Fatalf("err = %v, want ErrPassphraseTooShort", err)
	}
	// The floor applies where a passphrase is SET, not where one is checked.
	if _, _, err := NewBundleKDF(strings.Repeat("a", MinPassphraseLength)); err != nil {
		t.Fatalf("a passphrase exactly at the floor was refused: %v", err)
	}
}

// A bundle header is a file someone handed us, so every field in it is
// attacker-controlled. Out-of-range parameters must be refused rather than
// obeyed - upwards because 64 GiB of argon2 memory is a one-line way to kill
// Core, downwards because a tiny cost silently turns the KDF into something
// brute-forceable on a bundle the attacker wrote themselves.
func TestBundleKDF_RefusesHostileHeaderParameters(t *testing.T) {
	good, _, err := NewBundleKDF(testPassphrase)
	if err != nil {
		t.Fatalf("NewBundleKDF: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(h *BundleKDF)
	}{
		{"memory absurdly high", func(h *BundleKDF) { h.Memory = 64 * 1024 * 1024 }},
		{"memory below the floor", func(h *BundleKDF) { h.Memory = 8 }},
		{"time zero", func(h *BundleKDF) { h.Time = 0 }},
		{"time absurdly high", func(h *BundleKDF) { h.Time = 1000 }},
		{"threads zero", func(h *BundleKDF) { h.Threads = 0 }},
		{"threads absurdly high", func(h *BundleKDF) { h.Threads = 255 }},
		{"unknown algorithm", func(h *BundleKDF) { h.Algorithm = "sha256" }},
		{"salt not hex", func(h *BundleKDF) { h.Salt = "zzzz" }},
		{"salt empty", func(h *BundleKDF) { h.Salt = "" }},
		{"verifier not hex", func(h *BundleKDF) { h.Verifier = "zzzz" }},
		{"verifier wrong length", func(h *BundleKDF) { h.Verifier = "aabb" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := *good
			tc.mutate(&h)
			key, err := h.DeriveKey(testPassphrase)
			if err == nil {
				t.Fatal("hostile header was accepted")
			}
			if key != nil {
				t.Fatal("hostile header returned a key")
			}
		})
	}
}

func TestBundleKDF_NilHeaderIsAnErrorNotAPanic(t *testing.T) {
	var h *BundleKDF
	if _, err := h.DeriveKey(testPassphrase); err == nil {
		t.Fatal("a missing key header was accepted")
	}
}

func TestReseal_MovesAValueBetweenKeys(t *testing.T) {
	from := DeriveKey("old-cluster-secret", "settings")
	to := DeriveKey("new-cluster-secret", "settings")

	sealed, err := Encrypt(from, []byte("s3-secret-access-key"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	moved, err := Reseal(from, to, sealed)
	if err != nil {
		t.Fatalf("Reseal: %v", err)
	}
	if moved == sealed {
		t.Fatal("Reseal returned the input unchanged")
	}

	pt, err := Decrypt(to, moved)
	if err != nil {
		t.Fatalf("Decrypt under the target key: %v", err)
	}
	if string(pt) != "s3-secret-access-key" {
		t.Fatalf("plaintext = %q", pt)
	}
	// The whole point of a rotation is that the old key stops working.
	if _, err := Decrypt(from, moved); err == nil {
		t.Fatal("the resealed value still opens under the old key")
	}
}

func TestReseal_EmptyStaysEmpty(t *testing.T) {
	from := DeriveKey("a", "p")
	to := DeriveKey("b", "p")
	// A column nobody ever set must not come back holding an encryption of "",
	// which every reader downstream would take for a stored secret.
	got, err := Reseal(from, to, "")
	if err != nil {
		t.Fatalf("Reseal: %v", err)
	}
	if got != "" {
		t.Fatalf("empty ciphertext became %q", got)
	}
}

func TestReseal_RefusesAValueTheSourceKeyCannotOpen(t *testing.T) {
	from := DeriveKey("a", "p")
	other := DeriveKey("c", "p")
	to := DeriveKey("b", "p")

	sealed, err := Encrypt(other, []byte("value"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// Silently passing an unreadable value through would write garbage into the
	// new column and lose the ciphertext the old key could still have opened.
	if _, err := Reseal(from, to, sealed); err == nil {
		t.Fatal("Reseal accepted a value the source key cannot decrypt")
	}
}
