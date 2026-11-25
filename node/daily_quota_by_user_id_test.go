package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	beamauth "dylaris-pkg/beam/auth"
	"dylaris-pkg/beam/quota"
	pb "dylaris-proto/beam"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const quotaTestUserID = "3f1c2a9e-0b7d-4e21-9c5a-6d8e7f901234"

// R34-09: the daily upload bucket was keyed by username, so alice spent her
// day, renamed herself and had a fresh one. A ticket that carries the user id
// books to the id, which a rename does not change. A ticket from a Core older
// than the id still books by name - the rollout fallback, not the fix.
func TestBeamDailyQuotaSurvivesARename(t *testing.T) {
	cases := []struct {
		name       string
		userID     string
		wantCode   codes.Code
		wantBucket quota.Bucket
		wantBooked int64
	}{
		{"a ticket with the id sees the day spent under the old name", quotaTestUserID, codes.ResourceExhausted,
			quota.BucketFor(quotaTestUserID, "alice"), 100},
		{"a ticket from an older Core falls back to the name", "", codes.OK,
			quota.BucketFor("", "alice2"), 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bs, _, ctx := newTestBeamServer(t)
			mr := miniredis.RunT(t)
			bs.rdb = redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { bs.rdb.Close() })
			mr.Set(quota.DailyUploadBytesKey, "150")
			// Spent before the rename, under the id.
			mr.Set(quota.DailyKey(quota.BucketFor(quotaTestUserID, "alice"), time.Now()), "100")

			p, _ := peer.FromContext(ctx)
			sess := &beamSession{claims: &beamauth.BeamClaims{UserID: tc.userID, Username: "alice2"}, opened: time.Now()}
			sess.checked.Store(time.Now().UnixNano())
			bs.sessionByPeer.Store(p.Addr.String(), sess)
			bs.usernameByPeer.Store(p.Addr.String(), "alice2")

			stream := &fakeBeamUploadStream{ctx: ctx, msgs: []*pb.BeamUploadMsg{
				{Payload: &pb.BeamUploadMsg_Start{Start: &pb.BeamUploadStart{Path: "survival", Filename: "w.zip", TotalSize: 100}}},
				{Payload: &pb.BeamUploadMsg_Chunk{Chunk: &pb.BeamUploadChunkData{Data: bytes.Repeat([]byte("x"), 100)}}},
			}}
			err := bs.UploadFile(stream)
			if status.Code(err) != tc.wantCode {
				t.Fatalf("upload: err = %v, want code %v", err, tc.wantCode)
			}
			if got := bs.extractQuotaBucket(ctx); got != tc.wantBucket {
				t.Errorf("bucket = %q, want %q", got, tc.wantBucket)
			}
			if used, _ := bs.rdb.Get(context.Background(), quota.DailyKey(tc.wantBucket, time.Now())).Int64(); used != tc.wantBooked {
				t.Errorf("booked %d to %s, want %d", used, tc.wantBucket, tc.wantBooked)
			}
		})
	}
}

// The SFTP side learns the id from the server list Core publishes per user.
// The field name is the wire contract with core/services/sftp_sync.go.
func TestSFTPQuotaBucketComesFromThePublishedUserID(t *testing.T) {
	cases := []struct {
		name string
		list string
		want quota.Bucket
	}{
		{"a list from a Core that sends the id", `[{"uuid":"s1","name":"A","user_id":"` + quotaTestUserID + `"}]`,
			quota.BucketFor(quotaTestUserID, "")},
		{"a list from an older Core", `[{"uuid":"s1","name":"A"}]`, quota.BucketFor("", "alice2")},
		{"no servers at all", `[]`, quota.BucketFor("", "alice2")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var servers []sftpServerRef
			if err := json.Unmarshal([]byte(tc.list), &servers); err != nil {
				t.Fatalf("decode: %v", err)
			}
			v := newVirtualFS(servers, nil, nil, "alice2")
			if got := v.quotaBucket(); got != tc.want {
				t.Errorf("bucket = %q, want %q", got, tc.want)
			}
		})
	}
}
