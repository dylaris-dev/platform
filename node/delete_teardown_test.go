package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dylaris-pkg/queue"

	"github.com/alicebob/miniredis/v2"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/redis/go-redis/v9"
)

// A delete whose data removal failed logged "fully deleted" and dropped the
// storage mapping and the limit, so the leftover tree stayed on disk with
// nothing pointing at it and nothing capping it.
func TestADeleteWhoseRemovalFailsKeepsTheMapping(t *testing.T) {
	for _, c := range []struct {
		name     string
		fail     bool
		wantKept bool
	}{
		{"removal fails", true, true},
		{"removal works", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			const uuid = sharedDaemonUUID
			t.Cleanup(func() { forgetTombstoneInMemory(uuid) })
			ctx := context.Background()
			rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			dm, _ := newSharedDaemon(t, "-", false, nil)
			root := t.TempDir()
			storage := NewStorageManager(root, rdb)
			dm.storageMgr = storage
			serverDir := filepath.Join(root, uuid)
			if err := os.MkdirAll(serverDir, 0o755); err != nil {
				t.Fatal(err)
			}
			storageKey := fmt.Sprintf("node:%s:server:%s:storage", nodeID, uuid)
			rdb.Set(ctx, storageKey, root, 0)
			recordDiskLimit(ctx, rdb, uuid, 100)
			if c.fail {
				prev := removeTreeVerified
				removeTreeVerified = func(string) error { return errors.New("device or resource busy") }
				t.Cleanup(func() { removeTreeVerified = prev })
			}

			cmd := NodeCommand{Action: "delete"}
			cmd.Config.UUID = uuid
			processCommand(ctx, cmd, "", rdb, dm, "node-a", nil, storage)

			for _, key := range []string{storageKey, diskLimitKey(uuid)} {
				if kept := rdb.Exists(ctx, key).Val() == 1; kept != c.wantKept {
					t.Errorf("%s kept = %v, want %v", key, kept, c.wantKept)
				}
			}
			if _, err := os.Stat(serverDir); os.IsNotExist(err) == c.wantKept {
				t.Errorf("server dir gone = %v, want %v", os.IsNotExist(err), !c.wantKept)
			}
		})
	}
}

// An update_resources that ran beside a delete put the no-TTL limit key back
// for a server that no longer exists.
func TestUpdateResourcesBesideADeleteLeavesNoLimit(t *testing.T) {
	for _, c := range []struct {
		deleted   bool
		wantLimit bool
	}{
		{true, false},
		{false, true},
	} {
		t.Run(fmt.Sprintf("deleted=%v", c.deleted), func(t *testing.T) {
			const uuid = sharedDaemonUUID
			t.Cleanup(func() { forgetTombstoneInMemory(uuid) })
			ctx := context.Background()
			rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			dm, _ := newSharedDaemon(t, "-", false, nil)
			storage := NewStorageManager(t.TempDir(), rdb)
			dm.storageMgr = storage
			forgetTombstoneInMemory(uuid)
			if c.deleted {
				markServerDeleted(uuid)
			}

			cmd := NodeCommand{Action: "update_resources"}
			cmd.Config.UUID = uuid
			cmd.Config.Docker.DiskLimit = 2048
			processCommand(ctx, cmd, "", rdb, dm, "node-a", NewQuotaSet(nil), storage)

			if got := rdb.Exists(ctx, diskLimitKey(uuid)).Val() == 1; got != c.wantLimit {
				t.Errorf("disk limit key present = %v, want %v", got, c.wantLimit)
			}
		})
	}
}

// A restart or resource change on a shared daemon copied the binds of another
// node's stopped mc_<uuid>, and so ran the server on that node's data directory.
func TestRecreatesReuseOnlyTheirOwnBinds(t *testing.T) {
	prev := nodeSecretDir
	nodeSecretDir = t.TempDir()
	t.Cleanup(func() { nodeSecretDir = prev })
	t.Setenv("NODE_ID", "node-a")
	binds := []string{"/srv/a/" + sharedDaemonUUID + ":/data"}
	for _, c := range []struct {
		owner string
		want  bool
	}{
		{"node-a", true},
		{"", true}, // created before the label existed
		{"node-b", false},
	} {
		info := container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{HostConfig: &container.HostConfig{Binds: binds}},
			Config:            &container.Config{Labels: map[string]string{}},
		}
		if c.owner != "" {
			info.Config.Labels[ownerLabel] = c.owner
		}
		if got := len(ownBinds(info)) > 0; got != c.want {
			t.Errorf("owner %q: binds reused = %v, want %v", c.owner, got, c.want)
		}
	}
	b, err := os.ReadFile("docker_mgr.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "info.HostConfig.Binds..."); n != 0 {
		t.Errorf("docker_mgr.go copies inspected binds without the owner check %d time(s)", n)
	}
}

// A migrate_cleanup whose container removal failed still deleted the data and
// the archive, so the source reconciler restarted the leftover container on an
// empty directory.
func TestMigrateCleanupStopsWhenTheContainerStays(t *testing.T) {
	const uuid = "abababab-3333-3333-4444-555555555555"
	for _, c := range []struct {
		name      string
		removeRes int
		wantKept  bool
	}{
		{"remove fails", http.StatusInternalServerError, true},
		{"already gone", http.StatusNotFound, false},
		{"removed", http.StatusNoContent, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			prev := nodeSecretDir
			nodeSecretDir = t.TempDir()
			t.Cleanup(func() { nodeSecretDir = prev })
			t.Setenv("NODE_ID", "node-a")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/mc_"+uuid+"/json"):
					w.Write([]byte(`{"Id":"c-id","State":{"Running":false},"Config":{"Labels":{}},"HostConfig":{}}`))
				case r.Method == http.MethodDelete:
					w.WriteHeader(c.removeRes)
					if c.removeRes != http.StatusNoContent {
						w.Write([]byte(`{"message":"daemon busy"}`))
					}
				default:
					w.Write([]byte(`{}`))
				}
			}))
			t.Cleanup(srv.Close)
			cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.44"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cli.Close() })
			dm := &DockerManager{cli: cli, ctx: t.Context(), localDataPath: t.TempDir()}

			root := t.TempDir()
			world := filepath.Join(root, uuid, "level.dat")
			zip := stagedArchivePath(root, uuid)
			for _, p := range []string{world, zip} {
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			handleMigrateCleanup(context.Background(), rdb, &StorageManager{paths: []string{root}, rdb: rdb}, dm, "tok", uuid)

			for _, p := range []string{world, zip} {
				if _, err := os.Stat(p); (err == nil) != c.wantKept {
					t.Errorf("%s kept = %v, want %v", p, err == nil, c.wantKept)
				}
			}
			status := rdb.Get(context.Background(), queue.MigrationStatusKey("tok", uuid)).Val()
			if isErr := strings.Contains(status, `"error"`); isErr != c.wantKept {
				t.Errorf("status %q: error = %v, want %v", status, isErr, c.wantKept)
			}
		})
	}
}
