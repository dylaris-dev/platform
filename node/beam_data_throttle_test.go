package main

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// A throttled server's beam bytes are paced at Core's rate; any other server,
// or the same one once the key is gone, is not.
func TestServerDataThrottle(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	mr.Set("dylaris:server:srv:data_bw", "100000") // 100 KB/s

	th := NewServerDataThrottle(rdb)
	start := time.Now()
	// 100 KB burst passes at once, the next 50 KB waits ~0.5 s.
	if err := th.WaitN(ctx, "srv", 100_000); err != nil {
		t.Fatal(err)
	}
	if err := th.WaitN(ctx, "srv", 50_000); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 400*time.Millisecond {
		t.Fatalf("150 KB at 100 KB/s took %v, want about 0.5 s", el)
	}
	start = time.Now()
	if err := th.WaitN(ctx, "other", 10_000_000); err != nil || time.Since(start) > 50*time.Millisecond {
		t.Fatalf("an unthrottled server waited %v (%v)", time.Since(start), err)
	}
	var nilT *ServerDataThrottle
	if err := nilT.WaitN(ctx, "srv", 1); err != nil {
		t.Fatal(err)
	}
}
