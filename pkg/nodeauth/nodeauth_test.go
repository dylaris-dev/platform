package nodeauth

import (
	"crypto/ed25519"
	"encoding/hex"
	"strconv"
	"testing"
)

// The vector was computed with crypto/ed25519 alone, outside this package, over
// "dylaris-node-auth:v1:" + goldenIdentity + ":" + goldenNonce. It pins the
// MESSAGE: Ed25519 is deterministic, so a changed prefix, separator, order or
// encoding moves these bytes, and a node and a Core that disagree on them cannot
// log in at all.
const (
	goldenIdentity = "d19b65e7-0bbf-43d1-9c62-374c3219733a"
	goldenNonce    = "5f1c0a9e3b7d42e8a6c1f0b9d8e7c6a5b4f3e2d1c0b9a8f7e6d5c4b3a2918070"
	goldenPub      = "03a107bff3ce10be1d70dd18e74bc09967e4d6309ba50d5f1ddc8664125531b8"
	goldenSig      = "8340bb212452f1f71093900c03674e4e9e73ebd8e1e6b1fa64c93c972291d8d9" +
		"98823fd20aa04fba77d16345167688962eb7c97e59163ede81aa25aea1b85a05"
)

// goldenKey is the key from seed bytes 0x00..0x1f.
func goldenKey() ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSignChallengeGoldenVector(t *testing.T) {
	k := goldenKey()
	if got := hex.EncodeToString(k.Public().(ed25519.PublicKey)); got != goldenPub {
		t.Fatalf("public key = %s, want %s", got, goldenPub)
	}
	sig := SignChallenge(k, goldenIdentity, goldenNonce)
	if got := hex.EncodeToString(sig); got != goldenSig {
		t.Fatalf("signature = %s\nwant        %s", got, goldenSig)
	}
	if !VerifyChallenge(mustHex(t, goldenPub), goldenIdentity, goldenNonce, sig) {
		t.Fatal("the golden signature does not verify")
	}
}

func TestVerifyChallengeRefuses(t *testing.T) {
	k := goldenKey()
	pub := k.Public().(ed25519.PublicKey)
	sig := SignChallenge(k, goldenIdentity, goldenNonce)
	flipped := append([]byte(nil), sig...)
	flipped[0] ^= 1
	_, other, _ := ed25519.GenerateKey(nil)

	cases := []struct {
		name     string
		pub      ed25519.PublicKey
		identity string
		nonce    string
		sig      []byte
	}{
		{"a signature over another nonce", pub, goldenIdentity, goldenNonce + "0", sig},
		{"a signature made for another node's login", pub, "another-node", goldenNonce, sig},
		{"a flipped bit", pub, goldenIdentity, goldenNonce, flipped},
		{"another key's signature", pub, goldenIdentity, goldenNonce, SignChallenge(other, goldenIdentity, goldenNonce)},
		{"no signature", pub, goldenIdentity, goldenNonce, nil},
		// ed25519.Verify panics on these; the key comes off the wire or a row.
		{"a short key", pub[:31], goldenIdentity, goldenNonce, sig},
		{"no key", nil, goldenIdentity, goldenNonce, sig},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if VerifyChallenge(c.pub, c.identity, c.nonce, c.sig) {
				t.Error("accepted")
			}
		})
	}
}

// canonicalSmallOrder are the canonical encodings of the eight points of small
// order: the only values ed25519.Verify can recompute as R when s = 0.
var canonicalSmallOrder = []string{
	"0100000000000000000000000000000000000000000000000000000000000000",
	"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
	"0000000000000000000000000000000000000000000000000000000000000000",
	"0000000000000000000000000000000000000000000000000000000000000080",
	"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05",
	"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc85",
	"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a",
	"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac03fa",
}

// forgeable reports whether the standard library accepts, under pub, a
// signature made with no private key at all: R a small-order point and s = 0,
// for at least one of 64 login messages.
func forgeable(t *testing.T, pub []byte) bool {
	t.Helper()
	for i := 0; i < 64; i++ {
		msg := challengeMessage("node-abc", strconv.Itoa(i))
		for _, r := range canonicalSmallOrder {
			if ed25519.Verify(pub, msg, append(mustHex(t, r), make([]byte, 32)...)) {
				return true
			}
		}
	}
	return false
}

// Every key the filter refuses, with either sign bit, is one the standard
// library lets anybody forge a login for - proven here, not asserted from the
// constants - and a real key passes the filter and resists the same forgery.
func TestSmallOrderKeysAreRefusedBecauseTheyAreForgeable(t *testing.T) {
	for _, b := range smallOrder {
		for _, sign := range []byte{0, 0x80} {
			pub := append([]byte(nil), b...)
			pub[31] |= sign
			name := hex.EncodeToString(pub)
			if ValidPublicKey(pub) {
				t.Errorf("%s passed the filter", name)
			}
			if !forgeable(t, pub) {
				t.Errorf("%s is refused, but no signature could be forged for it; the list is wrong", name)
			}
		}
	}

	identityPoint := mustHex(t, canonicalSmallOrder[0])
	forged := append(append([]byte(nil), identityPoint...), make([]byte, 32)...)
	if !ed25519.Verify(identityPoint, challengeMessage(goldenIdentity, goldenNonce), forged) {
		t.Fatal("the premise failed: the standard library refused the identity-point forgery")
	}
	if VerifyChallenge(identityPoint, goldenIdentity, goldenNonce, forged) {
		t.Error("the identity-point forgery logged in")
	}

	real := goldenKey().Public().(ed25519.PublicKey)
	if !ValidPublicKey(real) {
		t.Error("a real key was refused")
	}
	if forgeable(t, real) {
		t.Error("a real key was forged; the forgery check proves nothing")
	}
}
