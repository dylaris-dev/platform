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

// R33-01/R34-02: the record a setup or reinstall writes is stamped with the
// dispatch time the node matches a failure report to (R33-05). It leaves a
// pending switch's undo record alone: the handler overtakes the switch only
// once the command is sent (OvertakeSwitchUndo).
func TestASetupStampsItsRecordAndLeavesAPendingSwitch(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx := context.Background()
	srv := &models.Server{ID: 3, UUID: "srv-3", ActiveSubServer: "survival", DesiredState: "stopped"}
	rdb.Set(ctx, SwitchUndoKey("srv-3"), `{"target":"modded","previous":"survival"}`, time.Hour)

	RememberSetupBefore(ctx, rdb, &setupBeforeStore{}, srv, "other")

	if raw, _ := rdb.Get(ctx, SwitchUndoKey("srv-3")).Result(); raw != `{"target":"modded","previous":"survival"}` {
		t.Errorf("the pending switch's undo record = %q, want it untouched", raw)
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

// R34-04: a switch refusal consumed after a setup or reinstall into the same
// sub-server went out is superseded: the install is what the node runs, so
// Core must not go back to the switch's previous sub-server.
func TestARefusalOfAnOvertakenSwitchIsDropped(t *testing.T) {
	for _, c := range []struct {
		name       string
		overtaken  bool
		wantActive int
	}{
		{"overtaken by a setup", true, 0},
		{"still the switch's", false, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs := &switchFailFakeStore{}
			fs.serversByUUID = map[string]models.Server{
				"u-1": {ID: 7, UUID: "u-1", ActiveSubServer: "modded", DesiredState: "online"},
			}
			s := &StatusWatcherService{store: fs, redis: newQueueTestRedis(t)}
			ctx := context.Background()
			s.redis.Set(ctx, SwitchUndoKey("u-1"), `{"target":"modded","previous":"main","desiredState":"stopped"}`, time.Hour)
			if c.overtaken {
				OvertakeSwitchUndo(ctx, s.redis, "u-1")
			}
			s.redis.Set(ctx, SwitchSubFailedKey("u-1"), `{"target":"modded","previous":"main","reason":"x"}`, 0)

			s.consumeSwitchSubFailures(ctx)

			if len(fs.active) != c.wantActive || (c.overtaken && len(fs.desired)+len(fs.notes) != 0) {
				t.Errorf("active=%v desired=%v notes=%d, want %d active writes", fs.active, fs.desired, len(fs.notes), c.wantActive)
			}
			if n, _ := s.redis.Exists(ctx, SwitchUndoKey("u-1")).Result(); n != 0 {
				t.Error("the undo record was left behind")
			}
		})
	}
}
