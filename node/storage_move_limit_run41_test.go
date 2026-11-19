package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// R41-07: a storage move found already on its target reads the limit while
// the server resolves to the target, whose project may still hold the bhard
// an earlier stay there left. Core's unlimited and the saved limit win over
// that project; it is believed only when nothing else names a limit.
func TestAFinishedStorageMoveIgnoresTheTargetsStaleProjectLimit(t *testing.T) {
	const uuid = "d4e5f6a7-0000-4000-8000-000000004107"
	cases := []struct {
		name     string
		cachedMB int64
		saved    *int64 // nil: no saved config
		want     int64
	}{
		{"saved unlimited beats the stale project", 0, ptrInt64(0), 0},
		{"saved limit beats the stale project", 0, ptrInt64(500), 500},
		{"cached limit beats both", 700, ptrInt64(0), 700},
		{"no saved config falls back to the project", 0, nil, 10240},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			prevMgr := globalStorageMgr
			t.Cleanup(func() { globalStorageMgr = prevMgr })
			sm := NewStorageManager(t.TempDir(), nil)
			globalStorageMgr = sm
			dir := filepath.Join(sm.Paths()[0], uuid)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.saved != nil {
				cfg := ServerConfig{UUID: uuid}
				cfg.Docker.DiskLimit = *tc.saved
				saveNodeConfigWithDiskLimit(dir, cfg)
			}
			rdb := newMiniRedis(t)
			recordDiskLimit(ctx, rdb, uuid, tc.cachedMB)
			stale := &fakeQuota{before: usageOf(0, 10240*mib)}
			if got := movedDiskLimitMB(ctx, rdb, stale, uuid); got != tc.want {
				t.Fatalf("limit = %d MB, want %d", got, tc.want)
			}
		})
	}
}

func ptrInt64(v int64) *int64 { return &v }
