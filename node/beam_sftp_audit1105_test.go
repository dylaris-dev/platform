package main

import (
	"context"
	"crypto/rand"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dylaris-pkg/beam/quota"
	"dylaris-pkg/fileperms"
	pb "dylaris-proto/beam"

	"github.com/alicebob/miniredis/v2"
	"github.com/pkg/sftp"
	"github.com/redis/go-redis/v9"
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

// revokingDownloadStream ends the Beam session after the first chunk is sent.
type revokingDownloadStream struct {
	fakeBeamDownloadStream
	revoke func()
}

func (r *revokingDownloadStream) Send(c *pb.BeamChunk) error {
	err := r.fakeBeamDownloadStream.Send(c)
	if len(r.chunks) == 1 {
		r.revoke()
	}
	return err
}

// H31: a download checked the session once, at its start, so a revoked member
// received the rest of the file or zip (and the per-server data throttle,
// keyed on a session that no longer had a server, switched off).
func TestABeamDownloadStopsWhenItsAccessIsRevoked(t *testing.T) {
	for _, tc := range []struct {
		name string
		zip  bool
	}{
		{"file", false},
		{"folder as zip", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bs, serverUUID, ctx := newTestBeamServer(t)
			dir := filepath.Join(bs.storageMgr.GetServerDir(serverUUID), "world")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			body := make([]byte, 4*beamChunkSize)
			rand.Read(body)
			if err := os.WriteFile(filepath.Join(dir, "r.0.0.mca"), body, 0o644); err != nil {
				t.Fatal(err)
			}
			req := &pb.BeamDownloadReq{Path: "world/r.0.0.mca"}
			if tc.zip {
				req = &pb.BeamDownloadReq{Path: "world", ZipIfDir: true}
			}
			st := &revokingDownloadStream{
				fakeBeamDownloadStream: fakeBeamDownloadStream{ctx: ctx},
				revoke: func() {
					sess := &beamSession{opened: time.Now()}
					sess.ended.Store(true)
					bs.sessionByPeer.Store("127.0.0.1:40000", sess)
				},
			}
			err := bs.DownloadFile(req, st)
			if status.Code(err) != codes.PermissionDenied {
				t.Errorf("DownloadFile = %v, want PermissionDenied", err)
			}
			if len(st.chunks) != 1 {
				t.Errorf("%d chunks sent, want only the one before the revocation", len(st.chunks))
			}
		})
	}
}

// H30: the temp claim was keyed on the temp's leaf name, which is the same in
// every folder, so the same id in a second folder took over the first
// folder's live stream and left its bytes uncharged.
func TestTheSameUploadIDInTwoFoldersIsTwoUploads(t *testing.T) {
	bs, serverUUID, ctx := newTestBeamServer(t)
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("x-beam-upload-id", "X"))
	var second *fakeBeamUploadStream
	var secondErr error
	a := &hookedUploadStream{
		fakeBeamUploadStream: fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("dirA", "a.bin", 8, []byte("abcd"), []byte("efgh"))},
		before: func(i int) {
			if i == 2 {
				second = &fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("dirB", "b.bin", 2, []byte("ok"))}
				secondErr = bs.UploadFile(second)
			}
		},
	}
	if err := bs.UploadFile(a); err != nil || a.resp == nil || !a.resp.Success {
		t.Errorf("the upload in dirA: err=%v resp=%+v, want it to finish", err, a.resp)
	}
	if secondErr != nil || second.resp == nil || !second.resp.Success {
		t.Errorf("the upload in dirB: err=%v resp=%+v", secondErr, second.resp)
	}
	got, err := os.ReadFile(filepath.Join(bs.storageMgr.GetServerDir(serverUUID), "dirA", "a.bin"))
	if err != nil || string(got) != "abcdefgh" {
		t.Errorf("dirA landed %q, %v", got, err)
	}
}

