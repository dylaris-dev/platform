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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// hookedUploadStream runs before(i) ahead of the i-th Recv, so a test can act
// while an upload is in the middle of its stream.
type hookedUploadStream struct {
	fakeBeamUploadStream
	before func(i int)
}

func (h *hookedUploadStream) Recv() (*pb.BeamUploadMsg, error) {
	if h.before != nil {
		h.before(h.fakeBeamUploadStream.idx)
	}
	return h.fakeBeamUploadStream.Recv()
}

func uploadMsgs(path, name string, total int64, chunks ...[]byte) []*pb.BeamUploadMsg {
	msgs := []*pb.BeamUploadMsg{{Payload: &pb.BeamUploadMsg_Start{Start: &pb.BeamUploadStart{Path: path, Filename: name, TotalSize: total}}}}
	var off int64
	for _, c := range chunks {
		msgs = append(msgs, &pb.BeamUploadMsg{Payload: &pb.BeamUploadMsg_Chunk{Chunk: &pb.BeamUploadChunkData{Data: c, Offset: off}}})
		off += int64(len(c))
	}
	return msgs
}

// G28: an upload stream checked the session once, at its start. Access revoked
// while it ran (member removed, password reset) still let it write and land
// the file.
func TestABeamUploadStopsWhenItsAccessIsRevoked(t *testing.T) {
	for _, tc := range []struct {
		name     string
		revokeAt int // Recv index before which the session ends
	}{
		{"between chunks", 2},
		{"before the end of the stream", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bs, serverUUID, ctx := newTestBeamServer(t)
			addr := "127.0.0.1:40000"
			stream := &hookedUploadStream{
				fakeBeamUploadStream: fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("survival", "x.jar", 8, []byte("abcd"), []byte("efgh"))},
				before: func(i int) {
					if i == tc.revokeAt {
						sess := &beamSession{opened: time.Now()}
						sess.ended.Store(true)
						bs.sessionByPeer.Store(addr, sess)
					}
				},
			}
			err := bs.UploadFile(stream)
			if status.Code(err) != codes.PermissionDenied {
				t.Errorf("UploadFile = %v, want PermissionDenied", err)
			}
			dest := filepath.Join(bs.storageMgr.GetServerDir(serverUUID), "survival", "x.jar")
			if _, err := os.Stat(dest); err == nil {
				t.Error("the file landed after access was revoked")
			}
			left, _ := filepath.Glob(filepath.Join(filepath.Dir(dest), ".beam-upload-*"))
			if len(left) != 0 {
				t.Errorf("temps left behind: %v", left)
			}
		})
	}
}

// G33: a second stream with the same upload id took over a temp a live stream
// was still writing, as if its bytes were charged, renamed it into place and
// was charged nothing for it. The newest stream owns the temp now and is
// charged for what nobody was; the one it replaced stops.
func TestABeamUploadTakeoverChargesTheBytesOnce(t *testing.T) {
	_, rdb := gaugeRedis(t)
	bs, serverUUID, ctx := newTestBeamServer(t)
	bs.rdb = rdb
	bs.storageMgr = NewStorageManager(t.TempDir(), rdb)
	rdb.Set(ctx, diskGaugeKey(serverUUID), `{"total":0,"limit":1000}`, 10*time.Minute)
	t.Cleanup(func() {
		unmeasured.Lock()
		delete(unmeasured.m, serverUUID)
		delete(unmeasured.last, serverUUID)
		unmeasured.Unlock()
	})
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("x-beam-upload-id", "X"))
	var second *fakeBeamUploadStream
	var secondErr error
	a := &hookedUploadStream{
		fakeBeamUploadStream: fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("survival", "p.jar", 8, []byte("abcdefgh"))},
		before: func(i int) {
			if i == 2 {
				second = &fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("survival", "p.jar", 0)}
				secondErr = bs.UploadFile(second)
			}
		},
	}
	if err := bs.UploadFile(a); status.Code(err) != codes.Aborted {
		t.Errorf("the replaced stream: %v, want Aborted", err)
	}
	if secondErr != nil || second.resp == nil || !second.resp.Success {
		t.Fatalf("the stream that took over: err=%v resp=%+v", secondErr, second.resp)
	}
	got, err := os.ReadFile(filepath.Join(bs.storageMgr.GetServerDir(serverUUID), "survival", "p.jar"))
	if err != nil || string(got) != "abcdefgh" {
		t.Errorf("landed %q, %v", got, err)
	}
	if total := gaugeTotal(t, rdb, serverUUID); total != 8 {
		t.Errorf("gauge = %d, want the 8 bytes counted once", total)
	}
	// Once every stream on it ended, the id starts afresh.
	c := &fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("survival", "p.jar", 2, []byte("ok"))}
	if err := bs.UploadFile(c); err != nil || c.resp == nil || !c.resp.Success {
		t.Errorf("a fresh upload under the id: err=%v resp=%+v", err, c.resp)
	}
	if n := len(bs.uploadTemps); n != 0 {
		t.Errorf("%d temp claims left after every stream ended", n)
	}
}

