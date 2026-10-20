package services

import (
	"context"
	"os"
	"strings"
	"testing"

	"dylaris-core/models"
)

// A move stops a server first whenever a JVM is up or on its way up. Only
// "online" counted, and a move during a boot archived a live directory and
// then deleted the running source container.
func TestMovesAsRunning(t *testing.T) {
	for status, want := range map[string]bool{
		"online": true, "starting": true, "restarting": true,
		"stopped": false, "offline": false, "disk_full": false, "pending_setup": false,
	} {
		if got := movesAsRunning(status); got != want {
			t.Errorf("movesAsRunning(%q) = %v, want %v", status, got, want)
		}
	}
	// The orchestrator has no end-to-end test; pin that the move decides
	// "running" through this predicate and nothing narrower.
	b, err := os.ReadFile("migration_orchestrator.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "wasRunning := movesAsRunning(preStatus)") {
		t.Error("the move no longer decides wasRunning with movesAsRunning")
	}
}

// Rebalance picks only a server at rest; one between states, or with other
// work on it, is left for a later tick even when it is the biggest.
func TestRebalancePickCandidateSkipsUnsettledServers(t *testing.T) {
	for _, status := range []string{"starting", "restarting", "stopping", "installing", "migrating"} {
		t.Run(status, func(t *testing.T) {
			busy := &models.Server{ID: 1, UUID: "srv-busy", Status: status, AutoMove: true, Memory: 8192}
			calm := &models.Server{ID: 2, UUID: "srv-calm", Status: "online", AutoMove: true, Memory: 1024}
			fs := &rebalanceFakeStore{
				serversByNode: map[int][]models.Server{1: {{ID: 1}, {ID: 2}}},
				serversByID:   map[int]*models.Server{1: busy, 2: calm},
			}
			w := &RebalanceWorker{store: fs, redis: newQueueTestRedis(t)}
			got := w.pickCandidate(context.Background(), &models.Node{ID: 1})
			if got == nil || got.ID != 2 {
				t.Fatalf("picked %+v, want the settled server 2", got)
			}
		})
	}
	// Settled statuses stay eligible.
	for _, status := range []string{"online", "stopped"} {
		srv := &models.Server{ID: 1, UUID: "srv-1", Status: status, AutoMove: true, Memory: 2048}
		fs := &rebalanceFakeStore{
			serversByNode: map[int][]models.Server{1: {{ID: 1}}},
			serversByID:   map[int]*models.Server{1: srv},
		}
		w := &RebalanceWorker{store: fs, redis: newQueueTestRedis(t)}
		if got := w.pickCandidate(context.Background(), &models.Node{ID: 1}); got == nil {
			t.Errorf("%s server not picked", status)
		}
	}
}
