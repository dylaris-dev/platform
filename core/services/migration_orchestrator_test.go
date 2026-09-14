package services

import (
	"context"
	"dylaris-core/models"
	"dylaris-pkg/queue"
	"encoding/json"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// seedStatsEntry writes one entry onto a server's stats-buffer stream with
// the given raw "data" payload, matching what playerCountFromStats reads.
func seedStatsEntry(t *testing.T, rdb *redis.Client, uuid, dataRaw string) {
	t.Helper()
	stream := "dylaris:server:" + uuid + ":stats:buffer"
	if _, err := rdb.XAdd(context.Background(), &redis.XAddArgs{
		Stream: stream,
		Values: map[string]interface{}{"data": dataRaw},
	}).Result(); err != nil {
		t.Fatalf("seed stats entry on %s: %v", stream, err)
	}
}

// seedNodePhase writes the node-owned dylaris:migration:<uuid>:status key
// that waitForNodePhase/waitForNodePhaseAny poll.
func seedNodePhase(t *testing.T, rdb *redis.Client, nodeToken, uuid, phase, errMsg string) {
	t.Helper()
	st := struct {
		Phase string `json:"phase"`
		Error string `json:"error,omitempty"`
	}{phase, errMsg}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal node phase: %v", err)
	}
	key := queue.MigrationStatusKey(nodeToken, uuid)
	if err := rdb.Set(context.Background(), key, data, 0).Err(); err != nil {
		t.Fatalf("seed node phase %s: %v", key, err)
	}
}

// --- playerCountFromStats (free func) ---

func TestPlayerCountFromStats(t *testing.T) {
	t.Run("parses players from the latest stream entry", func(t *testing.T) {
		rdb := newQueueTestRedis(t)
		seedStatsEntry(t, rdb, "srv-1", `{"players": 3, "other": "ignored"}`)
		if got := playerCountFromStats(context.Background(), rdb, "srv-1"); got != 3 {
			t.Errorf("playerCountFromStats = %d, want 3", got)
		}
	})

	// No entries at all: XRevRangeN on a stream key that was never created
	// returns an empty (not erroring) result in real Redis and in miniredis,
	// so this exercises the "len(msgs) == 0" branch, which the source treats
	// the same as an error (return 0).
	t.Run("empty stream -> 0", func(t *testing.T) {
		rdb := newQueueTestRedis(t)
		if got := playerCountFromStats(context.Background(), rdb, "srv-missing"); got != 0 {
			t.Errorf("playerCountFromStats = %d, want 0", got)
		}
	})

	t.Run("malformed JSON -> 0", func(t *testing.T) {
		rdb := newQueueTestRedis(t)
		seedStatsEntry(t, rdb, "srv-2", `not-json`)
		if got := playerCountFromStats(context.Background(), rdb, "srv-2"); got != 0 {
			t.Errorf("playerCountFromStats = %d, want 0 for malformed JSON", got)
		}
	})
}

// --- readMeta() ---

func TestMigrationOrchestrator_ReadMeta(t *testing.T) {
	t.Run("valid meta parsed", func(t *testing.T) {
		rdb := newQueueTestRedis(t)
		ctx := context.Background()
		o := &MigrationOrchestrator{redis: rdb}

		want := nodeMeta{SHA256: "abc123", Size: 100, SourceNodeID: "5", StagedAt: 111}
		data, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("marshal meta: %v", err)
		}
		if err := rdb.Set(ctx, queue.MigrationMetaKey("node-a", "srv-1"), data, 0).Err(); err != nil {
			t.Fatalf("seed meta: %v", err)
		}

		got, err := o.readMeta(ctx, "node-a", "srv-1")
		if err != nil {
			t.Fatalf("readMeta: %v", err)
		}
		if got != want {
			t.Errorf("readMeta = %+v, want %+v", got, want)
		}
	})

	t.Run("missing key -> error", func(t *testing.T) {
		rdb := newQueueTestRedis(t)
		o := &MigrationOrchestrator{redis: rdb}
		if _, err := o.readMeta(context.Background(), "node-a", "srv-missing"); err == nil {
			t.Fatal("expected error for a missing meta key")
		}
	})

	t.Run("empty sha256 -> validation error", func(t *testing.T) {
		rdb := newQueueTestRedis(t)
		ctx := context.Background()
		o := &MigrationOrchestrator{redis: rdb}

		data, err := json.Marshal(nodeMeta{SHA256: "", Size: 1})
		if err != nil {
			t.Fatalf("marshal meta: %v", err)
		}
		if err := rdb.Set(ctx, queue.MigrationMetaKey("node-a", "srv-2"), data, 0).Err(); err != nil {
			t.Fatalf("seed meta: %v", err)
		}

		if _, err := o.readMeta(ctx, "node-a", "srv-2"); err == nil {
			t.Fatal("expected error for empty sha256 (validation)")
		}
	})
}

