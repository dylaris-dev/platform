package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dylaris-pkg/queue"
)

// R33-01: a pending switch holds the reconciler off only while the switch has
// neither landed nor been overtaken (the node is still on its previous
// sub-server) and only for a short while after Core wrote it. A record that
// outlived its switch held a different, running sub-server down for a day.
func TestAPendingSwitchHoldsOnlyWhileItIsPending(t *testing.T) {
	const uuid = "33333333-0101-0101-0101-333333333333"
	fresh := time.Now().Add(-time.Minute).UnixNano()
	for _, c := range []struct {
		name, active, record string
		want                 bool
	}{
		{"pending", "main", fmt.Sprintf(`{"target":"modded","previous":"main","desiredState":"stopped","at":%d}`, fresh), true},
		{"pending, none active before", "", fmt.Sprintf(`{"target":"modded","previous":"","desiredState":"stopped","at":%d}`, fresh), true},
		{"landed", "modded", fmt.Sprintf(`{"target":"modded","previous":"main","desiredState":"stopped","at":%d}`, fresh), false},
		{"overtaken by a setup", "other", fmt.Sprintf(`{"target":"modded","previous":"main","desiredState":"stopped","at":%d}`, fresh), false},
		{"too old", "main", fmt.Sprintf(`{"target":"modded","previous":"main","desiredState":"stopped","at":%d}`, time.Now().Add(-time.Hour).UnixNano()), false},
		{"no dispatch time", "main", `{"target":"modded","previous":"main","desiredState":"stopped"}`, false},
		{"meant to run", "main", fmt.Sprintf(`{"target":"modded","previous":"main","desiredState":"online","at":%d}`, fresh), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, uuid), 0o755); err != nil {
				t.Fatal(err)
			}
			if c.active != "" {
				if err := os.WriteFile(filepath.Join(root, uuid, ".active_server"), []byte(c.active), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			rdb := newMiniRedis(t)
			ctx := context.Background()
			rdb.Set(ctx, switchUndoKey(uuid), c.record, time.Hour)
			if got := switchPendingOverStop(ctx, rdb, &StorageManager{paths: []string{root}}, uuid); got != c.want {
				t.Errorf("held = %v, want %v", got, c.want)
			}
		})
	}
}

// R33-02/R33-04: a landed switch deletes only its own undo record; a newer
// switch queued behind it keeps the record its refusal is put back from.
func TestALandedSwitchDeletesOnlyItsOwnUndoRecord(t *testing.T) {
	const uuid = "33333333-0202-0202-0202-333333333333"
	for _, c := range []struct {
		name, record, landed string
		wantKept             bool
	}{
		{"its own", `{"target":"modded","previous":"main"}`, "modded", false},
		{"its own, other case", `{"target":"Modded","previous":"main"}`, "modded", false},
		{"a newer switch's", `{"target":"other","previous":"modded"}`, "modded", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			rdb := newMiniRedis(t)
			ctx := context.Background()
			rdb.Set(ctx, switchUndoKey(uuid), c.record, time.Hour)
			forgetRecordFor(ctx, rdb, switchUndoKey(uuid), c.landed)
			if n, _ := rdb.Exists(ctx, switchUndoKey(uuid)).Result(); (n == 1) != c.wantKept {
				t.Errorf("record kept = %v, want %v", n == 1, c.wantKept)
			}
		})
	}
}

// R33-03: a setup or reinstall Core dispatched for a server that was not meant
// to run holds the reconciler off until it lands or Core puts it back, for a
// short while at most.
func TestAPendingSetupHoldsAStoppedServerDown(t *testing.T) {
	const uuid = "33333333-0303-0303-0303-333333333333"
	fresh := time.Now().Add(-time.Minute).UnixNano()
	for _, c := range []struct {
		name, record string
		want         bool
	}{
		{"pending", fmt.Sprintf(`{"Target":"modded","DesiredState":"stopped","At":%d}`, fresh), true},
		{"too old", fmt.Sprintf(`{"Target":"modded","DesiredState":"stopped","At":%d}`, time.Now().Add(-time.Hour).UnixNano()), false},
		{"from an older Core", `{"Target":"modded","DesiredState":"stopped"}`, false},
		{"meant to run", fmt.Sprintf(`{"Target":"modded","DesiredState":"online","At":%d}`, fresh), false},
		{"none", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rdb := newMiniRedis(t)
			ctx := context.Background()
			if c.record != "" {
				rdb.Set(ctx, setupBeforeKey(uuid), c.record, time.Hour)
			}
			if got := setupPendingOverStop(ctx, rdb, uuid); got != c.want {
				t.Errorf("held = %v, want %v", got, c.want)
			}
		})
	}
}

