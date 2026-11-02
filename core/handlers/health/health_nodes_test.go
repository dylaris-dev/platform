package health

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type nodesHealthStore struct {
	store.Store
	nodes []models.Node
}

func (f *nodesHealthStore) ListNodes() ([]models.Node, error) { return f.nodes, nil }

// nodesHealthHandler wires a handler over miniredis and seeds one node error
// stream entry, if msg is non-empty.
func nodesHealthHandler(t *testing.T, nodes []models.Node, token, level, msg string, age time.Duration) *HealthHandler {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	if msg != "" {
		entry, _ := json.Marshal(map[string]string{
			"ts":      time.Now().UTC().Add(-age).Format(time.RFC3339),
			"level":   level,
			"source":  "core-link",
			"message": msg,
		})
		if _, aerr := rdb.XAdd(context.Background(), &redis.XAddArgs{
			Stream: "dylaris:errors:node:" + token,
			Values: map[string]any{"data": string(entry)},
		}).Result(); aerr != nil {
			t.Fatalf("seed stream: %v", aerr)
		}
	}
	return NewHealthHandler(withCoreStorage(Deps{Store: &nodesHealthStore{nodes: nodes}, Redis: rdb}))
}

var onlineNode = []models.Node{{ID: 1, Name: "node-a", Token: "tok-a", Status: "online", Region: "eu-central"}}

// The finding this exists for, measured live: `status` comes from the node's
// Redis heartbeat, and the heartbeat does not travel over gRPC. A node whose
// control channel Core refuses therefore keeps reporting itself online. On the
// live cluster the panel said "up" for a node that could not open a single
// stream to Core - every server on it kept running while console, file transfer
// and RCON all failed with "Node not connected".
func TestNodesComponentFlagsAHeartbeatingNodeWithADeadControlChannel(t *testing.T) {
	h := nodesHealthHandler(t, onlineNode, "tok-a", "ERROR",
		`cannot open the control stream: error reading server preface: EOF [TLS mismatch...]`, time.Minute)

	comp := h.nodesComponent()
	if len(comp.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(comp.Items))
	}
	if comp.Items[0].Status != "degraded" {
		t.Errorf("item status = %q, want degraded - 'up' is the lie this fixes", comp.Items[0].Status)
	}
	if !strings.Contains(comp.Items[0].Detail, "control channel") {
		t.Errorf("item detail = %q, want it to say the control channel is down", comp.Items[0].Detail)
	}
	// It must not be counted as online, or the summary keeps saying everything
	// is fine while a node is unusable.
	if comp.Status == "up" || strings.HasPrefix(comp.Detail, "1/1") {
		t.Errorf("component = %s / %q, want it not to count this node as online", comp.Status, comp.Detail)
	}
}

// No report means nothing to say. A healthy node must stay plainly "up".
func TestNodesComponentLeavesAHealthyNodeAlone(t *testing.T) {
	h := nodesHealthHandler(t, onlineNode, "tok-a", "", "", 0)
	comp := h.nodesComponent()
	if comp.Status != "up" || comp.Items[0].Status != "up" {
		t.Fatalf("component = %s, item = %s; want both up", comp.Status, comp.Items[0].Status)
	}
	if comp.Items[0].Detail != "eu-central" {
		t.Errorf("detail = %q, want the plain region", comp.Items[0].Detail)
	}
}

// A recovery line is the node saying it reconnected. Treating the newest entry
// as a failure regardless of level would keep a solved problem on screen.
func TestNodesComponentIgnoresARecoveryEntry(t *testing.T) {
	h := nodesHealthHandler(t, onlineNode, "tok-a", "INFO", "control channel to Core core-1 is back", time.Minute)
	if comp := h.nodesComponent(); comp.Items[0].Status != "up" {
		t.Errorf("item status = %q, want up - the newest entry says it recovered", comp.Items[0].Status)
	}
}

// A stream entry outlives the condition it describes, and a node that recovers
// is online and has no row left to annotate. Without the age bound one solved
// outage would be reported next to that node forever.
func TestNodesComponentDropsAStaleReport(t *testing.T) {
	h := nodesHealthHandler(t, onlineNode, "tok-a", "ERROR", "something old", nodeSelfReportedFailureMaxAge+time.Minute)
	if comp := h.nodesComponent(); comp.Items[0].Status != "up" {
		t.Errorf("item status = %q, want up - the report is older than the bound", comp.Items[0].Status)
	}
}

// An offline node still gets the reason appended, which is the original point:
// "offline, last seen ..." says nothing about why.
func TestNodesComponentExplainsAnOfflineNode(t *testing.T) {
	seen := time.Now().UTC().Add(-time.Hour)
	nodes := []models.Node{{ID: 1, Name: "node-a", Token: "tok-a", Status: "offline", LastSeenAt: &seen}}
	h := nodesHealthHandler(t, nodes, "tok-a", "ERROR", "certificate fingerprint mismatch", time.Minute)

	comp := h.nodesComponent()
	if comp.Items[0].Status != "down" {
		t.Fatalf("item status = %q, want down", comp.Items[0].Status)
	}
	if !strings.Contains(comp.Items[0].Detail, "certificate fingerprint mismatch") {
		t.Errorf("detail = %q, want the node's own reason appended", comp.Items[0].Detail)
	}
}
