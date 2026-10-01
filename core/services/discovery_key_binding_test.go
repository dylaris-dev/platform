package services

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// A node may write only its own discovery key, but chose the id inside it: a
// heartbeat naming another node was read as that node's.
func TestAHeartbeatSpeaksOnlyForTheNodeWhoseKeyItIsIn(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	mr.Set("dylaris:discovery:honest", `{"id":"honest","ramFree":1}`)
	mr.Set("dylaris:discovery:attacker", `{"id":"victim","ramFree":999}`)

	beats := LoadHeartbeats(t.Context(), rdb)
	if _, ok := beats["victim"]; ok {
		t.Fatal("a heartbeat in the attacker's key was taken as the victim's")
	}
	if _, ok := beats["honest"]; !ok || len(beats) != 1 {
		t.Fatalf("beats = %v, want only the honest node", beats)
	}
}
