package main

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dylaris-pkg/fileperms"
	pb "dylaris-proto/beam"

	"github.com/pkg/sftp"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/peer"
)

// S07: an overwrite through a symlink inside the write scope truncates the
// link's target, but the size it frees was read with Lstat (the link), so a
// save at the disk limit that frees space was refused and the gauge was
// booked the whole new size.
func TestAnOverwriteThroughASymlinkCreditsTheTargetsSize(t *testing.T) {
	const uuid = "07070707-0707-0707-0707-070707070707"
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, sm *StorageManager, rdb *redis.Client) error
	}{
		{"beam save", func(t *testing.T, sm *StorageManager, rdb *redis.Client) error {
			bs := &beamServer{storageMgr: sm, rdb: rdb, throttle: NewBeamThrottle(context.Background(), nil)}
			addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 42007}
			ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: addr})
			bs.serverUUIDByPeer.Store(addr.String(), uuid)
			bindTestSession(bs, addr.String())
			full := fileperms.Full()
			bs.permsByPeer.Store(addr.String(), &full)
			resp, err := bs.SaveFileContent(ctx, &pb.BeamFileSaveReq{Path: "plugins/cfg.yml", Content: "small"})
			if err != nil {
				return err
			}
			if !resp.Success {
				return &os.PathError{Op: "save", Path: "plugins/cfg.yml", Err: os.ErrPermission}
			}
			return nil
		}},
		{"sftp put", func(t *testing.T, sm *StorageManager, rdb *redis.Client) error {
			refs := []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}
			w, err := newVirtualFS(refs, sm, rdb, "ivy").Filewrite(&sftp.Request{Method: "Put", Filepath: "s/plugins/cfg.yml", Flags: 0x02 | 0x08 | 0x10})
			if err != nil {
				return err
			}
			if _, err := w.WriteAt([]byte("small"), 0); err != nil {
				w.(io.Closer).Close()
				return err
			}
			return w.(io.Closer).Close()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rdb := gaugeRedis(t)
			sm := NewStorageManager(t.TempDir(), rdb)
			dir := filepath.Join(sm.GetServerDir(uuid), "plugins")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "big.yml"), make([]byte, 500), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("big.yml", filepath.Join(dir, "cfg.yml")); err != nil {
				t.Fatal(err)
			}
			rdb.Set(context.Background(), diskGaugeKey(uuid), `{"total":1000,"limit":1000}`, 10*time.Minute)
			t.Cleanup(func() {
				unmeasured.Lock()
				delete(unmeasured.m, uuid)
				delete(unmeasured.last, uuid)
				unmeasured.Unlock()
			})

			if err := tc.write(t, sm, rdb); err != nil {
				t.Fatalf("an overwrite that frees space at the disk limit was refused: %v", err)
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "big.yml")); string(b) != "small" {
				t.Errorf("big.yml holds %q", b)
			}
			if got := gaugeTotal(t, rdb, uuid); got > 1000 {
				t.Errorf("gauge after a shrinking overwrite = %d, want at most 1000", got)
			}
		})
	}
}
