package placement

import (
	"context"
	"errors"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// allocErrStore fails the allocation read. The embedded nil store.Store makes
// any other call panic, so the test also shows nothing past the failure is read.
type allocErrStore struct {
	store.Store
}

func (allocErrStore) SumAllocatedByNode(int) (int64, float64, error) {
	return 0, 0, errors.New("statement timeout")
}
func (allocErrStore) CountServersByNode(int) (int, error) { return 0, nil }

// A node whose allocation could not be read is not an empty node. Reading the
// failure as zero made it pass every capacity check with the best score there
// is, so a full node was picked first.
func TestScoreNodeRefusesANodeWhoseAllocationIsUnreadable(t *testing.T) {
	n := &models.Node{ID: 1, Name: "n1", TotalRAMMB: 4096, RAMOvercommitRatio: 1, TotalCPU: 4, CPUOvercommitRatio: 1}
	cand := ScoreNode(context.Background(), allocErrStore{}, nil, n, PickNodeRequest{RAMMB: 1024}, nil)
	if cand.Available {
		t.Fatalf("node with an unreadable allocation is available (score %v)", cand.Score)
	}
	if cand.Reason == "" {
		t.Error("refused without a reason")
	}
}
