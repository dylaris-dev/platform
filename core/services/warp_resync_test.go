package services

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestProcessResync_PushesRegionPeerSetAndClearsPerLeaderKey(t *testing.T) {
	svc, fs, mr := enrollTestService(t)
	for _, p := range []struct{ pub, ip, home string }{
		{"pub1", "10.0.99.2", "leader-01"},
		{"pub2", "10.0.99.3", "leader-01"},
		// Homed on another leader: it must not be handed to this one, or the
		// client ping-pongs between the two (see Enroll).
		{"pub3", "10.0.99.4", "leader-02"},
	} {
		sp := storePeer(1, p.pub, p.ip)
		sp.AssignedLeader = p.home
		fs.InsertWarpPeer(sp)
	}
	ctx := context.Background()
	// Per-leader resync key: leader "leader-01" serves region "leader-01".
	svc.redis.Set(ctx, "dylaris:warp:leader-01:resync-request", "leader-01", 0)

	if err := svc.processResync(ctx); err != nil {
		t.Fatalf("resync: %v", err)
	}

	vals, _ := mr.List("dylaris:warp:leader-01:queue")
	if len(vals) != 1 {
		t.Fatalf("expected 1 resync command, got %d", len(vals))
	}
	var cmd struct {
		Type  string `json:"type"`
		Peers []struct {
			Pubkey     string   `json:"pubkey"`
			WGIP       string   `json:"wg_ip"`
			AllowedIPs []string `json:"allowed_ips"`
		} `json:"peers"`
	}
	if err := json.Unmarshal([]byte(vals[0]), &cmd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cmd.Type != "resync" || len(cmd.Peers) != 2 {
		t.Fatalf("bad resync command: %+v", cmd)
	}
	if _, err := svc.redis.Get(ctx, "dylaris:warp:leader-01:resync-request").Result(); err != redis.Nil {
		t.Fatal("per-leader resync-request key should be cleared after processing")
	}
}
