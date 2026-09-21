package services

import (
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services/redisacl"
)

// TestSFTPNodeServersKey_MatchesTheNodeACLGrant pins the three-way agreement
// that makes SFTP work at all:
//
//	Core WRITES    sftp:node:<token>:user:<username>  (sftpNodeServersKey, here)
//	Core GRANTS    %R~sftp:node:<token>:*             (redisacl.BuildNodeACLRules)
//	the node READS sftp:node:<nodeID>:user:<username> where nodeID is the
//	                                                  Core-ASSIGNED identity,
//	                                                  i.e. the token
//	                                                  (node/sftp_server.go)
//
// The writer used to build its key from nodes.NAME. Name and token are equal at
// enrollment, so it worked everywhere until an admin used the panel's "Node
// Name" field (PATCH /nodes/{id}/config), at which point the writer moved to a
// key the reader never looks at AND the reader's ACL forbids. The failure is
// silent: authentication is keyed by username and still succeeds, so the user
// logs in and sees an EMPTY SFTP root.
//
// The node here therefore carries a Name that differs from its Token - a test
// where the two match cannot tell the two implementations apart.
func TestSFTPNodeServersKey_MatchesTheNodeACLGrant(t *testing.T) {
	const (
		token = "1f0b3f0e-2b1a-4a5e-9c3d-8f7a6b5c4d3e" // Core-minted, immutable
		name  = "frankfurt-box-01"                     // what an admin types in the panel
		user  = "alice"
	)
	node := models.Node{Token: token, Name: name, DisplayName: "Frankfurt Box"}

	key := sftpNodeServersKey(node, user)

	if strings.Contains(key, name) {
		t.Fatalf("the key was built from the node's admin-mutable name: %q", key)
	}
	if want := "sftp:node:" + token + ":user:" + user; key != want {
		t.Fatalf("key = %q, want %q", key, want)
	}

	// The grant is a glob, so compare the way Redis does: prefix up to the "*".
	var grant string
	for _, rule := range redisacl.BuildNodeACLRules(token, "pw", nil) {
		if s, ok := rule.(string); ok && strings.HasPrefix(s, "%R~sftp:node:") {
			grant = strings.TrimPrefix(s, "%R~")
			break
		}
	}
	if grant == "" {
		t.Fatal("BuildNodeACLRules no longer grants an sftp:node: pattern; " +
			"the node cannot read its server list at all under mandatory ACL")
	}
	if prefix := strings.TrimSuffix(grant, "*"); !strings.HasPrefix(key, prefix) {
		t.Errorf("Core writes %q but grants the node only %q - every SFTP session on this node sees an empty root", key, grant)
	}
}

// TestSFTPNodeServersKey_DistinctPerNodeAndUser guards the other direction: one
// node must not read another's list, and one user must not see another's
// servers. Both rest entirely on this key's shape, since the ACL grant is a
// prefix glob over everything after the token.
func TestSFTPNodeServersKey_DistinctPerNodeAndUser(t *testing.T) {
	nodeA := models.Node{Token: "node-a", Name: "shared-name"}
	nodeB := models.Node{Token: "node-b", Name: "shared-name"}

	a := sftpNodeServersKey(nodeA, "alice")
	if got := sftpNodeServersKey(nodeB, "alice"); got == a {
		t.Errorf("two nodes share one key: %q", got)
	}
	if got := sftpNodeServersKey(nodeA, "bob"); got == a {
		t.Errorf("two users share one key: %q", got)
	}
}

// Whether a node serves SFTP at all is a different question from whether a
// given user may use it, and it had no answer here. The panel's credentials
// route refused with "beam_only" while this publisher kept every credential an
// SFTP session needs alive in Redis and the node kept accepting them.
//
// Measured on production: with file_access_mode = beam, a delegate logged in
// over SFTP with their panel password and listed their server's files, and the
// bcrypt hash of every user's password was being republished every 60 seconds
// for a feature the operator had switched off.
func TestSFTPServedBy(t *testing.T) {
	platform := models.Node{Token: "tok"}
	external := models.Node{Token: "tok", Tags: "byon,external"}

	cases := []struct {
		name     string
		node     models.Node
		fileMode string
		want     bool
	}{
		{"sftp mode serves it", platform, "sftp", true},
		{"both serves it", platform, "both", true},
		{"beam does NOT", platform, "beam", false},
		// A platform that never saved the setting has no row at all, and the
		// panel route treats only "beam" as off. Anything else here would change
		// behaviour on every install that never opened the form.
		{"never configured behaves as before", platform, "", true},
		{"an unknown value is not 'beam'", platform, "something-else", true},
		// An external node forces beam locally whatever the platform says -
		// the same rule handlers/servers_sftp.go applies.
		{"an external node never serves it", external, "sftp", false},
		{"an external node in beam mode either", external, "beam", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sftpServedBy(c.node, c.fileMode); got != c.want {
				t.Errorf("sftpServedBy(external=%v, %q) = %v, want %v", c.node.IsExternal(), c.fileMode, got, c.want)
			}
		})
	}
}
