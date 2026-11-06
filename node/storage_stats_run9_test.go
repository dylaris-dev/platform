package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/client"
	"github.com/redis/go-redis/v9"
)

// P17: GetStorageInfo and SelectStoragePath ran statfs under sm.mu. A hung
// NFS path held the read lock forever, the next SetPlacement queued for the
// write lock, and Go's RWMutex then blocked every GetServerPath on the node,
// servers on the healthy disk included.
func TestAHungStoragePathDoesNotStallPathLookups(t *testing.T) {
	for _, tc := range []struct {
		name string
		stat func(sm *StorageManager)
	}{
		{"heartbeat storage info", func(sm *StorageManager) { sm.GetStorageInfo() }},
		{"auto placement", func(sm *StorageManager) { sm.SelectStoragePath("cccccccc-1111-2222-3333-444444444444", "") }},
		{"manual placement", func(sm *StorageManager) {
			sm.SetPlacement(placementManual, nil)
			sm.SelectStoragePath("cccccccc-1111-2222-3333-444444444444", "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			sm := NewStorageManager(root, nil)
			const uuid = "dddddddd-1111-2222-3333-444444444444"
			if err := os.MkdirAll(filepath.Join(root, uuid), 0o755); err != nil {
				t.Fatal(err)
			}

			hung, release := make(chan struct{}), make(chan struct{})
			prev := diskSpaceOf
			t.Cleanup(func() { diskSpaceOf = prev })
			diskSpaceOf = func(string) (uint64, uint64) {
				select {
				case hung <- struct{}{}:
				default:
				}
				<-release
				return 1 << 30, 1 << 29
			}
			statDone := make(chan struct{})
			defer func() { close(release); <-statDone }()

			go func() { defer close(statDone); tc.stat(sm) }()
			<-hung
			done := make(chan string, 1)
			go func() {
				sm.SetPlacement(placementAuto, nil)
				done <- sm.GetServerPath(uuid)
			}()
			select {
			case got := <-done:
				if got != root {
					t.Fatalf("GetServerPath = %q, want %q", got, root)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("SetPlacement + GetServerPath blocked behind a hung statfs")
			}
		})
	}
}

// failExists fails EXISTS on one key, the way a single Redis timeout lands.
type failExists struct{ key string }

func (failExists) DialHook(next redis.DialHook) redis.DialHook { return next }
func (failExists) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (f failExists) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if args := cmd.Args(); len(args) == 2 && strings.EqualFold(cmd.Name(), "exists") && args[1] == f.key {
			err := errors.New("i/o timeout")
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}

// P18: an error on the counted EXISTS read as "never counted", so the boot
// re-walked a counted server with the once-only grace allowed; an error on the
// lifted EXISTS skipped a server whose limit a crash had left lifted.
func TestTheBootRecountDoesNotReadARedisErrorAsAnAnswer(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		failKey  string
		counted  bool
		wantDone bool
		wantErr  bool
		wantWalk bool
	}{
		{name: "counted check fails", failKey: diskCountedKey("srv"), counted: true, wantErr: true},
		{name: "counted check fails, never counted", failKey: diskCountedKey("srv"), wantErr: true},
		{name: "lifted check fails on a counted server", failKey: diskLiftedKey("srv"), counted: true, wantDone: true, wantWalk: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rdb := newDiskGuardRedis(t)
			recordDiskLimit(ctx, rdb, "srv", 100)
			if tc.counted {
				rdb.Set(ctx, diskCountedKey("srv"), 1, 0)
			}
			rdb.AddHook(failExists{key: tc.failKey})
			// The walk takes it from under its limit to over: a grace if allowed.
			q := &fakeQuota{before: usageOf(10*mib, 100*mib), after: usageOf(150*mib, 100*mib), limitMB: 100}

			done, err := reconcileServer(ctx, rdb, q, "srv")
			if done != tc.wantDone || (err != nil) != tc.wantErr || q.assigned != tc.wantWalk {
				t.Fatalf("reconcileServer = %v, %v (walked %v), want %v, error %v (walked %v)",
					done, err, q.assigned, tc.wantDone, tc.wantErr, tc.wantWalk)
			}
			if _, ok := loadDiskGrace(ctx, rdb, "srv"); ok {
				t.Fatal("the boot granted the once-only grace on a Redis error")
			}
		})
	}
}

// P20: for a container that is not running, Docker's one-shot stats answers
// 200 with only Name and Id. The collector took that as a sample and wrote
// "online" over the "stopped" the stop had just posted.
func TestStatsOfAStoppedContainerAreAnError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"stopped", `{"name":"/mc_x","id":"abc"}`, true},
		{"running", `{"name":"/mc_x","id":"abc","read":"2026-11-06T10:00:00Z","memory_stats":{"usage":1048576,"limit":2097152}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.44"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cli.Close() })
			dm := &DockerManager{cli: cli, ctx: t.Context()}

			prev := &PrevCPUStats{TotalUsage: 7, ContainerID: "abc"}
			stats, gotPrev, err := dm.GetContainerStats("mc_x", prev)
			if (err != nil) != tc.wantErr {
				t.Fatalf("GetContainerStats error = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr && (stats != nil || gotPrev != prev) {
				t.Fatalf("a stopped container returned stats %+v and replaced the CPU counters", stats)
			}
		})
	}
}
