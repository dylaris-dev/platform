package services

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"dylaris-core/models"
)

// R33-01: a switch_failed report Core drops as superseded takes its switch's
// undo record with it, so the record cannot outlive the switch. A newer
// switch's record stays.
func TestASupersededSwitchReportForgetsItsUndoRecord(t *testing.T) {
	for _, c := range []struct {
		name, undo string
		wantKept   bool
	}{
		{"its own record", `{"target":"old","previous":"main","desiredState":"stopped"}`, false},
		{"the newer switch's record", `{"target":"modded","previous":"old","desiredState":"stopped"}`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs := &switchFailFakeStore{}
			fs.serversByUUID = map[string]models.Server{
				"u-1": {ID: 7, UUID: "u-1", ActiveSubServer: "modded"},
			}
			s := &StatusWatcherService{store: fs, redis: newQueueTestRedis(t)}
			ctx := context.Background()
			s.redis.Set(ctx, SwitchSubFailedKey("u-1"), `{"target":"old","previous":"main","reason":"x"}`, 0)
			s.redis.Set(ctx, SwitchUndoKey("u-1"), c.undo, time.Hour)

			s.consumeSwitchSubFailures(ctx)

			if n, _ := s.redis.Exists(ctx, SwitchUndoKey("u-1")).Result(); (n == 1) != c.wantKept {
				t.Errorf("undo record kept = %v, want %v", n == 1, c.wantKept)
			}
		})
	}
}

// R33-01: a setup or reinstall overtakes a pending switch, so it forgets the
// switch's undo record; the record it writes is stamped with the dispatch time
// the node bounds its hold by (R33-03) and matches a failure report to (R33-05).
func TestASetupForgetsAPendingSwitchAndStampsItsRecord(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx := context.Background()
	srv := &models.Server{ID: 3, UUID: "srv-3", ActiveSubServer: "survival", DesiredState: "stopped"}
	rdb.Set(ctx, SwitchUndoKey("srv-3"), `{"target":"modded","previous":"survival"}`, time.Hour)

	RememberSetupBefore(ctx, rdb, &setupBeforeStore{}, srv, "other")

	if n, _ := rdb.Exists(ctx, SwitchUndoKey("srv-3")).Result(); n != 0 {
		t.Error("the overtaken switch's undo record is still there")
	}
	var b setupBefore
	raw, _ := rdb.Get(ctx, setupBeforeKey("srv-3")).Result()
	if json.Unmarshal([]byte(raw), &b) != nil || b.At <= 0 || time.Since(time.Unix(0, b.At)) > time.Minute {
		t.Errorf("setup_before = %s, want a dispatch time", raw)
	}
}

// ForgetSwitchUndo deletes the record only when it is about the named switch.
func TestForgetSwitchUndo(t *testing.T) {
	for _, c := range []struct {
		target   string
		wantKept bool
	}{{"Modded", false}, {"other", true}} {
		rdb := newQueueTestRedis(t)
		ctx := context.Background()
		rdb.Set(ctx, SwitchUndoKey("u-1"), `{"target":"modded"}`, time.Hour)
		ForgetSwitchUndo(ctx, rdb, "u-1", c.target)
		if n, _ := rdb.Exists(ctx, SwitchUndoKey("u-1")).Result(); (n == 1) != c.wantKept {
			t.Errorf("target %q: kept = %v, want %v", c.target, n == 1, c.wantKept)
		}
	}
}

// R33-05: a durable failure key is applied only to the setup it answers. One
// an older Core already applied over Pub/Sub, found after the owner retried,
// must not roll the retry back or tell the owner twice.
func TestASetupFailureKeyIsAppliedOnlyToItsSetup(t *testing.T) {
	for _, c := range []struct {
		name    string
		attempt func(at int64) string
		wantRun int
	}{
		{"this setup", func(at int64) string { return fmt.Sprintf(`,"attempt":%d`, at) }, 1},
		{"an earlier setup", func(at int64) string { return fmt.Sprintf(`,"attempt":%d`, at-1) }, 0},
		{"no attempt", func(int64) string { return "" }, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			rdb := newQueueTestRedis(t)
			ctx := context.Background()
			srv := &models.Server{ID: 3, UUID: "srv-3", NodeID: 1, ActiveSubServer: "modded"}
			st := &setupBeforeStore{srv: srv}
			RememberSetupBefore(ctx, rdb, st, srv, "modded")
			var b setupBefore
			raw, _ := rdb.Get(ctx, setupBeforeKey("srv-3")).Result()
			if err := json.Unmarshal([]byte(raw), &b); err != nil {
				t.Fatal(err)
			}
			rdb.Set(ctx, SetupFailedKey("srv-3"), `{"serverUuid":"srv-3","subServer":"modded","error":"x"`+c.attempt(b.At)+`}`, time.Minute)

			NewSetupResultService(st, rdb, nil, nil).consumeSetupFailures(ctx)

			if len(st.written) != c.wantRun {
				t.Errorf("setup columns restored %d times, want %d", len(st.written), c.wantRun)
			}
		})
	}
}
