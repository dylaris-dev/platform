package stats

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// A new stats stream starts with the newest samples the live chart holds,
// oldest first - not the node's whole hour, which the panel took one update
// at a time.
func TestStatsReplayIsTheNewestSamplesInOrder(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	ctx := context.Background()
	for i := 1; i <= 200; i++ {
		if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: "buf", ID: fmt.Sprintf("%d-0", i), Values: map[string]any{"data": fmt.Sprint(i)}}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	got := statsReplay(ctx, rdb, "buf")
	if len(got) != statsReplayMax || got[0] != "141" || got[len(got)-1] != "200" {
		t.Fatalf("replayed %v, want samples 141..200", got)
	}
	if got := statsReplay(ctx, rdb, "missing"); len(got) != 0 {
		t.Fatalf("an empty buffer replayed %v", got)
	}
}
