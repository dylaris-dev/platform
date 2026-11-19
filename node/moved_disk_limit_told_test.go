package main

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// R40-10: Core sends diskLimit 0 for an unlimited server. The node read that
// as "not told" and put a stale cached cap back on the server.
func TestAMoveThatSaysUnlimitedDropsAStaleCap(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		want    int64
	}{
		{"explicit unlimited", `{"action":"migrate_in","config":{"uuid":"u","docker":{"diskLimit":0}}}`, 0},
		{"explicit cap", `{"action":"migrate_in","config":{"uuid":"u","docker":{"diskLimit":512}}}`, 512},
		{"older Core sends none", `{"action":"migrate_in","config":{"uuid":"u"}}`, 10240},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			ctx := context.Background()
			recordDiskLimit(ctx, rdb, "u", 10240)
			applyMovedDiskLimit(ctx, rdb, NewQuotaSet(nil), t.TempDir(), "u", sentDiskLimit(tc.payload), "")
			if got := loadDiskLimit(ctx, rdb, "u"); got != tc.want {
				t.Fatalf("limit after the move = %d, want %d", got, tc.want)
			}
		})
	}
}
