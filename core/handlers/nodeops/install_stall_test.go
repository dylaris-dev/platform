package nodeops

import (
	"context"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func stallRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()}), mr
}

// offline reports every node as disconnected; online as connected.
func offline(int) bool { return false }
func online(int) bool  { return true }

func srv(uuid, status string, nodeID int) models.Server {
	return models.Server{UUID: uuid, Status: status, NodeID: nodeID}
}

// The case F3 describes: the node died mid-install, so nothing is refreshing the
// busy key and nothing is going to until it comes back.
func TestStalledInstallIsReported(t *testing.T) {
	rdb, _ := stallRedis(t)
	servers := []models.Server{srv("a", "installing", 1)}

	AnnotateStalledInstalls(context.Background(), rdb, offline, servers)

	if !servers[0].InstallStalled {
		t.Fatal("an install on an offline node with no busy key was not reported")
	}
	if servers[0].InstallStallReason == "" {
		t.Error("the reason is the whole point; it must not be empty")
	}
}

// Every one of these is a false positive that would have fired on an ordinary
// install, which is worse than the silence it replaces.
func TestStalledInstallStaysQuiet(t *testing.T) {
	ctx := context.Background()

	t.Run("a node that is actively installing", func(t *testing.T) {
		rdb, mr := stallRedis(t)
		// holdBusyStatus keeps this key alive for as long as the node works.
		mr.Set(services.NodeBusyKey("a"), "installing")
		servers := []models.Server{srv("a", "installing", 1)}
		AnnotateStalledInstalls(ctx, rdb, offline, servers)
		if servers[0].InstallStalled {
			t.Error("a live install was reported as stalled")
		}
	})

	// The dispatch window. Between Core enqueueing the job and the node picking
	// it up there is no busy key yet - without the connectivity check this would
	// fire on EVERY install.
	t.Run("a connected node that has not started yet", func(t *testing.T) {
		rdb, _ := stallRedis(t)
		servers := []models.Server{srv("a", "installing", 1)}
		AnnotateStalledInstalls(ctx, rdb, online, servers)
		if servers[0].InstallStalled {
			t.Error("the ordinary dispatch window was reported as a stall")
		}
	})

	t.Run("a server that is not installing", func(t *testing.T) {
		rdb, _ := stallRedis(t)
		servers := []models.Server{srv("a", "offline", 1), srv("b", "online", 1)}
		AnnotateStalledInstalls(ctx, rdb, offline, servers)
		for _, s := range servers {
			if s.InstallStalled {
				t.Errorf("%s (%s) was reported as a stalled install", s.UUID, s.Status)
			}
		}
	})

	// An unreachable Redis says nothing about whether the node is working, so it
	// must not be turned into a claim that it is not.
	t.Run("Redis is down", func(t *testing.T) {
		rdb, mr := stallRedis(t)
		mr.Close()
		servers := []models.Server{srv("a", "installing", 1)}
		AnnotateStalledInstalls(ctx, rdb, offline, servers)
		if servers[0].InstallStalled {
			t.Error("a Redis failure invented a stall")
		}
	})

	t.Run("no connectivity check available", func(t *testing.T) {
		rdb, _ := stallRedis(t)
		servers := []models.Server{srv("a", "installing", 1)}
		AnnotateStalledInstalls(ctx, rdb, nil, servers)
		if servers[0].InstallStalled {
			t.Error("without a way to tell whether the node is connected, nothing may be claimed")
		}
	})
}

// Mixed fleets are the realistic shape: one dead node among several.
func TestStalledInstallPicksOnlyTheAffected(t *testing.T) {
	rdb, mr := stallRedis(t)
	mr.Set(services.NodeBusyKey("busy"), "installing")

	connected := func(nodeID int) bool { return nodeID != 7 }
	servers := []models.Server{
		srv("busy", "installing", 7),    // dead node, but still holding the key
		srv("stalled", "installing", 7), // dead node, nothing holding it
		srv("fine", "installing", 1),    // live node mid-dispatch
		srv("running", "online", 7),     // not installing at all
	}
	AnnotateStalledInstalls(context.Background(), rdb, connected, servers)

	want := map[string]bool{"busy": false, "stalled": true, "fine": false, "running": false}
	for _, s := range servers {
		if s.InstallStalled != want[s.UUID] {
			t.Errorf("%s: stalled = %v, want %v", s.UUID, s.InstallStalled, want[s.UUID])
		}
	}
}

// The idle path: no server is installing, so there is nothing to ask about.
//
// Proven by handing it a Redis that is already CLOSED. Every command against it
// fails, so if the function needed Redis at all it could not come back with a
// clean answer - and a nil client is passed for the same reason, since the
// nil branch marks candidates and would therefore be visible if it were reached.
// This runs on every server-list load, one of the hottest reads in the panel.
func TestStalledInstallIsQuietWhenNothingIsInstalling(t *testing.T) {
	rdb, mr := stallRedis(t)
	mr.Close()
	servers := []models.Server{srv("a", "online", 1), srv("b", "offline", 1)}

	AnnotateStalledInstalls(context.Background(), rdb, offline, servers)
	AnnotateStalledInstalls(context.Background(), nil, offline, servers)

	for _, s := range servers {
		if s.InstallStalled {
			t.Errorf("%s (%s) was marked", s.UUID, s.Status)
		}
	}
}

// A missing Redis with a genuinely stalled server is the opposite call: the node
// is KNOWN to be disconnected, which is the load-bearing half, and a busy key
// that could not be read would only have hidden it.
func TestStalledInstallWithoutRedisStillReports(t *testing.T) {
	servers := []models.Server{srv("a", "installing", 1)}
	AnnotateStalledInstalls(context.Background(), nil, offline, servers)
	if !servers[0].InstallStalled {
		t.Error("an install on a node known to be offline was not reported")
	}
}

// The node writes this key; Core only reads it. A rename on either side alone
// silently disables the detection, so the format is pinned here the way the
// other cross-component keys are.
func TestNodeBusyKeyFormat(t *testing.T) {
	if got := services.NodeBusyKey("abc"); got != "dylaris:server:abc:node_busy" {
		t.Errorf("nodeBusyKey = %q", got)
	}
}