// --- writeStatus() ---

func TestMigrationOrchestrator_WriteStatus(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx := context.Background()
	o := &MigrationOrchestrator{redis: rdb}

	startedAt := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	o.writeStatus(ctx, "srv-1", "migrating", "", 1, 2, "rebalance", startedAt)

	raw, err := rdb.Get(ctx, "dylaris:migration:srv-1:orchestration").Result()
	if err != nil {
		t.Fatalf("Get orchestration key: %v", err)
	}
	var got orchestrationStatus
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal orchestrationStatus: %v", err)
	}
	if got.Phase != "migrating" || got.Error != "" || got.SourceNodeID != 1 || got.TargetNodeID != 2 ||
		got.Reason != "rebalance" || got.StartedAt != startedAt.Unix() {
		t.Errorf("orchestrationStatus = %+v, unexpected fields", got)
	}
	if got.UpdatedAt == 0 {
		t.Error("expected UpdatedAt to be set to a non-zero unix timestamp")
	}
}

// --- waitForNodePhase() / waitForNodePhaseAny() ---

func TestMigrationOrchestrator_WaitForNodePhase(t *testing.T) {
	t.Run("phase already matches -> returns immediately", func(t *testing.T) {
		rdb := newQueueTestRedis(t)
		ctx := context.Background()
		o := &MigrationOrchestrator{redis: rdb}
		seedNodePhase(t, rdb, "node-a", "srv-1", "staged", "")

		phase, errMsg := o.waitForNodePhase(ctx, "node-a", "srv-1", "staged", 5*time.Second)
		if phase != "staged" || errMsg != "" {
			t.Errorf("waitForNodePhase = (%q, %q), want (staged, \"\")", phase, errMsg)
		}
	})

	t.Run("node reports error phase", func(t *testing.T) {
		rdb := newQueueTestRedis(t)
		ctx := context.Background()
		o := &MigrationOrchestrator{redis: rdb}
		seedNodePhase(t, rdb, "node-a", "srv-2", "error", "disk full")

		phase, errMsg := o.waitForNodePhase(ctx, "node-a", "srv-2", "staged", 5*time.Second)
		if phase != "error" || errMsg != "disk full" {
			t.Errorf("waitForNodePhase = (%q, %q), want (error, disk full)", phase, errMsg)
		}
	})

	// migrationPollInterval (2s) is a hardcoded constant with no clock
	// injection, so even a short requested timeout cannot return before the
	// first poll tick fires. This case genuinely blocks ~2s; see the report
	// for this noted as a production smell (not fixed - out of scope for a
	// test-only change).
	t.Run("timeout elapses -> timed out result", func(t *testing.T) {
		rdb := newQueueTestRedis(t)
		ctx := context.Background()
		o := &MigrationOrchestrator{redis: rdb}

		phase, errMsg := o.waitForNodePhase(ctx, "node-a", "srv-missing", "staged", 50*time.Millisecond)
		if phase != "" || errMsg != "timed out" {
			t.Errorf("waitForNodePhase = (%q, %q), want (\"\", timed out)", phase, errMsg)
		}
	})
}

