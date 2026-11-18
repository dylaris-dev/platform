package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "dylaris-proto/beam"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// A08: a storage-key GET Redis could not answer, with a leftover copy of the
// server on an earlier path, resolved to the leftover. Only the key can say
// which copy is live; with one copy the scan is still used, as before.
func TestAnUnreadableStorageKeyDoesNotPickBetweenCopies(t *testing.T) {
	for _, tc := range []struct {
		name string
		both bool
		want int // index into the paths, -1 for ""
	}{
		{"one copy", false, 0},
		{"a leftover copy", true, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })
			leftover, live := t.TempDir(), t.TempDir()
			sm := NewStorageManager(leftover+","+live, rdb)
			dirs := []string{leftover}
			if tc.both {
				dirs = append(dirs, live)
			}
			for _, p := range dirs {
				if err := os.MkdirAll(filepath.Join(p, run18UUID), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// A key of the wrong type fails the GET, as a Redis blip does.
			key := "node:" + nodeID + ":server:" + run18UUID + ":storage"
			if _, err := mr.Lpush(key, "x"); err != nil {
				t.Fatal(err)
			}
			want := ""
			if tc.want >= 0 {
				want = sm.Paths()[tc.want]
			}
			if got := sm.GetServerPath(run18UUID); got != want {
				t.Fatalf("GetServerPath = %q, want %q", got, want)
			}
		})
	}
}

// A06: ReadFileContent and SaveFileContent moved up to 10 MB per call past
// both the node-wide Beam caps and the per-server data throttle.
func TestBeamContentRPCsArePaced(t *testing.T) {
	content := strings.Repeat("a", 200_000)
	for _, tc := range []struct {
		name string
		set  func(t *testing.T, bs *beamServer, uuid string)
		save bool
	}{
		{"read node cap", func(t *testing.T, bs *beamServer, _ string) { bs.throttle.setLimits(0, 1) }, false},
		{"save node cap", func(t *testing.T, bs *beamServer, _ string) { bs.throttle.setLimits(1, 0) }, true},
		{"read data throttle", setDataThrottle, false},
		{"save data throttle", setDataThrottle, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bs, uuid, ctx := newTestBeamServer(t)
			dir := filepath.Join(bs.storageMgr.GetServerDir(uuid), "survival")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			tc.set(t, bs, uuid)
			// The limiter refuses at once a wait past the deadline.
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			var err error
			var ok bool
			if tc.save {
				var r *pb.BeamOpResp
				if r, err = bs.SaveFileContent(ctx, &pb.BeamFileSaveReq{Path: "survival/a.txt", Content: content}); r != nil {
					ok = r.Success
				}
			} else {
				var r *pb.BeamFileContentResp
				if r, err = bs.ReadFileContent(ctx, &pb.BeamFileReadReq{Path: "survival/a.txt"}); r != nil {
					ok = r.Success
				}
			}
			if err == nil || ok {
				t.Fatalf("200 KB at 1 byte/s passed at once (err %v, success %v)", err, ok)
			}
		})
	}
}

func setDataThrottle(t *testing.T, bs *beamServer, uuid string) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.Set("dylaris:server:"+uuid+":data_bw", "1000")
	bs.dataThrottle = NewServerDataThrottle(rdb)
}
