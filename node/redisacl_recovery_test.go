package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The regression this pins: a node that still has .node_secret on disk must
// STILL present its enroll token. Reset pairing wipes Core's copy of the secret
// and DELUSERs the node's Redis users but cannot reach the node's disk, so "has
// a cached secret" and "needs to re-pair" are true at the same time - and the
// else-chain that used to build this treated them as alternatives.
//
// NODE_RECOVERY_TOKEN is gone: re-admission is decided in the panel now, so the
// node has nothing to present for it and nothing to be configured with.
func TestBootstrapCreds(t *testing.T) {
	cases := []struct {
		name      string
		hasCached bool
		enroll    string
		wantProof bool
		wantToken string
	}{
		{
			name:      "first boot: no cache, enroll token only",
			enroll:    "enroll-token",
			wantToken: "enroll-token",
		},
		{
			name:      "steady state: cached secret, no token, proof only",
			hasCached: true,
			wantProof: true,
		},
		{
			name:      "a stale enroll token left in the env does not suppress the proof",
			hasCached: true,
			enroll:    "enroll-token",
			wantProof: true,
			wantToken: "enroll-token",
		},
		{
			name: "nothing to present at all - the state an operator now resolves from the panel",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proof, token := bootstrapCreds(c.hasCached, c.enroll)
			if proof != c.wantProof {
				t.Errorf("sendProof = %v, want %v", proof, c.wantProof)
			}
			if token != c.wantToken {
				t.Errorf("token = %q, want %q", token, c.wantToken)
			}
		})
	}
}

// The enroll token is only useful if it actually reaches the wire. Pin that
// bootstrapSecretViaGRPC assigns EnrollToken from bootstrapCreds' result and
// not from a branch of its own - a re-introduced `else if` there would restore
// the exact defect while TestBootstrapCreds stayed green.
func TestBootstrapAuthTakesItsCredentialsFromBootstrapCreds(t *testing.T) {
	src, err := os.ReadFile("redisacl_bootstrap.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	body := string(src)

	if !strings.Contains(body, "sendProof, token := bootstrapCreds(hasCached, nodeEnrollToken)") {
		t.Error("bootstrapSecretViaGRPC no longer asks bootstrapCreds what to present")
	}
	if !strings.Contains(body, "auth.EnrollToken = token") {
		t.Error("the token bootstrapCreds chose is no longer assigned to auth.EnrollToken")
	}
	// Any OTHER assignment to EnrollToken means a second, competing decision.
	assigns := regexp.MustCompile(`auth\.EnrollToken\s*=\s*(\w+)`).FindAllStringSubmatch(body, -1)
	if len(assigns) != 1 || assigns[0][1] != "token" {
		t.Errorf("auth.EnrollToken is assigned from %v; it must be assigned exactly once, from bootstrapCreds", assigns)
	}
}
