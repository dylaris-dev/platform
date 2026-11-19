package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dylaris-pkg/fileperms"
	pb "dylaris-proto/beam"

	"github.com/alicebob/miniredis/v2"
	"github.com/pkg/sftp"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// firstCmdHook runs fn once, before the first Redis command the client sends.
type firstCmdHook struct {
	fn   func()
	done bool
}

func (h *firstCmdHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *firstCmdHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *firstCmdHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if !h.done {
			h.done = true
			h.fn()
		}
		return next(ctx, cmd)
	}
}

// R41-05: a republish that withdrew write access after Filewrite's permission
// check but before the handle was registered walked a registry without it, so
// the new handle kept its move hold until the next recheck.
func TestAnSFTPHandleOpenedDuringARepublishDropsItsHold(t *testing.T) {
	const uuid = "41054105-4105-4105-4105-410541054105"
	sm, paths := newPlacementManager(t, 1)
	if err := os.MkdirAll(filepath.Join(paths[0], uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { rdb.Close() })
	v := newVirtualFS([]sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}, sm, rdb, "alice")
	// The limits are read from Redis between the permission check and the
	// registration: the republish lands exactly in that window.
	rdb.AddHook(&firstCmdHook{fn: func() {
		republish(v.set, v.open, []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Perms{Read: true}}})
	}})
	w, err := v.Filewrite(&sftp.Request{Method: "Put", Filepath: "s/cfg.yml", Flags: 0x02 | 0x08})
	if err != nil {
		t.Fatal(err)
	}
	defer w.(io.Closer).Close()
	if _, err := sm.watchWrites(uuid); err != nil {
		t.Fatalf("a handle opened while its write access was withdrawn still holds the server: %v", err)
	}
	if _, err := w.WriteAt([]byte("x"), 0); err == nil {
		t.Fatal("a handle whose hold was ended wrote")
	}
}

// stalledUploadStream delivers its messages, then blocks in Recv like a client
// that vanished mid-upload, until release is closed.
type stalledUploadStream struct {
	fakeBeamUploadStream
	afterLast func()
	release   chan struct{}
}

func (s *stalledUploadStream) Recv() (*pb.BeamUploadMsg, error) {
	if s.idx < len(s.msgs) {
		return s.fakeBeamUploadStream.Recv()
	}
	if s.afterLast != nil {
		s.afterLast()
		s.afterLast = nil
	}
	<-s.release
	return nil, io.ErrUnexpectedEOF
}

// R41-06: the access recheck of a Beam upload ran only when a message
// arrived, so a stalled stream kept its writer hold and migrate_out refused
// with writers in progress after the server was already stopped.
func TestAStalledBeamUploadEndsWhenItsAccessGoes(t *testing.T) {
	prev := beamRecheckEvery
	beamRecheckEvery = 20 * time.Millisecond
	t.Cleanup(func() { beamRecheckEvery = prev })
	bs, serverUUID, ctx := newTestBeamServer(t)
	addr := "127.0.0.1:40000"
	stream := &stalledUploadStream{
		fakeBeamUploadStream: fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("survival", "big.zip", 8, []byte("abcd"))},
		release:              make(chan struct{}),
		afterLast: func() {
			sess := &beamSession{opened: time.Now()}
			sess.ended.Store(true)
			bs.sessionByPeer.Store(addr, sess)
		},
	}
	t.Cleanup(func() { close(stream.release) })
	done := make(chan error, 1)
	go func() { done <- bs.UploadFile(stream) }()
	select {
	case err := <-done:
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("UploadFile = %v, want PermissionDenied", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a stalled upload whose access went is still running")
	}
	if _, err := bs.storageMgr.watchWrites(serverUUID); err != nil {
		t.Fatalf("the ended upload still holds the server: %v", err)
	}
}
