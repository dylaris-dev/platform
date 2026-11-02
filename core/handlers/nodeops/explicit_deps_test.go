package nodeops

import (
	"context"
	"testing"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/models"
	"dylaris-core/services"
)

// The wrappers took *AppState before the move and now take its fields. These
// pin that each field still reaches the decision it fed.

func TestTheStallWrapperAsksTheRegistry(t *testing.T) {
	servers := []models.Server{srv("a", "installing", 1)}
	AnnotateStalledInstallsFor(context.Background(), nil, nil, servers)
	if servers[0].InstallStalled {
		t.Fatal("no registry must report nothing")
	}
	// An empty registry has node 1 disconnected; no Redis still reports.
	AnnotateStalledInstallsFor(context.Background(), nil, nodegrpc.NewRegistry(), servers)
	if !servers[0].InstallStalled {
		t.Fatal("a disconnected node's install was not reported as stalled")
	}
}

func TestLinkOnlineAsksAboutTheTokenDerivedFromTheClusterSecret(t *testing.T) {
	rdb, mr := stallRedis(t)
	owner := "u1"
	byon := models.Node{ID: 1, OwnerID: &owner}
	if !IsBYONNode(byon) {
		t.Fatal("fixture is not a BYON node")
	}
	tok := services.EffectiveLinkToken(&byon, "cluster-secret")
	mr.Set("online_link:"+tok, "1")

	nodes := []models.Node{byon}
	SetNodesLinkOnline(context.Background(), rdb, "cluster-secret", nodes)
	if nodes[0].LinkOnline == nil || !*nodes[0].LinkOnline {
		t.Fatalf("LinkOnline = %v, want true for the derived token", nodes[0].LinkOnline)
	}
	nodes = []models.Node{byon}
	SetNodesLinkOnline(context.Background(), rdb, "another-secret", nodes)
	if nodes[0].LinkOnline == nil || *nodes[0].LinkOnline {
		t.Fatalf("LinkOnline = %v, want false under another secret", nodes[0].LinkOnline)
	}
}
