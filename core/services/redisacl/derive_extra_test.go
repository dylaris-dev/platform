package redisacl

import "testing"

// TestGoldenVectorsExtended pins additional derivation vectors beyond the
// original node/shipper/proof set in derive_test.go (link, challenge,
// heartbeat, cluster-proof). MUST be reproduced byte-identically by
// node/redisacl_extra_test.go's TestACLGoldenVectorsExtended - a drift here
// silently breaks the corresponding node-side auth path. This file only
// ADDS vectors; the original derive_test.go is untouched.
func TestGoldenVectorsExtended(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	const token = "node-a"

	if got, want := LinkPassword(secret, token), "6245236664161e1d08ad351374d45c986a98c6737f103c3479645936773fa3de"; got != want {
		t.Errorf("LinkPassword vector drift:\n got  %s\n want %s", got, want)
	}
	if got, want := LinkPassword(secret, "node-b"), "61980a19743ec48541a65d1b2d6b6f2a94b2370b90d6389ebd059b4795574415"; got != want {
		t.Errorf("LinkPassword(node-b) vector drift:\n got  %s\n want %s", got, want)
	}
	if LinkUsername("x") != "node-x-link" {
		t.Errorf("LinkUsername format wrong: %s", LinkUsername("x"))
	}

	if got, want := ChallengeResponse(secret, "test-nonce-1"), "d737f05576089d017677f16151a7b803c4adb9660a0bc6e2e045ec640414f34b"; got != want {
		t.Errorf("ChallengeResponse vector drift:\n got  %s\n want %s", got, want)
	}

	if got, want := HeartbeatSig(secret, token, 1700000000), "f554fe0872588fe9eb83ad1b668c07b11ce44c33794c2b63554d00e6be49a61d"; got != want {
		t.Errorf("HeartbeatSig vector drift:\n got  %s\n want %s", got, want)
	}

	if got, want := ClusterProof("test-cluster-secret", token), "1d4ece4f626894b29c4b904842dee7795f4ac62dd841eeeeb7a8d979ada847b2"; got != want {
		t.Errorf("ClusterProof vector drift:\n got  %s\n want %s", got, want)
	}
}

func TestChallengeAndHeartbeatRoundTrip(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")

	resp := ChallengeResponse(secret, "nonce-xyz")
	if !VerifyChallenge(secret, "nonce-xyz", resp) {
		t.Error("valid challenge response must verify")
	}
	if VerifyChallenge(secret, "nonce-xyz", "deadbeef") {
		t.Error("bad challenge response must not verify")
	}
	if VerifyChallenge(secret, "different-nonce", resp) {
		t.Error("challenge response must not verify under a different nonce")
	}

	sig := HeartbeatSig(secret, "node-a", 1700000000)
	if !VerifyHeartbeatSig(secret, "node-a", 1700000000, sig) {
		t.Error("valid heartbeat sig must verify")
	}
	if VerifyHeartbeatSig(secret, "node-a", 1700000001, sig) {
		t.Error("heartbeat sig must not verify under a different timestamp")
	}
}
