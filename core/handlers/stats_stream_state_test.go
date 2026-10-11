package handlers

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// A stopped server's buffer stays in Redis for an hour; replaying it made the
// panel show those values as current. The stream now says whether the newest
// sample is fresh, and replays only then.
func TestReadStatsStreamState(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	ctx := context.Background()
	now := time.Now()
	add := func(key string, at time.Time) {
		t.Helper()
		if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: key, ID: fmt.Sprintf("%d-0", at.UnixMilli()), Values: map[string]interface{}{"data": "{}"}}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	add("running", now.Add(-5*time.Minute))
	add("running", now.Add(-2*time.Second))
	add("stopped", now.Add(-10*time.Minute))

	tests := []struct {
		key, status string
		live        bool
		hasSample   bool
	}{
		{"running", "online", true, true},
		{"stopped", "stopped", false, true},
		{"never-ran", "stopped", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			st := readStatsStreamState(ctx, rdb, tt.key, tt.status, now)
			if st.Live != tt.live || (st.LastSampleAt != nil) != tt.hasSample || st.Status != tt.status {
				t.Errorf("state = %+v, want live=%v sample=%v status=%q", st, tt.live, tt.hasSample, tt.status)
			}
		})
	}
}
