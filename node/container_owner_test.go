package main

import (
	"os"
	"strings"
	"testing"
)

// The rule this decides: a node manages the MC containers IT created, and not
// the ones another node on the same docker.sock created.
//
// Measured before the label existed, on a machine running the test stack and a
// BYON node side by side: both nodes published stats for the same two
// containers, and the second node's STARTUP restarted the first node's two
// running Minecraft servers - players and all - because it compared their Redis
// address against its own and found it stale.
func TestOwnsContainer(t *testing.T) {
	self := []string{"node-a"}

	t.Run("its own", func(t *testing.T) {
		if !ownsContainer(map[string]string{ownerLabel: "node-a"}, self) {
			t.Error("a node disowned a container it created")
		}
	})

	t.Run("another node's", func(t *testing.T) {
		if ownsContainer(map[string]string{ownerLabel: "node-b"}, self) {
			t.Error("a node claimed another node's container")
		}
	})

	// The upgrade case, and the reason the default is "mine". Every container
	// running today predates the label; reading them as foreign would make a
	// node stop reconciling its own fleet the moment it is updated, which is a
	// worse failure than the one being fixed.
	t.Run("unlabelled is mine", func(t *testing.T) {
		if !ownsContainer(nil, self) {
			t.Error("a pre-label container was treated as someone else's")
		}
		if !ownsContainer(map[string]string{ownerLabel: "  "}, self) {
			t.Error("a blank label was treated as someone else's")
		}
	})

	// A node that cannot name itself keeps the old behaviour rather than
	// silently managing nothing.
	t.Run("a node with no identity claims everything", func(t *testing.T) {
		if !ownsContainer(map[string]string{ownerLabel: "node-b"}, nil) {
			t.Error("a node with no identity stopped managing containers")
		}
	})

	// The transition that would otherwise bite: a node labels its first
	// containers with NODE_ID, then enrols and gains a server-assigned id. It
	// has to keep answering to both, or it disowns what it made an hour ago.
	t.Run("it answers to every name it has had", func(t *testing.T) {
		both := []string{"assigned-id", "configured-id"}
		if !ownsContainer(map[string]string{ownerLabel: "configured-id"}, both) {
			t.Error("a container labelled before enrolment was disowned after it")
		}
		if !ownsContainer(map[string]string{ownerLabel: "assigned-id"}, both) {
			t.Error("a container labelled after enrolment was disowned")
		}
		if ownsContainer(map[string]string{ownerLabel: "somebody-else"}, both) {
			t.Error("answering to two names became answering to any")
		}
	})
}

func TestNodeIdentityPrefersTheAssignedID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NODE_ID", "configured-id")

	// Before enrolment there is only the configured one.
	if got := nodeIdentity(dir); got != "configured-id" {
		t.Errorf("nodeIdentity() = %q, want the configured id", got)
	}
	if got := nodeIdentities(dir); len(got) != 1 || got[0] != "configured-id" {
		t.Errorf("nodeIdentities() = %v, want just the configured id", got)
	}

	// After enrolment the server-assigned one leads, because that is what Core
	// and the Redis ACL know this node by - but the old one stays claimable.
	if err := saveNodeID(dir, "assigned-id"); err != nil {
		t.Fatal(err)
	}
	if got := nodeIdentity(dir); got != "assigned-id" {
		t.Errorf("nodeIdentity() = %q, want the assigned id", got)
	}
	got := nodeIdentities(dir)
	if len(got) != 2 || got[0] != "assigned-id" || got[1] != "configured-id" {
		t.Errorf("nodeIdentities() = %v, want both, assigned first", got)
	}
}

// The label has to actually be applied where containers are created, or the
// filter above is a no-op that reads as a fix. Both MC creation sites, named
// individually: a file-wide check is green when only one of them carries it.
func TestBothMCContainerSitesCarryTheOwnerLabel(t *testing.T) {
	b, err := os.ReadFile("docker_mgr.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "ownerLabel:      nodeIdentity(nodeSecretDir),"); n != 2 {
		t.Errorf("%d MC container configs stamp the owner label, want 2 - an unlabelled one is claimable by any node", n)
	}
	// And both listers filter, since either one unfiltered reopens the hole.
	if n := strings.Count(string(b), "ownsContainer(c.Labels, self)"); n != 2 {
		t.Errorf("%d listers filter by owner, want 2", n)
	}
}