// G30: a Beam upload and an SFTP handle each counted only their own path's
// in-flight bytes, so both at once each had the whole headroom.
func TestBeamAndSFTPShareTheServerHeadroom(t *testing.T) {
	_, rdb := gaugeRedis(t)
	sm := NewStorageManager(t.TempDir(), rdb)
	bs := &beamServer{storageMgr: sm, rdb: rdb, throttle: NewBeamThrottle(context.Background(), nil)}
	const uuid = "11051105-1105-1105-1105-110511051105"
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 41105}
	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: addr})
	bs.serverUUIDByPeer.Store(addr.String(), uuid)
	full := fileperms.Full()
	bs.permsByPeer.Store(addr.String(), &full)
	if err := os.MkdirAll(sm.GetServerDir(uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	rdb.Set(ctx, diskGaugeKey(uuid), `{"total":0,"limit":100}`, 10*time.Minute)
	t.Cleanup(func() {
		unmeasured.Lock()
		delete(unmeasured.m, uuid)
		delete(unmeasured.last, uuid)
		unmeasured.Unlock()
	})
	refs := []sftpServerRef{{UUID: uuid, Name: "s", Perms: fileperms.Full()}}
	openSFTP := func(name string) io.WriterAt {
		w, err := newVirtualFS(refs, sm, rdb, "alice").Filewrite(&sftp.Request{Method: "Put", Filepath: "s/" + name, Flags: 0x02 | 0x08 | 0x10})
		if err != nil {
			t.Fatal(err)
		}
		return w
	}

	t.Run("beam first", func(t *testing.T) {
		var sftpErr error
		stream := &hookedUploadStream{
			fakeBeamUploadStream: fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("", "b.bin", 80, make([]byte, 80))},
			before: func(i int) {
				if i == 1 {
					w := openSFTP("a.bin")
					_, sftpErr = w.WriteAt(make([]byte, 60), 0)
					w.(io.Closer).Close()
				}
			},
		}
		bs.UploadFile(stream)
		if sftpErr == nil {
			t.Error("an SFTP write used headroom a running Beam upload had reserved")
		}
	})

	rdb.Set(ctx, diskGaugeKey(uuid), `{"total":0,"limit":100}`, 10*time.Minute)
	t.Run("sftp first", func(t *testing.T) {
		w := openSFTP("c.bin")
		defer w.(io.Closer).Close()
		if _, err := w.WriteAt(make([]byte, 60), 0); err != nil {
			t.Fatal(err)
		}
		stream := &fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("", "d.bin", 60, make([]byte, 60))}
		if err := bs.UploadFile(stream); status.Code(err) != codes.ResourceExhausted {
			t.Errorf("a Beam upload past what an open SFTP handle wrote: %v, want ResourceExhausted", err)
		}
	})
}