// H33: a resume whose temp is gone (swept, or chunks lost in flight) wrote
// at the client's offset into a new empty temp, and the zero-filled head was
// renamed into place as a success.
func TestAResumePastWhatTheTempHoldsIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		have   []byte // temp content before the resume, nil = none
		offset int64
		want   codes.Code
	}{
		{"temp swept", nil, 4, codes.FailedPrecondition},
		{"temp short", []byte("ab"), 4, codes.FailedPrecondition},
		{"resume at its end", []byte("abcd"), 4, codes.OK},
		{"resend inside it", []byte("abcd"), 2, codes.OK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bs, serverUUID, ctx := newTestBeamServer(t)
			ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("x-beam-upload-id", "R"))
			dir := filepath.Join(bs.storageMgr.GetServerDir(serverUUID), "survival")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.have != nil {
				if err := os.WriteFile(filepath.Join(dir, ".beam-upload-R"), tc.have, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			tail := []byte("abcdefgh")[tc.offset:]
			msgs := uploadMsgs("survival", "w.zip", 8)
			msgs = append(msgs, &pb.BeamUploadMsg{Payload: &pb.BeamUploadMsg_Chunk{Chunk: &pb.BeamUploadChunkData{Data: tail, Offset: tc.offset}}})
			st := &fakeBeamUploadStream{ctx: ctx, msgs: msgs}
			err := bs.UploadFile(st)
			if status.Code(err) != tc.want {
				t.Fatalf("UploadFile = %v, want %v", err, tc.want)
			}
			got, rerr := os.ReadFile(filepath.Join(dir, "w.zip"))
			if tc.want == codes.OK && (rerr != nil || string(got) != "abcdefgh") {
				t.Errorf("landed %q, %v", got, rerr)
			}
			if tc.want != codes.OK && rerr == nil {
				t.Errorf("a file with a hole landed: %q", got)
			}
		})
	}
}

// H36: a temp no Beam stream left (copied or planted under the temp's name)
// was taken as already charged, so streaming over it booked nothing.
func TestAPlantedUploadTempIsNotTakenAsCharged(t *testing.T) {
	bs, serverUUID, ctx := newTestBeamServer(t)
	mr := miniredis.RunT(t)
	bs.rdb = redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { bs.rdb.Close() })
	mr.Set(quota.DailyUploadBytesKey, "100")
	p, _ := peer.FromContext(ctx)
	bs.usernameByPeer.Store(p.Addr.String(), "carol")
	dir := filepath.Join(bs.storageMgr.GetServerDir(serverUUID), "survival")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beam-upload-P"), make([]byte, 8), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("x-beam-upload-id", "P"))
	st := &fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("survival", "w.zip", 8, []byte("abcdefgh"))}
	if err := bs.UploadFile(st); err != nil {
		t.Fatal(err)
	}
	if used, _ := bs.rdb.Get(context.Background(), quota.DailyKey("carol", time.Now())).Int64(); used != 8 {
		t.Errorf("daily usage = %d, want the 8 bytes streamed", used)
	}
}

// H34: panel (mesh) uploads reserved on one per-server counter, Beam and SFTP
// on another, so a panel and a Beam upload at once each had the whole
// headroom.
func TestMeshAndBeamUploadsShareTheServerHeadroom(t *testing.T) {
	_, rdb := gaugeRedis(t)
	bs, uuid, ctx := newTestBeamServer(t)
	bs.rdb = rdb
	bs.storageMgr = NewStorageManager(t.TempDir(), rdb)
	dir := bs.storageMgr.GetServerDir(uuid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		unmeasured.Lock()
		delete(unmeasured.m, uuid)
		delete(unmeasured.last, uuid)
		unmeasured.Unlock()
	})

	rdb.Set(ctx, diskGaugeKey(uuid), `{"total":0,"limit":100}`, 10*time.Minute)
	t.Run("mesh first", func(t *testing.T) {
		b, ok := uploadBudget(rdb, dir, uuid, 70)
		defer b.release()
		if !ok {
			t.Fatal("the mesh upload alone was refused")
		}
		st := &fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("", "d.bin", 60, make([]byte, 60))}
		if err := bs.UploadFile(st); status.Code(err) != codes.ResourceExhausted {
			t.Errorf("a Beam upload past a running panel upload: %v, want ResourceExhausted", err)
		}
	})
	rdb.Set(ctx, diskGaugeKey(uuid), `{"total":0,"limit":100}`, 10*time.Minute)
	t.Run("beam first", func(t *testing.T) {
		var meshOK bool
		st := &hookedUploadStream{
			fakeBeamUploadStream: fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("", "e.bin", 70, make([]byte, 70))},
			before: func(i int) {
				if i == 1 {
					var b *writeBudget
					b, meshOK = uploadBudget(rdb, dir, uuid, 60)
					b.release()
				}
			},
		}
		bs.UploadFile(st)
		if meshOK {
			t.Error("a panel upload used headroom a running Beam upload had reserved")
		}
	})
}
