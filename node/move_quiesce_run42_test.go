package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dylaris-pkg/fileperms"
	nodepb "dylaris-proto/node"

	"github.com/alicebob/miniredis/v2"
	"github.com/pkg/sftp"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"dylaris-pkg/migration"
	"dylaris-pkg/queue"
)

// R42-04/R42-05: migrate_out started the archive at the first instant no
// writer was open, while Beam and SFTP sessions still held write access, so a
// write that started a moment later - or the next file of a folder upload -
// threw the staged archive away after the server was already stopped. It now
// stages only once no write has begun for a quiet window, and refuses before
// staging when writes never stop.
func TestMigrateOutStagesOnlyAfterAQuietWindow(t *testing.T) {
	prevWait, prevQuiet := migrateOutWritersWait, migrateOutQuietWindow
	t.Cleanup(func() { migrateOutWritersWait, migrateOutQuietWindow = prevWait, prevQuiet })
	migrateOutWritersWait, migrateOutQuietWindow = 2*time.Second, 300*time.Millisecond
	ms := time.Millisecond
	tests := []struct {
		name string
		// holds are writes as [start, end] after the command arrives.
		holds [][2]time.Duration
		// endless takes and drops a writer every 50 ms for the whole run.
		endless bool
		wantErr string
	}{
		{"a write that starts after the command", [][2]time.Duration{{100 * ms, 150 * ms}}, false, ""},
		{"files one after another", [][2]time.Duration{{0, 100 * ms}, {150 * ms, 250 * ms}}, false, ""},
		{"writes that never stop", nil, true, "still being written"},
		{"no write", nil, false, ""},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, stagingTestUUID), 0o755); err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(root, stagingTestUUID, "level.dat"), []byte("w"), 0o644)
			storage := &StorageManager{paths: []string{root}}
			t0 := time.Now()
			var lastStart time.Duration
			for _, h := range tc.holds {
				lastStart = max(lastStart, h[0])
				d := h[1] - h[0]
				time.AfterFunc(h[0], func() { time.AfterFunc(d, storage.holdWriter(stagingTestUUID)) })
			}
			stop := make(chan struct{})
			var wg sync.WaitGroup
			if tc.endless {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						select {
						case <-stop:
							return
						case <-time.After(50 * ms):
							storage.holdWriter(stagingTestUUID)()
						}
					}
				}()
			}
			prev := migrateOutArchive
			t.Cleanup(func() { migrateOutArchive = prev })
			var archivedAt time.Duration
			archived := false
			migrateOutArchive = func(src, dst string) (string, int64, error) {
				archived, archivedAt = true, time.Since(t0)
				return migration.Archive(src, dst)
			}
			rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			progress := queue.MigrationProgressID(stagingTestUUID, string(rune('q'+i)))

			handleMigrateOut(context.Background(), rdb, storage, "tok", stagingTestUUID, progress, 0)
			close(stop)
			wg.Wait()

			st := readMigrationStatusMaybe(rdb, progress)
			if tc.wantErr != "" {
				if st.Phase != "error" || !strings.Contains(st.Error, tc.wantErr) {
					t.Fatalf("status %+v, want an error naming %q", st, tc.wantErr)
				}
				if archived {
					t.Fatal("staged while writes were still landing")
				}
				return
			}
			if st.Phase != "staged" {
				t.Fatalf("status %+v: want staged", st)
			}
			if len(tc.holds) > 0 && archivedAt < lastStart+migrateOutQuietWindow {
				t.Fatalf("archive started %v after the command, before the last write (%v) was %v quiet", archivedAt, lastStart, migrateOutQuietWindow)
			}
			if storage.writesWithdrawn(stagingTestUUID) {
				t.Fatal("write access is still withdrawn after the run ended")
			}
		})
	}
}

