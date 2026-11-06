package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// M04: the directory scan is the tenant's /data, so plugin code can create as
// many directories as it has inodes. Each one used to cost a Redis round trip
// every pass, for every server on the node, in one goroutine.
func TestShipperGuardSubServersIsBounded(t *testing.T) {
	dir := t.TempDir()
	for i := range 500 {
		os.Mkdir(filepath.Join(dir, fmt.Sprintf("a%04d", i)), 0o755)
	}
	os.Mkdir(filepath.Join(dir, "bad name"), 0o755)
	os.Mkdir(filepath.Join(dir, ".hidden"), 0o755)
	os.Mkdir(filepath.Join(dir, "zz-active"), 0o755)
	os.WriteFile(filepath.Join(dir, ".active_server"), []byte("zz-active\n"), 0o644)

	subs := shipperGuardSubServers(dir)
	if len(subs) > shipperGuardMaxSubServers {
		t.Fatalf("%d sub-server keys per pass, want at most %d", len(subs), shipperGuardMaxSubServers)
	}
	if len(subs) == 0 || subs[0] != "zz-active" {
		t.Fatalf("the active sub-server (sorted last) must always be guarded, got %v", subs[:min(3, len(subs))])
	}
	for _, s := range subs {
		if s == "bad name" || s == ".hidden" {
			t.Fatalf("%q is not a sub-server name and was guarded", s)
		}
	}
}

// P01: a console stream over its cap is trimmed to its newest lines, not
// deleted. The shipper's own bound (5000 lines of up to 64 KiB) is far over
// the cap, so a chatty server used to lose its whole history every pass. What
// the trim cannot bring under the cap (one huge entry, or a non-stream written
// at that name) is still deleted.
func TestShipperGuardTrimsAnOversizedConsoleStream(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	ctx := context.Background()
	line := strings.Repeat("x", 1024)

	logs := shipperGuardedKeys("u1", nil)[0]
	for i := range 2000 {
		mr.XAdd(logs.key, "*", []string{"line", fmt.Sprintf("%d %s", i, line)})
	}
	used, _ := rdb.MemoryUsage(ctx, logs.key).Result()
	logs.cap = used / 2
	if guardKey(ctx, rdb, logs) {
		t.Fatal("an oversized console stream was deleted instead of trimmed")
	}
	n, _ := rdb.XLen(ctx, logs.key).Result()
	if n == 0 || n > shipperStreamTrimLen {
		t.Fatalf("stream holds %d lines after the guard, want 1..%d", n, shipperStreamTrimLen)
	}
	last, _ := rdb.XRevRangeN(ctx, logs.key, "+", "-", 1).Result()
	if len(last) != 1 || !strings.HasPrefix(last[0].Values["line"].(string), "1999 ") {
		t.Fatalf("the newest line was not kept: %v", last)
	}

	for _, tc := range []struct {
		name string
		put  func(key string)
	}{
		{"one entry over the cap", func(k string) { mr.XAdd(k, "*", []string{"line", strings.Repeat("y", 1<<20)}) }},
		{"a string at the stream's name", func(k string) { mr.Set(k, strings.Repeat("y", 1<<20)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := guardedKey{"dylaris:server:u2:logs", 1 << 10, 0, shipperStreamTrimLen}
			mr.Del(k.key)
			tc.put(k.key)
			if !guardKey(ctx, rdb, k) || mr.Exists(k.key) {
				t.Fatal("a stream the trim cannot bound was kept")
			}
		})
	}
}
