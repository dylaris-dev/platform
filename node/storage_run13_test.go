package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// hangGet blocks GET on one key until release closes, the way a lookup on a
// hung NFS path never returns.
type hangGet struct {
	key     string
	hung    chan struct{}
	release chan struct{}
}

func (hangGet) DialHook(next redis.DialHook) redis.DialHook { return next }
func (hangGet) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h hangGet) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if args := cmd.Args(); len(args) == 2 && strings.EqualFold(cmd.Name(), "get") && args[1] == h.key {
			select {
			case h.hung <- struct{}{}:
			default:
			}
			<-h.release
		}
		return next(ctx, cmd)
	}
}

// S12: GetServerPath and resolveMigration held sm.mu.RLock across their Redis
// lookup and the os.Stat scan. One lookup hung on an NFS path, the next
// SetPlacement queued for the write lock, and every other server's path
// lookup on the node blocked behind it.
func TestAHungServerLookupDoesNotStallOtherLookups(t *testing.T) {
	const slow, other = "eeeeeeee-1111-2222-3333-444444444444", "ffffffff-1111-2222-3333-444444444444"
	for _, tc := range []struct {
		name   string
		lookup func(sm *StorageManager, target string)
	}{
		{"path lookup", func(sm *StorageManager, _ string) { sm.GetServerPath(slow) }},
		{"storage move", func(sm *StorageManager, target string) { sm.resolveMigration(slow, target) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm, roots := newPlacementManager(t, 2)
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })
			sm.rdb = rdb
			if err := os.MkdirAll(filepath.Join(roots[0], other), 0o755); err != nil {
				t.Fatal(err)
			}
			h := hangGet{key: "node:" + nodeID + ":server:" + slow + ":storage", hung: make(chan struct{}), release: make(chan struct{})}
			rdb.AddHook(h)
			lookupDone := make(chan struct{})
			defer func() { close(h.release); <-lookupDone }()

			go func() { defer close(lookupDone); tc.lookup(sm, roots[1]) }()
			<-h.hung
			done := make(chan string, 1)
			go func() {
				sm.SetPlacement(placementAuto, nil)
				done <- sm.GetServerPath(other)
			}()
			select {
			case got := <-done:
				if got != roots[0] {
					t.Fatalf("GetServerPath = %q, want %q", got, roots[0])
				}
			case <-time.After(2 * time.Second):
				t.Fatal("SetPlacement + GetServerPath blocked behind another server's hung lookup")
			}
		})
	}
}

// S13: a server whose storage key names a path that is not mounted resolved to
// paths[0], the default for a NEW server. Delete and migrate_cleanup removed
// the empty directory there, reported success and dropped the key and limit,
// leaving the real tree behind on the path once it came back.
func TestAKeyNamingAnUnmountedPathResolvesToNothing(t *testing.T) {
	const uuid = "abababab-1111-2222-3333-444444444444"
	for _, tc := range []struct {
		name  string
		key   bool
		dirAt bool // the server's directory exists on paths[0]
		want  func(paths []string) string
	}{
		{"keyed not found", true, false, func([]string) string { return "" }},
		{"keyed found on an active path", true, true, func(p []string) string { return p[0] }},
		{"unkeyed new server", false, false, func(p []string) string { return p[0] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })
			sm, _ := newPlacementManager(t, 2)
			sm.rdb = rdb
			paths := sm.Paths()
			if tc.key {
				mr.Set("node:"+nodeID+":server:"+uuid+":storage", t.TempDir())
			}
			if tc.dirAt {
				if err := os.MkdirAll(filepath.Join(paths[0], uuid), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			want := tc.want(paths)
			if got := sm.GetServerPath(uuid); got != want {
				t.Fatalf("GetServerPath = %q, want %q", got, want)
			}
			dm := &DockerManager{storageMgr: sm, hostPathCache: map[string]string{paths[0]: paths[0], paths[1]: paths[1]}, hostDataPath: t.TempDir()}
			if got := dm.resolveHostServerPath(uuid); (got == "") != (want == "") {
				t.Fatalf("resolveHostServerPath = %q, want a path only where the server resolved (%q)", got, want)
			}
		})
	}
}
