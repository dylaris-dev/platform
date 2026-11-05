package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"dylaris-pkg/beam/quota"
	pb "dylaris-proto/beam"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// A resumable upload that drops before EOF keeps its temp on disk. It used to
// release its reservation and note nothing, so the next upload under a fresh
// id saw the whole headroom and daily quota again, as often as it liked.
func TestAnAbandonedResumableBeamUploadStillCounts(t *testing.T) {
	const size = 100
	upload := func(id string, total int64, data []byte, dropped bool) *fakeBeamUploadStream {
		msgs := []*pb.BeamUploadMsg{{Payload: &pb.BeamUploadMsg_Start{Start: &pb.BeamUploadStart{
			Path: "survival", Filename: "w-" + id + ".zip", TotalSize: total,
		}}}}
		if data != nil {
			msgs = append(msgs, &pb.BeamUploadMsg{Payload: &pb.BeamUploadMsg_Chunk{Chunk: &pb.BeamUploadChunkData{Data: data}}})
		}
		f := &fakeBeamUploadStream{msgs: msgs}
		if dropped {
			f.end = errors.New("connection reset")
		}
		return f
	}
	cases := []struct {
		name      string
		gauge     string // stats:disk JSON, "" for none
		daily     string // beam:daily_upload_bytes, "" for none
		second    *fakeBeamUploadStream
		secondID  string
		wantCode  codes.Code
		wantDaily int64
	}{
		{"a fresh id is refused once the abandoned temp fills the disk limit",
			`{"total":0,"limit":150}`, "", upload("b", size, nil, false), "b", codes.ResourceExhausted, size},
		{"a fresh id is refused once the abandoned temp used the daily quota",
			"", "150", upload("b", size, nil, false), "b", codes.ResourceExhausted, size},
		{"resuming the same id is not charged twice",
			`{"total":0,"limit":150}`, "150", upload("a", size, nil, false), "a", codes.OK, size},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bs, uuid, ctx := newTestBeamServer(t)
			mr := miniredis.RunT(t)
			bs.rdb = redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { bs.rdb.Close() })
			if tc.gauge != "" {
				bs.rdb.Set(context.Background(), diskGaugeKey(uuid), tc.gauge, 10*time.Minute)
			}
			if tc.daily != "" {
				mr.Set(quota.DailyUploadBytesKey, tc.daily)
			}
			p, _ := peer.FromContext(ctx)
			bs.usernameByPeer.Store(p.Addr.String(), "carol")
			withID := func(id string) context.Context {
				return metadata.NewIncomingContext(ctx, metadata.Pairs("x-beam-upload-id", id))
			}

			first := upload("a", size, bytes.Repeat([]byte("x"), size), true)
			first.ctx = withID("a")
			if err := bs.UploadFile(first); err == nil {
				t.Fatal("the dropped stream reported success")
			}

			tc.second.ctx = withID(tc.secondID)
			err := bs.UploadFile(tc.second)
			if status.Code(err) != tc.wantCode {
				t.Fatalf("second upload: err = %v, want code %v", err, tc.wantCode)
			}
			if used, _ := bs.rdb.Get(context.Background(), quota.DailyKey("carol", time.Now())).Int64(); used != tc.wantDaily {
				t.Errorf("daily usage = %d, want %d", used, tc.wantDaily)
			}
		})
	}
}

// The beam upload never asked how much the NODE has left, only the server's
// own limit, so a server without one could fill the disk for every tenant.
func TestABeamUploadIsBoundByTheNodesFreeSpace(t *testing.T) {
	prev := restoreDiskBudget
	restoreDiskBudget = func(string) int64 { return 50 }
	t.Cleanup(func() { restoreDiskBudget = prev })
	bs, _, ctx := newTestBeamServer(t)
	stream := &fakeBeamUploadStream{ctx: ctx, msgs: []*pb.BeamUploadMsg{
		{Payload: &pb.BeamUploadMsg_Start{Start: &pb.BeamUploadStart{Path: "survival", Filename: "w.zip", TotalSize: 100}}},
	}}
	if err := bs.UploadFile(stream); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("err = %v, want ResourceExhausted", err)
	}
	if n := inflightWrites.Load(); n != 0 {
		t.Fatalf("the refused upload left %d bytes claimed", n)
	}
}
