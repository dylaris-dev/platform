package services

import (
	"encoding/json"
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
