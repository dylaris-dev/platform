package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// R46-01: version_update_failed, copy_sub_failed and setup_failed expired
// after five or ten minutes while Core's matching record lived an hour or two,
// so a failure Core could not read in that window (Core down, no leader) was
// lost and the record's state stayed wrong. Each report now lives as long as
// the record it answers; an older Core writes none, and the report keeps its
// old TTL.
func TestFailureReportsOutliveTheRecordTheyAnswer(t *testing.T) {
	const uuid = "46464646-0101-0101-0101-464646464646"
	reports := []struct {
		name, record, report string
		old                  time.Duration
		write                func(ctx context.Context, rdb *redis.Client)
	}{
		{"version_update_failed", "version_update_undo", "version_update_failed", 5 * time.Minute,
			func(ctx context.Context, rdb *redis.Client) {
				abandonVersionUpdate(ctx, rdb, uuid, "download failed")
			}},
		{"copy_sub_failed", "copy_pending", "copy_sub_failed", 5 * time.Minute,
			func(ctx context.Context, rdb *redis.Client) {
				reportCopyFailed(ctx, rdb, uuid, "b", "disk full")
			}},
		{"setup_failed", "setup_before", "setup_failed", 10 * time.Minute,
			func(ctx context.Context, rdb *redis.Client) {
				publishSetupFailure(ctx, rdb, uuid, "b", errors.New("install failed"), false)
			}},
	}
	for _, r := range reports {
		for _, tc := range []struct {
			name     string
			recTTL   time.Duration // 0 = no record (older Core)
			min, max time.Duration
		}{
			{"Core's record pending for two hours", 2 * time.Hour, 2*time.Hour - time.Minute, 2 * time.Hour},
			{"Core's record shorter than the old TTL", time.Minute, r.old - time.Minute, r.old},
			{"an older Core wrote no record", 0, r.old - time.Minute, r.old},
		} {
			t.Run(r.name+"/"+tc.name, func(t *testing.T) {
				rdb := newMiniRedis(t)
				ctx := context.Background()
				recKey := fmt.Sprintf("dylaris:server:%s:%s", uuid, r.record)
				if tc.recTTL > 0 {
					rdb.Set(ctx, recKey, `{"target":"b","at":1}`, tc.recTTL)
				} else if r.record == "setup_before" {
					// The setup key is written only for a stamped setup;
					// a record with no TTL stands in for the old TTL case.
					rdb.Set(ctx, recKey, `{"target":"b","at":1}`, 0)
				}
				r.write(ctx, rdb)
				ttl := rdb.TTL(ctx, fmt.Sprintf("dylaris:server:%s:%s", uuid, r.report)).Val()
				if ttl < tc.min || ttl > tc.max {
					t.Errorf("report TTL = %v, want between %v and %v", ttl, tc.min, tc.max)
				}
			})
		}
	}
}
