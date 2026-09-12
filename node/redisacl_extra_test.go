package main

import "testing"

// TestACLGoldenVectorsExtended MUST match
// core/services/redisacl/derive_extra_test.go's TestGoldenVectorsExtended
// exactly. Core pins its own side of the same three values; a change to a domain
// string on ONE side then turns exactly one of the two suites red. Without this
// file both stay green while every node in the fleet fails to authenticate.
//
// The link vectors that used to be here are gone with the node-managed Link: the
// node no longer derives a Link credential, and a golden vector for a derivation
// that no longer exists pins nothing. RouteOnlyLinkPassword never had a
// node-side equivalent (Core-only) - see the Core test's doc comment.
//
// What is left has nothing to do with the Link and is on the authentication path
// of every node:
//
//   - aclChallengeResponse: the ACL watchdog's proof of the per-node secret
//   - aclHeartbeatSig:      stamped on EVERY heartbeat, verified by Core
//   - aclClusterProof:      the cluster proof on the gRPC auth path
func TestACLGoldenVectorsExtended(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	const token = "node-a"

	if got, want := aclChallengeResponse(secret, "test-nonce-1"), "d737f05576089d017677f16151a7b803c4adb9660a0bc6e2e045ec640414f34b"; got != want {
		t.Errorf("aclChallengeResponse vector drift vs Core:\n got  %s\n want %s", got, want)
	}

	if got, want := aclHeartbeatSig(secret, token, 1700000000), "f554fe0872588fe9eb83ad1b668c07b11ce44c33794c2b63554d00e6be49a61d"; got != want {
		t.Errorf("aclHeartbeatSig vector drift vs Core:\n got  %s\n want %s", got, want)
	}

	if got, want := aclClusterProof("test-cluster-secret", token), "1d4ece4f626894b29c4b904842dee7795f4ac62dd841eeeeb7a8d979ada847b2"; got != want {
		t.Errorf("aclClusterProof vector drift vs Core:\n got  %s\n want %s", got, want)
	}
}
