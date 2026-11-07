package services

import (
	"context"
	"dylaris-core/models"
	"dylaris-pkg/queue"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

	// G50: the node stops writing when the container goes, so a stopped
	// server's last sample keeps the players it had then. Aged like
	// PlayersOnlineByServer ages it, or the rebalancer never moves it.
	t.Run("stale entry -> 0", func(t *testing.T) {
		rdb := newQueueTestRedis(t)
		old := time.Now().Add(-2 * playersFreshFor).UnixMilli()
		if err := rdb.XAdd(context.Background(), &redis.XAddArgs{
			Stream: "dylaris:server:srv-3:stats:buffer",
			ID:     fmt.Sprintf("%d-0", old),
			Values: map[string]interface{}{"data": `{"players": 4}`},
		}).Err(); err != nil {
			t.Fatal(err)
		}
		if got := playerCountFromStats(context.Background(), rdb, "srv-3"); got != 0 {
			t.Errorf("playerCountFromStats = %d, want 0 for a sample older than %v", got, playersFreshFor)
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
	// EnqueueMigration reads the server now: the request carries the node it
	// was decided against (see TestASecondRequestForTheSameServerIsRefused).
	o, _, _, rdb := newFlow(t, "stopped")

	if err := o.EnqueueMigration(context.Background(), 5, 2, "rebalance", "system"); err != nil {
		t.Fatalf("EnqueueMigration: %v", err)
	}

	payload := readStreamPayload(t, rdb, migrationStreamKey)
	if int(payload["serverID"].(float64)) != 5 {
		t.Errorf("serverID = %v, want 5", payload["serverID"])
	}
	if int(payload["targetNodeID"].(float64)) != 2 {
		t.Errorf("targetNodeID = %v, want 2", payload["targetNodeID"])
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

// J44: the disk grace lives under the source node's own key, which the target
// cannot read, so Core hands it over. Only from a node whose word Core takes:
// a customer's machine writes its own keys, and a grace it made up would buy
// disk on someone else's node.
func TestMovedDiskGrace(t *testing.T) {
	ctx := context.Background()
	rdb := newQueueTestRedis(t)
	const uuid = "srv-1"
	a, b := "tenant-a", "tenant-b"
	node := func(token string, owner *string) *models.Node { return &models.Node{Token: token, OwnerID: owner} }
	for _, token := range []string{"plat", "own-a"} {
		if err := rdb.Set(ctx, "node:"+token+":server:"+uuid+":disk_grace", "100 300", 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name           string
		source, target *models.Node
		want           string
	}{
		{"platform source", node("plat", nil), node("t", nil), "100 300"},
		{"platform to a customer's machine", node("plat", nil), node("t", &b), "100 300"},
		{"a customer's machine to platform", node("own-a", &a), node("t", nil), ""},
		{"to another customer's machine", node("own-a", &a), node("t", &b), ""},
		{"between one customer's machines", node("own-a", &a), node("t", &a), "100 300"},
		{"no grace on the source", node("none", nil), node("t", nil), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := movedDiskGrace(ctx, rdb, tt.source, tt.target, uuid); got != tt.want {
				t.Errorf("grace = %q, want %q", got, tt.want)
			}
		})
	}
}

// The pull token is checked on every pull attempt, and Core waits up to
// migrationTransferTimeout for the transfer. A token that dies first turns a
// migrate_in that waited behind long jobs, or a retry after a failed first
// attempt, into a 401 and a rolled-back move while Core still waits.
func TestMigrationTokenOutlivesTheTransferWait(t *testing.T) {
	if migrationTokenTTL < migrationTransferTimeout {
		t.Fatalf("migrationTokenTTL %v < migrationTransferTimeout %v", migrationTokenTTL, migrationTransferTimeout)
	}
}

// G52: an archive over the single-PUT limit could not go through transfer
// storage at all. It now gets a multipart upload with one signed URL per part;
// one that fits keeps the single PUT an older node understands.
func TestPresignMigrationParts(t *testing.T) {
	tests := []struct {
		name      string
		size      int64
		wantParts int
	}{
		{"fits one PUT", migrationPutMaxSize, 0},
		{"one byte over", migrationPutMaxSize + 1, int(migrationPutMaxSize/backupPartSize) + 1},
		{"32 GB world", 32 << 30, int((32 << 30) / backupPartSize)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prov := &fakeMultipartStorage{}
			uploadID, urls, err := presignMigrationParts(context.Background(), prov, "migration-transfer/k.zip", tc.size, time.Hour)
			if err != nil {
				t.Fatalf("presignMigrationParts: %v", err)
			}
			if len(urls) != tc.wantParts {
				t.Fatalf("got %d part URLs, want %d", len(urls), tc.wantParts)
			}
			if tc.wantParts == 0 {
				if uploadID != "" || prov.creates != 0 {
					t.Fatalf("an archive that fits one PUT started a multipart upload (%q)", uploadID)
				}
				return
			}
			if uploadID == "" || prov.creates != 1 {
				t.Fatalf("upload id %q after %d creates, want one upload", uploadID, prov.creates)
			}
			for i, u := range urls {
				if want := fmt.Sprintf("partNumber=%d&", i+1); !strings.Contains(u, want) || !strings.Contains(u, "uploadId="+uploadID) {
					t.Fatalf("URL %d = %q, want part %d of %s", i, u, i+1, uploadID)
				}
			}
		})
	}
}

// G54: the stage wait was a fixed 5 minutes from the moment Core queued
// migrate_out, so archiving a large server failed every move. A node that
// reports its archive growing now keeps the wait open.
func TestWaitForNodePhaseProgressExtendsDeadline(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx := context.Background()
	o := &MigrationOrchestrator{redis: rdb}
	key := queue.MigrationStatusKey("node-a", "srv-p")
	set := func(v string) {
		if err := rdb.Set(ctx, key, v, 0).Err(); err != nil {
			t.Error(err)
		}
	}
	set(`{"phase":"staging","bytes":1}`)
	go func() {
		time.Sleep(time.Second)
		set(`{"phase":"staging","bytes":2}`)
		time.Sleep(2 * time.Second)
		set(`{"phase":"staged"}`)
	}()

	// 100ms is long past by the second poll (2s), so only the growth seen
	// there can keep the wait open until "staged".
	phase, errMsg := o.waitForNodePhase(ctx, "node-a", "srv-p", "staged", 100*time.Millisecond)
	if phase != "staged" || errMsg != "" {
		t.Fatalf("waitForNodePhase = (%q, %q), want (staged, \"\")", phase, errMsg)
	}
}

// H57/H60: the R2 legs and the LAN pull waited a fixed time, and only the
// stage wait grew with progress, so every transfer longer than the wait failed.
// The target's pull reports its bytes under "transferring", and the wait for
// "transferred" or "need_remote" grows with them.
func TestWaitForNodePhaseAnyProgressExtendsDeadline(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx := context.Background()
	o := &MigrationOrchestrator{redis: rdb}
	key := queue.MigrationStatusKey("node-b", "srv-t")
	set := func(v string) {
		if err := rdb.Set(ctx, key, v, 0).Err(); err != nil {
			t.Error(err)
		}
	}
	set(`{"phase":"transferring","bytes":1}`)
	go func() {
		time.Sleep(time.Second)
		set(`{"phase":"transferring","bytes":2}`)
		time.Sleep(2 * time.Second)
		set(`{"phase":"transferred"}`)
	}()

	phase, errMsg := o.waitForNodePhaseAny(ctx, "node-b", "srv-t", map[string]bool{"transferred": true, "need_remote": true}, 100*time.Millisecond)
	if phase != "transferred" || errMsg != "" {
		t.Fatalf("waitForNodePhaseAny = (%q, %q), want (transferred, \"\")", phase, errMsg)
	}
}

// J55: a node restarted mid-leg reruns the redelivered command from byte 0,
// and the wait extended only past the killed run's high-water mark - so the
// rerun timed out before it could catch up. A count that changes is progress.
func TestWaitForNodePhaseAnyARestartedCountIsProgress(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx := context.Background()
	o := &MigrationOrchestrator{redis: rdb}
	key := queue.MigrationStatusKey("node-b", "srv-r")
	set := func(v string) {
		if err := rdb.Set(ctx, key, v, 0).Err(); err != nil {
			t.Error(err)
		}
	}
	set(`{"phase":"transferring","bytes":5}`)
	go func() {
		time.Sleep(time.Second)
		set(`{"phase":"transferring","bytes":1}`)
		time.Sleep(2 * time.Second)
		set(`{"phase":"transferred"}`)
	}()

	phase, errMsg := o.waitForNodePhaseAny(ctx, "node-b", "srv-r", map[string]bool{"transferred": true}, 100*time.Millisecond)
	if phase != "transferred" || errMsg != "" {
		t.Fatalf("waitForNodePhaseAny = (%q, %q), want (transferred, \"\")", phase, errMsg)
	}
}

// H60/H62: the waits now grow with progress, so everything sized against the
// fixed waits must outlast the longest a move can now take.
func TestMigrationLifetimesOutlastTheLongestWait(t *testing.T) {
	// migrate_in, or need_remote then two R2 legs, after staging and the stop.
	longest := migrationStopTimeout + migrationProgressMaxWait + migrationTransferTimeout + 2*migrationProgressMaxWait + migrationStartTimeout
	if migrationQueuedTTL <= longest {
		t.Errorf("migrationQueuedTTL = %s, the longest move runs %s: a second request is accepted under a live one", migrationQueuedTTL, longest)
	}
	// The pull token is checked on every attempt of the LAN pull.
	if migrationTokenTTL < migrationProgressMaxWait {
		t.Errorf("migrationTokenTTL = %s, the LAN pull may run %s: its retries are refused", migrationTokenTTL, migrationProgressMaxWait)
	}
}

// Q20: the R2 transfer's error went into the move status the server's owner
// and sub-users read, and a storage error carries the operator's endpoint,
// bucket, key and request ids. It stays in the Core log; the owner gets a
// fixed line, unless the reason is one meant for them.
func TestMigrationTransferReasonKeepsStorageDetailInTheLog(t *testing.T) {
	_, _, tooBig := presignMigrationParts(context.Background(), &fakeMultipartStorage{}, "k", int64(maxPartNumber+1)*backupPartSize, time.Hour)
	s3err := errors.New("s3 CompleteMultipartUpload acct123.r2.cloudflarestorage.com/bucket/migration-transfer/x.zip: AccessDenied, RequestID: abc")
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"raw storage error", fmt.Errorf("complete transfer upload: %w", s3err), "r2 transfer failed: Core could not move the archive through transfer storage; the Core log has the reason"},
		{"archive too large", tooBig, "r2 transfer failed: " + tooBig.Error()},
		{"node's own reason", refused("source R2 upload failed (phase=error): disk full"), "r2 transfer failed: source R2 upload failed (phase=error): disk full"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := migrationTransferReason(tc.err); got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}
