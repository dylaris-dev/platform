package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/docker/docker/api/types/container"
)

// V12: a start that waited on a storage move's fence inspected the container
// before the move removed it, and recreated it from the old /data bind. The
// move had deleted that tree, so Docker made it again, empty, and the server
// ran a fresh world there for good while its real one sat on the target.
func TestRestartBindsDropAnOldBindTheMoveDeleted(t *testing.T) {
	const uuid = sharedDaemonUUID
	tests := []struct {
		name     string
		bindAt   int  // the root the container's /data bind names
		oldThere bool // the tree still exists under that root
		noKey    bool // no storage key: the copy is a scan's guess
		want     func(old []string) []string
	}{
		{"bound where the server lives", 1, true, false, func(old []string) []string { return old }},
		{"moved away and the old tree deleted", 0, false, false, func([]string) []string { return nil }},
		// W12: the key names the target, so the source is the copy the
		// move's finish deletes, however whole it still is.
		{"two copies and the key names the other", 0, true, false, func([]string) []string { return nil }},
		{"two copies and no key to say which", 0, true, true, func(old []string) []string { return old }},
		{"bound outside every storage path", -1, true, false, func(old []string) []string { return old }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sm, roots := newPlacementManager(t, 2)
			if err := os.MkdirAll(filepath.Join(roots[1], uuid), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.oldThere && tc.bindAt >= 0 {
				if err := os.MkdirAll(filepath.Join(roots[tc.bindAt], uuid), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			mr := newMiniRedis(t)
			sm.rdb = mr
			if !tc.noKey {
				sm.setRedisPath(uuid, roots[1])
			}
			dm := &DockerManager{storageMgr: sm, hostPathCache: map[string]string{roots[0]: roots[0], roots[1]: roots[1]}, hostDataPath: t.TempDir()}

			bindRoot := t.TempDir()
			if tc.bindAt >= 0 {
				bindRoot = roots[tc.bindAt]
			}
			old := []string{filepath.Join(bindRoot, uuid) + ":/data"}
			info := container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{HostConfig: &container.HostConfig{Binds: old}}}
			if got, want := dm.restartBinds(info, uuid), tc.want(old); !reflect.DeepEqual(got, want) {
				t.Fatalf("restartBinds = %v, want %v", got, want)
			}
		})
	}
}
