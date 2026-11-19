package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"dylaris-pkg/queue"
)

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

// R33-03: a setup that landed deletes only its own record.
func TestALandedSetupForgetsOnlyItsOwnRecord(t *testing.T) {
	const uuid = "33333333-0606-0606-0606-333333333333"
	rdb := newMiniRedis(t)
	ctx := context.Background()
	rdb.Set(ctx, setupBeforeKey(uuid), `{"Target":"modded","At":1}`, time.Hour)
	forgetRecordFor(ctx, rdb, setupBeforeKey(uuid), "other")
	if n, _ := rdb.Exists(ctx, setupBeforeKey(uuid)).Result(); n != 1 {
		t.Fatal("another setup's landing forgot the record")
	}
	forgetRecordFor(ctx, rdb, setupBeforeKey(uuid), "modded")
	if n, _ := rdb.Exists(ctx, setupBeforeKey(uuid)).Result(); n != 0 {
		t.Error("the record outlived the setup that landed")
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