// R33-05: the durable failure key names the setup it answers (the dispatch
// time Core wrote into setup_before), so Core applies it only to that setup.
// A setup Core did not stamp - an older Core, which applies the Pub/Sub copy
// and never deletes the key - gets no key at all.
func TestASetupFailureKeyNamesTheSetupItAnswers(t *testing.T) {
	const uuid = "33333333-0505-0505-0505-333333333333"
	for _, c := range []struct {
		name, record string
		wantAttempt  int64 // 0 = no key, Pub/Sub copy not durable
	}{
		{"stamped by Core", `{"Target":"modded","At":12345}`, 12345},
		{"older Core", `{"Target":"modded"}`, 0},
		{"another setup's", `{"Target":"other","At":12345}`, 0},
		{"no record", "", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			rdb := newMiniRedis(t)
			ctx := context.Background()
			if c.record != "" {
				rdb.Set(ctx, setupBeforeKey(uuid), c.record, time.Hour)
			}
			sub := rdb.Subscribe(ctx, queue.SetupResultsChannel(nodeID))
			defer sub.Close()
			if _, err := sub.Receive(ctx); err != nil {
				t.Fatal(err)
			}
			ch := sub.Channel()

			publishSetupFailure(ctx, rdb, uuid, "modded", errors.New("x"), false)

			raw, err := rdb.Get(ctx, "dylaris:server:"+uuid+":setup_failed").Result()
			if c.wantAttempt == 0 {
				if err == nil {
					t.Errorf("key written: %s", raw)
				}
			} else {
				var rep struct {
					Attempt int64 `json:"attempt"`
				}
				if err != nil || json.Unmarshal([]byte(raw), &rep) != nil || rep.Attempt != c.wantAttempt {
					t.Errorf("key = %q (%v), want attempt %d", raw, err, c.wantAttempt)
				}
			}
			var pub map[string]interface{}
			if err := json.Unmarshal([]byte((<-ch).Payload), &pub); err != nil {
				t.Fatal(err)
			}
			if (pub["durable"] == true) != (c.wantAttempt != 0) {
				t.Errorf("published = %v, want durable = %v", pub, c.wantAttempt != 0)
			}
		})
	}
}

// R33-03: a setup that landed deletes its record, so the hold ends with it.
func TestALandedSetupEndsItsHold(t *testing.T) {
	const uuid = "33333333-0606-0606-0606-333333333333"
	rdb := newMiniRedis(t)
	ctx := context.Background()
	rdb.Set(ctx, setupBeforeKey(uuid), fmt.Sprintf(`{"Target":"modded","DesiredState":"stopped","At":%d}`, time.Now().UnixNano()), time.Hour)
	forgetRecordFor(ctx, rdb, setupBeforeKey(uuid), "other")
	if !setupPendingOverStop(ctx, rdb, uuid) {
		t.Fatal("another setup's landing ended the hold")
	}
	forgetRecordFor(ctx, rdb, setupBeforeKey(uuid), "modded")
	if setupPendingOverStop(ctx, rdb, uuid) {
		t.Error("still held after the setup landed")
	}
}

// R33-03: setup and reinstall forget their record once the software is in
// place. Source check, like TestALandedSwitchRemovesTheUndoRecord: the
// branches need a real Docker to succeed.
func TestALandedSetupOrReinstallForgetsItsRecord(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{{`case "setup":`, `case "reconfigure":`}, {`case "reinstall":`, `case "migrate_storage":`}} {
		if !strings.Contains(between(t, string(b), c[0], c[1]), "forgetRecordFor(ctx, rdb, setupBeforeKey(") {
			t.Errorf("%s leaves Core's setup_before record holding the server down", c[0])
		}
	}
}