// R42-04/R42-06: while a move stages a server, every write path refuses at
// once - Beam on its cached session, SFTP on its unrepublished list, the
// panel's mesh writes and copies, and a panel upload already streaming - so
// none of them can take a writer hold the move then has to wait out or drop
// its archive for.
func TestWithdrawnWritesAreRefusedOnEveryPath(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T) (sm *StorageManager, uuid string, check func(t *testing.T))
	}{
		{"beam write", func(t *testing.T) (*StorageManager, string, func(*testing.T)) {
			bs, uuid, ctx := newTestBeamServer(t)
			return bs.storageMgr, uuid, func(t *testing.T) {
				if err := bs.requireFilePerm(ctx, canWrite, "write"); status.Code(err) != codes.FailedPrecondition {
					t.Errorf("write = %v, want FailedPrecondition", err)
				}
				if err := bs.requireFilePerm(ctx, canDelete, "delete"); status.Code(err) != codes.FailedPrecondition {
					t.Errorf("delete = %v, want FailedPrecondition", err)
				}
				if err := bs.requireFilePerm(ctx, canRead, "read"); err != nil {
					t.Errorf("read = %v, want allowed", err)
				}
			}
		}},
		{"sftp new and open handle", func(t *testing.T) (*StorageManager, string, func(*testing.T)) {
			sm := NewStorageManager(t.TempDir(), nil)
			const uuid = "42044204-4204-4204-4204-420442044204"
			os.MkdirAll(sm.GetServerDir(uuid), 0o755)
			v := newVirtualFS([]sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}, sm, nil, "alice")
			open, err := v.Filewrite(&sftp.Request{Method: "Put", Filepath: "s/a.txt", Flags: 0x02 | 0x08})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { open.(io.Closer).Close() })
			return sm, uuid, func(t *testing.T) {
				if _, err := v.Filewrite(&sftp.Request{Method: "Put", Filepath: "s/b.txt", Flags: 0x02 | 0x08}); err == nil {
					t.Error("a new SFTP write was opened")
				}
				if err := v.Filecmd(&sftp.Request{Method: "Mkdir", Filepath: "s/d"}); err == nil {
					t.Error("an SFTP mkdir ran")
				}
				if _, err := open.WriteAt([]byte("x"), 0); err == nil {
					t.Error("an open SFTP handle wrote")
				}
			}
		}},
		{"mesh write chunk and copy", func(t *testing.T) (*StorageManager, string, func(*testing.T)) {
			withDiskBudget(t, 1000, 1000)
			m, uuid := meshForBudget(t)
			cc := &coreConnection{stream: &fakeCoreStream{}}
			m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "up", ServerUuid: uuid, Payload: &nodepb.NodeMessage_WriteReq{WriteReq: &nodepb.WriteFileReq{Path: "survival/up.bin", TotalSize: 8}}})
			if _, ok := m.pendingWrites["up"]; !ok {
				t.Fatal("the upload was not accepted before the move")
			}
			return m.handler.storageMgr, uuid, func(t *testing.T) {
				m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "up", Payload: &nodepb.NodeMessage_Chunk{Chunk: &nodepb.DataChunk{Data: []byte("abcd")}}})
				if _, ok := m.pendingWrites["up"]; ok {
					t.Error("an upload in progress kept writing")
				}
				m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "new", ServerUuid: uuid, Payload: &nodepb.NodeMessage_WriteReq{WriteReq: &nodepb.WriteFileReq{Path: "survival/new.bin", TotalSize: 8}}})
				if _, ok := m.pendingWrites["new"]; ok {
					t.Error("a new mesh upload was accepted")
				}
				reply := m.handler.Handle(&nodepb.NodeMessage{RequestId: "cp", ServerUuid: uuid, Payload: &nodepb.NodeMessage_CopyReq{CopyReq: &nodepb.CopyFileReq{SrcPath: "survival/server.properties", DstPath: "survival/copy.properties"}}})
				if len(reply) != 1 || reply[0].GetError().GetCode() != 409 {
					t.Errorf("copy = %v, want a 409", reply)
				}
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sm, uuid, check := tc.run(t)
			restore := sm.withdrawWrites(uuid)
			check(t)
			restore()
			sm.movingMu.Lock()
			n := sm.writers[uuid]
			sm.movingMu.Unlock()
			if n != 0 {
				t.Errorf("%d writer hold(s) left after the refusals", n)
			}
		})
	}
}
