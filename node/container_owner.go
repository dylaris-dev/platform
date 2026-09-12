package main

import (
	"os"
	"strings"
)

// Which MC containers on this Docker daemon are THIS node's.
//
// Every node drives the host's docker.sock, and MC containers are host siblings
// rather than children - so ContainerList shows a node every mc_ container on
// the machine, including ones another node created. Nothing distinguished them:
// the containers carried no labels at all, and ListAllMCContainers matched on
// the "mc_" name prefix alone.
//
// With one node per machine that is correct and cheaper than a label. With two,
// each node treats the other's servers as its own. Measured on a developer
// machine running the test stack and a BYON node side by side:
//
//   - Both nodes published stats for the same two containers.
//   - ReconcileRedisEnv on the second node's STARTUP compared the first node's
//     containers against its own Redis address, found them "stale", and
//     restarted both - taking two running Minecraft servers down with their
//     players, from a node that was not even enrolled.
//   - The second node had created data directories for the first node's servers.
//
// So containers are labelled with the node that created them, and the listers
// filter on it.

// ownerLabel names the node that created a container. Namespaced because it goes
// onto a container the operator may also label for their own purposes.
const ownerLabel = "com.dylaris.node"

// roleLabel says WHAT a container is, where ownerLabel says whose it is. Set by
// the image rather than by whoever deploys it (gateway's root Dockerfile), so a
// copy of that image in somebody's own registry, under a name nothing here can
// predict, is still recognisable - see isLinkContainer.
//
// Namespaced for the same reason ownerLabel is. The unnamespaced dylaris.role
// this node puts on NETWORKS is a different object and stays as it is; renaming
// it would strand the networks that already carry it.
const (
	roleLabel = "com.dylaris.role"
	roleLink  = "link"
)

// nodeIdentity is the value written into the label on a NEW container.
//
// The server-assigned id when there is one, because that is the identity Core
// and the Redis ACL know this node by. The configured NODE_ID otherwise: a node
// that has not enrolled yet still creates containers, and a label that were
// empty until enrolment would leave exactly the startup window this fix is
// about unprotected.
//
// Deliberately not the hostname. Two containers on one host share it, which is
// the case that has to be told apart.
func nodeIdentity(secretDir string) string {
	if id, ok := loadNodeID(secretDir); ok {
		return id
	}
	return strings.TrimSpace(os.Getenv("NODE_ID"))
}

// nodeIdentities is every name this node answers to when READING a label.
//
// More than one, and that is the point. A node labels its first containers with
// NODE_ID because it has not enrolled yet, then enrols and gains a
// server-assigned id - so the value nodeIdentity returns CHANGES under a node
// that is running normally. Matching on only the current one would make a node
// disown the containers it created an hour earlier, which is the same failure
// this whole file exists to prevent, just arriving from the other direction.
func nodeIdentities(secretDir string) []string {
	var out []string
	if id, ok := loadNodeID(secretDir); ok {
		out = append(out, id)
	}
	if v := strings.TrimSpace(os.Getenv("NODE_ID")); v != "" && (len(out) == 0 || out[0] != v) {
		out = append(out, v)
	}
	return out
}

// ownsContainer decides whether this node may act on a container.
//
// An UNLABELLED container is treated as ours. Every container that exists today
// predates the label, and reading them as somebody else's would orphan a running
// fleet on the first upgrade - the node would stop reconciling its own servers,
// which is worse than the problem being fixed. They gain the label when they are
// next recreated.
//
// An empty identity - a node that knows neither id - also claims everything,
// for the same reason: the old behaviour is the safe fallback, and a node that
// cannot name itself must not silently stop managing its own containers.
func ownsContainer(labels map[string]string, self []string) bool {
	owner := strings.TrimSpace(labels[ownerLabel])
	if owner == "" || len(self) == 0 {
		return true
	}
	for _, id := range self {
		if owner == id {
			return true
		}
	}
	return false
}
