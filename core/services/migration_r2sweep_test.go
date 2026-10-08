package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// The pre-signed PUT outlives the transfer's own delete, so a source could
// upload again and leave an object for good. The sweep deletes it once more
// after the URLs expired - only then, and forgets it only when that worked.
func TestSweepR2TransfersDeletesOnlyWhatIsDue(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx := context.Background()
	o := &MigrationOrchestrator{redis: rdb}
	now := time.Now()
	add := func(key string, at time.Time) {
		rdb.ZAdd(ctx, migrationR2SweepKey, redis.Z{Score: float64(at.Unix()), Member: key})
	}
	add("due", now.Add(-time.Minute))
	add("live", now.Add(time.Hour))
	add("failing", now.Add(-time.Minute))
	add("gone-storage", now.Add(-8*24*time.Hour))

	var deleted []string
	o.sweepR2Transfers(ctx, now, func(_ context.Context, key string) error {
		if key == "failing" || key == "gone-storage" {
			return errors.New("storage error")
		}
		deleted = append(deleted, key)
		return nil
	})

	if len(deleted) != 1 || deleted[0] != "due" {
		t.Fatalf("deleted %v, want only the due object", deleted)
	}
	left, _ := rdb.ZRange(ctx, migrationR2SweepKey, 0, -1).Result()
	want := map[string]bool{"live": true, "failing": true}
	if len(left) != len(want) {
		t.Fatalf("left %v, want live (not due) and failing (retried next time)", left)
	}
	for _, k := range left {
		if !want[k] {
			t.Fatalf("left %v, want live and failing", left)
		}
	}
}
