// Package nodeauth is a node's login: an Ed25519 signature over the nonce Core
// issues on connect.
//
// It lives here, and not as one copy per module like the HMAC derivations in
// node/redisacl.go and core/services/redisacl/derive.go, because the message
// being signed is a wire contract between the two: a drift on either side locks
// every node out, and one copy cannot drift from itself.
package nodeauth

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
)

// challengeDomain separates this signature from anything else the key could
// ever be asked to sign. The v1 is the contract version: a change to the
// message is a new prefix, never an edit of this one.
const challengeDomain = "dylaris-node-auth:v1:"

// challengeMessage is what the node signs: the domain, the identity it presented
// as NodeAuth.node_token, and Core's nonce. The identity is in it so a signature
// proves the login of the node it was made for and no other - not a relayed
// answer, and not a key that ended up on two rows.
func challengeMessage(identity, nonce string) []byte {
	return []byte(challengeDomain + identity + ":" + nonce)
}

// SignChallenge is the node's answer to a Core-issued nonce, for the identity
// it presented in the same NodeAuth.
func SignChallenge(key ed25519.PrivateKey, identity, nonce string) []byte {
	return ed25519.Sign(key, challengeMessage(identity, nonce))
}

// VerifyChallenge reports whether sig answers nonce for identity under pub. A
// key ValidPublicKey refuses is a failed proof, never a panic: ed25519.Verify
// panics on a wrong-length key, and pub arrives from the network or a row.
func VerifyChallenge(pub ed25519.PublicKey, identity, nonce string, sig []byte) bool {
	if !ValidPublicKey(pub) {
		return false
	}
	return ed25519.Verify(pub, challengeMessage(identity, nonce), sig)
}

// ValidPublicKey reports whether pub can be a node's login key: 32 bytes, and not
// a point of small order. ed25519.Verify accepts those, and a signature can be
// made for them without any private key - the identity point 01 00..00 verifies
// every message with R = the identity and s = 0 - so registering one would let
// anybody log in as that node.
func ValidPublicKey(pub []byte) bool {
	if len(pub) != ed25519.PublicKeySize {
		return false
	}
	for _, bad := range smallOrder {
		if bytes.Equal(pub[:31], bad[:31]) && pub[31]&0x7f == bad[31] {
			return false
		}
	}
	return true
}

// smallOrder holds every encoding of a point of order 1, 2, 4 or 8 that
// crypto/ed25519 decodes, compared with the sign bit (bit 255) masked off: the
// eight points, and the non-canonical encodings y = p and y = p + 1 of y = 0 and
// y = 1, which the standard library also accepts. It is libsodium's
// has_small_order list. TestSmallOrderKeysAreRefusedBecauseTheyAreForgeable
// forges a signature for every entry against the standard library rather than
// trusting these constants.
var smallOrder = func() [][]byte {
	var out [][]byte
	for _, h := range []string{
		"0000000000000000000000000000000000000000000000000000000000000000", // y = 0, order 4
		"0100000000000000000000000000000000000000000000000000000000000000", // y = 1, the identity
		"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05", // order 8
		"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a", // order 8
		"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f", // y = -1, order 2
		"edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f", // y = p, a second y = 0
		"eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f", // y = p + 1, a second identity
	} {
		b, err := hex.DecodeString(h)
		if err != nil {
			panic(err)
		}
		out = append(out, b)
	}
	return out
}()
