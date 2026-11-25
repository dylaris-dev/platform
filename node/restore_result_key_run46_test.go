package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"dylaris-pkg/queue"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// The restore result was one PUBLISH, so one sent while no Core held the
// leader lease was gone, and the reaper closed a restore that had replaced the
// world as failed (R46-02). Every final result is also kept under the server,
// long enough for the reaper to find it.
func TestARestoreResultIsKeptForTheReaper(t *testing.T) {
	tests := []struct{ status, errMsg string }{
		{"success", ""},
		{"failed", "stage dir: disk full"},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { rdb.Close() })
			cmd := BackupRestoreCommand{RestoreID: 12, RunID: 3, ServerUUID: "srv-uuid"}

			reportRestore(context.Background(), rdb, cmd, tt.status, tt.errMsg)

			key := queue.RestoreResultKey("srv-uuid", 12)
			raw, err := mr.Get(key)
			if err != nil {
				t.Fatalf("no result kept at %s: %v", key, err)
			}
			var got struct {
				RestoreID  int    `json:"restoreId"`
				Status     string `json:"status"`
				Error      string `json:"error"`
				ServerUUID string `json:"serverUuid"`
			}
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatal(err)
			}
			if got.RestoreID != 12 || got.Status != tt.status || got.Error != tt.errMsg || got.ServerUUID != "srv-uuid" {
				t.Errorf("kept %+v", got)
			}
			if ttl := mr.TTL(key); ttl < 6*time.Hour {
				t.Errorf("TTL %s, want at least the reaper's six hours", ttl)
			}
		})
	}
}