func TestMigrationOrchestrator_WaitForNodePhaseAny(t *testing.T) {
	t.Run("returns on first accepted phase", func(t *testing.T) {
		rdb := newQueueTestRedis(t)
		ctx := context.Background()
		o := &MigrationOrchestrator{redis: rdb}
		seedNodePhase(t, rdb, "node-a", "srv-1", "need_remote", "")

		phase, errMsg := o.waitForNodePhaseAny(ctx, "node-a", "srv-1", map[string]bool{"transferred": true, "need_remote": true}, 5*time.Second)
		if phase != "need_remote" || errMsg != "" {
			t.Errorf("waitForNodePhaseAny = (%q, %q), want (need_remote, \"\")", phase, errMsg)
		}
	})

	t.Run("timeout elapses -> timed out result", func(t *testing.T) {
		rdb := newQueueTestRedis(t)
		ctx := context.Background()
		o := &MigrationOrchestrator{redis: rdb}

		phase, errMsg := o.waitForNodePhaseAny(ctx, "node-a", "srv-missing", map[string]bool{"transferred": true}, 50*time.Millisecond)
		if phase != "" || errMsg != "timed out" {
			t.Errorf("waitForNodePhaseAny = (%q, %q), want (\"\", timed out)", phase, errMsg)
		}
	})
}

// --- EnqueueMigration() ---

func TestMigrationOrchestrator_EnqueueMigration(t *testing.T) {
	rdb := newQueueTestRedis(t)
	// EnqueueMigration only touches o.redis (verified in source: it marshals
	// the request and calls queue.Publish(ctx, o.redis, migrationStreamKey,
	// data) - it never reads o.store/o.queue/o.gateway). No fake store or
	// gateway is needed.
	o := &MigrationOrchestrator{redis: rdb}

	if err := o.EnqueueMigration(context.Background(), 42, 7, "rebalance", "system"); err != nil {
		t.Fatalf("EnqueueMigration: %v", err)
	}

	payload := readStreamPayload(t, rdb, migrationStreamKey)
	if int(payload["serverID"].(float64)) != 42 {
		t.Errorf("serverID = %v, want 42", payload["serverID"])
	}
	if int(payload["targetNodeID"].(float64)) != 7 {
		t.Errorf("targetNodeID = %v, want 7", payload["targetNodeID"])
	}
	if payload["reason"] != "rebalance" {
		t.Errorf("reason = %v, want rebalance", payload["reason"])
	}
	if payload["requestedBy"] != "system" {
		t.Errorf("requestedBy = %v, want system", payload["requestedBy"])
	}
}

// A move with either end outside the datacenter needs the source's LAN
// addresses: that is what makes the target answer need_remote and take the
// object-storage transfer instead of an overlay pull it cannot reach. An
// External node is unowned, so the owner check alone handed it nothing.
func TestMigrationSourceLANIPs(t *testing.T) {
	lan := []string{"192.168.1.10"}
	owner := "tenant-a"
	platform := func() *models.Node { return &models.Node{PrivateIPs: lan} }
	external := func() *models.Node { return &models.Node{PrivateIPs: lan, Tags: "ssd, external"} }
	owned := func() *models.Node { return &models.Node{PrivateIPs: lan, OwnerID: &owner} }
	tests := []struct {
		name           string
		source, target *models.Node
		want           bool
	}{
		{"platform to platform stays on the overlay", platform(), platform(), false},
		{"platform to External", platform(), external(), true},
		{"External to platform", external(), platform(), true},
		{"External to External", external(), external(), true},
		{"platform to a customer's machine", platform(), owned(), true},
		{"a customer's machine to platform", owned(), platform(), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := migrationSourceLANIPs(tt.source, tt.target)
			if (got != nil) != tt.want {
				t.Errorf("LAN IPs = %v, want handed over: %v", got, tt.want)
			}
		})
	}
}
