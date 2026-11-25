package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// R45-02: a refusal that expired before Core read it (Core down for more than
// five minutes) left Core's 24h switch_undo record pending, and setup,
// reinstall, version move and copy were refused until it ran out. The report
// now lives as long as the record it answers; an older Core writes none, and
// the report keeps its five minutes.
func TestSwitchFailedReportOutlivesNoRecordItAnswers(t *testing.T) {
	const uuid = "45454545-0202-0202-0202-454545454545"
	for _, tc := range []struct {
		name     string
		undoTTL  time.Duration // 0 = no switch_undo record
		min, max time.Duration
	}{
		{"Core's record pending for a day", 24 * time.Hour, 23 * time.Hour, 24 * time.Hour},
		{"Core's record shorter than five minutes", time.Minute, 4 * time.Minute, 5 * time.Minute},
		{"an older Core wrote no record", 0, 4 * time.Minute, 5 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rdb := newMiniRedis(t)
			ctx := context.Background()
			if tc.undoTTL > 0 {
				rdb.Set(ctx, switchUndoKey(uuid), `{"target":"b","previous":"a"}`, tc.undoTTL)
			}
			reportSwitchFailed(ctx, rdb, uuid, "b", "a", "no runnable server")
			ttl := rdb.TTL(ctx, fmt.Sprintf("dylaris:server:%s:switch_failed", uuid)).Val()
			if ttl < tc.min || ttl > tc.max {
				t.Errorf("report TTL = %v, want between %v and %v", ttl, tc.min, tc.max)
			}
		})
	}
}
