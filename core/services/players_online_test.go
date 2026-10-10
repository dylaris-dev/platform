package services

import (
	"context"
	"dylaris-core/models"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestSumLatestPlayers(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	tests := []struct {
		name    string
		samples []playerSample
		want    int
	}{
		{"nothing reported", nil, 0},
		{"servers are added", []playerSample{
			{"a", ago(time.Second), 3},
			{"b", ago(2 * time.Second), 4},
		}, 7},
		{"two samples of one server count once, the newer one", []playerSample{
			{"a", ago(10 * time.Second), 5},
			{"a", ago(2 * time.Second), 2},
			{"b", ago(time.Second), 1},
		}, 3},
		{"a server whose last sample is stale counts zero", []playerSample{
			{"a", ago(2 * time.Second), 3},
			{"b", ago(playersFreshFor + time.Second), 9},
		}, 3},
		{"a stale newer-looking sample does not hide a fresh one of the same server", []playerSample{
			{"a", ago(time.Second), 4},
			{"a", ago(playersFreshFor + time.Minute), 9},
		}, 4},
		{"a sample from the future does not count", []playerSample{
			{"a", now.Add(time.Hour), 50},
			{"b", ago(time.Second), 1},
		}, 1},
		{"a negative report is not subtracted", []playerSample{
			{"a", ago(time.Second), -5},
			{"b", ago(time.Second), 2},
		}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sumLatestPlayers(tc.samples, now); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// PlayersOnline end to end against the stream the node writes: the NEWEST
// entry of each server, aged by its Redis-assigned ID.
func TestPlayersOnlineReadsNewestEntryPerServer(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	now := time.Now()

	add := func(uuid string, at time.Time, players int) {
		t.Helper()
		if err := rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: fmt.Sprintf("dylaris:server:%s:stats:buffer", uuid),
			ID:     fmt.Sprintf("%d-0", at.UnixMilli()),
			// A node clock far in the past must not matter: freshness is the ID.
			Values: map[string]interface{}{"data": fmt.Sprintf(`{"ts":1,"players":%d}`, players)},
		}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	add("a", now.Add(-20*time.Second), 8)
	add("a", now.Add(-2*time.Second), 3) // newest wins, not 8+3
	add("b", now.Add(-time.Second), 2)
	add("stopped", now.Add(-10*time.Minute), 40)
	add("proxy", now.Add(-time.Second), 5) // the network's total, already counted on a and b

	servers := []models.Server{{UUID: "a"}, {UUID: "b"}, {UUID: "stopped"}, {UUID: "never-ran"}, {UUID: "proxy", ServerType: "proxy"}}
	got, err := PlayersOnline(ctx, rdb, servers, now)
	if err != nil {
		t.Fatal(err)
	}
	if got != 5 {
		t.Errorf("players online = %d, want 5", got)
	}

	mr.Close()
	if _, err := PlayersOnline(ctx, rdb, servers, now); err == nil {
		t.Error("an unreachable Redis must be an error, not 0 players")
	}
}
