package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestTheHeartbeatCarriesTheMachineHostname asserts the WRITE, not the helper.
//
// The hostname is in the heartbeat for one reason: it is the only value a Link
// and a node on the same machine can both arrive at without being told about
// each other. A mode:global Swarm service gets no per-replica configuration -
// the env template exposes .Node.Hostname and .Node.ID and nothing else, and
// neither is the server-assigned uuid everything is keyed by - so the Hub joins
// the two on this field to publish beam:node:<uuid>, which is what a beam
// ticket is resolved through.
//
// Testing machineHostname() alone would not have caught the field being dropped
// from the payload, and a dropped field here is silent: Core ignores it, and the
// Hub would simply publish no mapping, which shows up as beam transfers that do
// not start.
//
// Only a node holding CLUSTER_SECRET sends it: the Hub's join is meant for
// platform machines, and a BYON tenant chooses NODE_HOSTNAME freely, so a BYON
// heartbeat carrying a platform machine's name would move that machine's Link
// mapping onto the tenant's node.
func TestTheHeartbeatCarriesTheMachineHostname(t *testing.T) {
	cases := []struct {
		name          string
		clusterSecret string
		wantHostname  bool
	}{
		{"platform node", "cluster-secret", true},
		{"BYON node without CLUSTER_SECRET", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			defer rdb.Close()

			t.Setenv("NODE_HOSTNAME", "eu-node-07")
			old := clusterSecret
			clusterSecret = tc.clusterSecret
			t.Cleanup(func() { clusterSecret = old })
			const id = "597de090-d6b5-4c66-b617-d13b676ecb53"

			sendHeartbeat(context.Background(), rdb, id, "dev", "eu-central", nil, nil)

			raw, err := rdb.Get(context.Background(), "dylaris:discovery:"+id).Result()
			if err != nil {
				t.Fatalf("the heartbeat wrote no discovery key: %v", err)
			}
			var got map[string]interface{}
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatalf("heartbeat is not JSON: %v", err)
			}

			h, has := got["hostname"]
			if tc.wantHostname && h != "eu-node-07" {
				t.Errorf("hostname = %v, want the NODE_HOSTNAME value.\n"+
					"Without it the Hub cannot tie a self-enrolled Link to this node, and beam:node:<uuid> is never published - "+
					"which surfaces as a beam transfer that will not start, with nothing in any log naming the cause",
					h)
			}
			if !tc.wantHostname && has {
				t.Errorf("hostname = %v sent by a node without CLUSTER_SECRET; the Hub would join a BYON node to a platform Link", h)
			}
			// The id is what the hostname has to be joined WITH, so a payload carrying
			// one and not the other is useless in the same way.
			if got["id"] != id {
				t.Errorf("id = %v, want %q", got["id"], id)
			}
		})
	}
}
