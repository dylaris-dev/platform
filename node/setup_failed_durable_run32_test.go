package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"dylaris-pkg/queue"
)

// R32-03: a setup failure went out on Pub/Sub only, so a report sent while no
// Core leader listened (a redeploy, a leader change) was lost, and with it the
// whole rollback. It is also kept under the server's prefix, like
// switch_failed, and the Pub/Sub copy says so, so a Core that reads the key
// does not apply it twice. An older Core reads the Pub/Sub copy as before.
func TestASetupFailureReportOutlivesACoreRestart(t *testing.T) {
	const uuid = "32323232-0303-0303-0303-323232323232"
	for _, restored := range []bool{false, true} {
		rdb := newMiniRedis(t)
		ctx := context.Background()
		sub := rdb.Subscribe(ctx, queue.SetupResultsChannel(nodeID))
		if _, err := sub.Receive(ctx); err != nil {
			t.Fatal(err)
		}
		ch := sub.Channel()

		publishSetupFailure(ctx, rdb, uuid, "modded", errors.New("download failed"), restored)

		raw, err := rdb.Get(ctx, "dylaris:server:"+uuid+":setup_failed").Result()
		if err != nil {
			t.Fatalf("restored=%v: no setup_failed key: %v", restored, err)
		}
		if ttl := rdb.TTL(ctx, "dylaris:server:"+uuid+":setup_failed").Val(); ttl <= 0 {
			t.Errorf("restored=%v: key TTL = %v, want one", restored, ttl)
		}
		var rep map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &rep); err != nil {
			t.Fatal(err)
		}
		if rep["serverUuid"] != uuid || rep["subServer"] != "modded" || rep["error"] != "download failed" || (rep["restored"] == true) != restored {
			t.Errorf("key = %v", rep)
		}
		msg := <-ch
		var pub map[string]interface{}
		if err := json.Unmarshal([]byte(msg.Payload), &pub); err != nil {
			t.Fatal(err)
		}
		if pub["durable"] != true || pub["error"] != "download failed" {
			t.Errorf("published = %v, want the report marked durable", pub)
		}
		sub.Close()
	}
}
