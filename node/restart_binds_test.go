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
		want     func(old []string) []string
	}{
		{"bound where the server lives", 1, true, func(old []string) []string { return old }},
		{"moved away and the old tree deleted", 0, false, func([]string) []string { return nil }},
		{"two copies and the old one still there", 0, true, func(old []string) []string { return old }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sm, roots := newPlacementManager(t, 2)
			if err := os.MkdirAll(filepath.Join(roots[1], uuid), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.oldThere {
				if err := os.MkdirAll(filepath.Join(roots[tc.bindAt], uuid), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			mr := newMiniRedis(t)
			sm.rdb = mr
			sm.setRedisPath(uuid, roots[1])
			dm := &DockerManager{storageMgr: sm, hostPathCache: map[string]string{roots[0]: roots[0], roots[1]: roots[1]}, hostDataPath: t.TempDir()}

			old := []string{filepath.Join(roots[tc.bindAt], uuid) + ":/data"}
			info := container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{HostConfig: &container.HostConfig{Binds: old}}}
			if got, want := dm.restartBinds(info, uuid), tc.want(old); !reflect.DeepEqual(got, want) {
				t.Fatalf("restartBinds = %v, want %v", got, want)
			}
		})
	}
}
