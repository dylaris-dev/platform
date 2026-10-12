package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// Core lets a warp key roll tell the operator "leave the link alone" only on
// this field, so it is written exactly while the link updater says it would
// move the Link, and left out otherwise.
func TestTheHeartbeatSaysWhetherTheNodeSwapsItsLinkKey(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	t.Cleanup(func() { linkKeySwapReady.Store(false) })
	const id = "node-swap-test"

	for _, ready := range []bool{false, true} {
		linkKeySwapReady.Store(ready)
		sendHeartbeat(context.Background(), rdb, id, "dev", "eu-central", nil, nil)
		raw, err := rdb.Get(context.Background(), "dylaris:discovery:"+id).Result()
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]interface{}
		json.Unmarshal([]byte(raw), &got)
		if _, has := got["linkKeySwap"]; has != ready {
			t.Errorf("ready=%v: linkKeySwap present = %v", ready, has)
		}
	}
}
