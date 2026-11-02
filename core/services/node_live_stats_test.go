package services

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"dylaris-core/models"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestLinkCountIsUnknownWithoutAHeartbeat pins the contract the panel's "no
// Link" warning stands on: a reported 0 reaches it as 0, and a node that said
// nothing reaches it with no linkCount at all. As a plain int both were 0, and
// every offline node would have been warned about.
func TestLinkCountIsUnknownWithoutAHeartbeat(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	mr.Set("dylaris:discovery:n-zero", `{"cpuUsage":1,"linkCount":0}`)
	mr.Set("dylaris:discovery:n-silent", `{"cpuUsage":1}`)

	nodes := []models.Node{{Token: "n-zero"}, {Token: "n-silent"}, {Token: "n-offline"}}
	EnrichNodesWithLiveStats(t.Context(), nil, rdb, nodes)

	if nodes[0].LinkCount == nil || *nodes[0].LinkCount != 0 {
		t.Errorf("reported 0: LinkCount = %v, want a pointer to 0", nodes[0].LinkCount)
	}
	raw, _ := json.Marshal(nodes[0])
	if !strings.Contains(string(raw), `"linkCount":0`) {
		t.Errorf("reported 0 is not serialised as 0: %s", raw)
	}

	for _, n := range nodes[1:] {
		if n.LinkCount != nil {
			t.Errorf("%s: LinkCount = %d, want nil (unknown)", n.Token, *n.LinkCount)
		}
		raw, _ := json.Marshal(n)
		if strings.Contains(string(raw), `"linkCount"`) {
			t.Errorf("%s: unknown count is serialised: %s", n.Token, raw)
		}
	}
}

// A node whose Docker cannot enforce memory limits says so in its heartbeat,
// and the panel gets it; a node that says nothing (older, or fine) does not.
func TestNoMemoryLimitReachesThePanel(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	mr.Set("dylaris:discovery:n-pi", `{"cpuUsage":1,"noMemoryLimit":true}`)
	mr.Set("dylaris:discovery:n-old", `{"cpuUsage":1}`)

	nodes := []models.Node{{Token: "n-pi"}, {Token: "n-old"}}
	EnrichNodesWithLiveStats(t.Context(), nil, rdb, nodes)

	if raw, _ := json.Marshal(nodes[0]); !strings.Contains(string(raw), `"noMemoryLimit":true`) {
		t.Errorf("the flag did not reach the node: %s", raw)
	}
	if raw, _ := json.Marshal(nodes[1]); strings.Contains(string(raw), "noMemoryLimit") {
		t.Errorf("a silent node is flagged: %s", raw)
	}
}

type perNodeCounter struct {
	counts map[int]int
	err    error
	calls  int
}

func (c *perNodeCounter) CountServersPerNode() (map[int]int, error) {
	c.calls++
	return c.counts, c.err
}

// One count statement for the whole fleet; a node with no row reads 0, and a
// failed read leaves the counts untouched as a failed per-node read did.
func TestServerCountsInOneRead(t *testing.T) {
	nodes := []models.Node{{ID: 1, ServerCount: 9}, {ID: 2, ServerCount: 9}, {ID: 3, ServerCount: 9}}
	c := &perNodeCounter{counts: map[int]int{1: 4, 3: 1}}
	EnrichNodesWithLiveStats(t.Context(), c, nil, nodes)
	if c.calls != 1 || nodes[0].ServerCount != 4 || nodes[1].ServerCount != 0 || nodes[2].ServerCount != 1 {
		t.Fatalf("calls %d counts %d/%d/%d", c.calls, nodes[0].ServerCount, nodes[1].ServerCount, nodes[2].ServerCount)
	}
	failed := &perNodeCounter{err: errors.New("db down")}
	EnrichNodesWithLiveStats(t.Context(), failed, nil, nodes)
	if nodes[0].ServerCount != 4 {
		t.Fatalf("a failed read changed the count to %d", nodes[0].ServerCount)
	}
}
