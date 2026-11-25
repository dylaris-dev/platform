package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	beamauth "dylaris-pkg/beam/auth"
	"dylaris-pkg/queue"
	pb "dylaris-proto/beam"

	"google.golang.org/grpc/peer"
)

// R53-04: a folder or selective download's size estimate summed the node-local
// backup store and the node's own files, which the zip itself leaves out, so a
// member without backups.read learned the size of every backup on the node.
func TestADownloadEstimateLeavesOutWhatTheZipLeavesOut(t *testing.T) {
	bs, serverUUID, ctx := newTestBeamServer(t)
	root := seedTree(t, bs, serverUUID) // 7 + 7 + 3 visible bytes
	if err := os.MkdirAll(filepath.Join(root, backupDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, backupDirName, "b.zip"), make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".node_config.json"), make([]byte, 500), 0o644); err != nil {
		t.Fatal(err)
	}
	linked := os.Symlink(filepath.Join("..", backupDirName), filepath.Join(root, "plugins", "b")) == nil

	for _, tc := range []struct {
		name string
		run  func(st *fakeBeamDownloadStream) error
		want int64
		link bool
	}{
		{"selected backup store", func(st *fakeBeamDownloadStream) error {
			return bs.DownloadSelective(&pb.BeamSelectiveReq{Selected: []string{backupDirName}}, st)
		}, 0, false},
		{"selected node config", func(st *fakeBeamDownloadStream) error {
			return bs.DownloadSelective(&pb.BeamSelectiveReq{Selected: []string{".node_config.json", "server.properties"}}, st)
		}, 7, false},
		{"select all at the root", func(st *fakeBeamDownloadStream) error {
			return bs.DownloadSelective(&pb.BeamSelectiveReq{SelectAll: true}, st)
		}, 17, false},
		{"the whole server as a folder", func(st *fakeBeamDownloadStream) error {
			return bs.DownloadFile(&pb.BeamDownloadReq{Path: "", ZipIfDir: true}, st)
		}, 17, false},
		{"a link to the backup store", func(st *fakeBeamDownloadStream) error {
			return bs.DownloadFile(&pb.BeamDownloadReq{Path: "plugins/b", ZipIfDir: true}, st)
		}, 0, true},
	} {
		if tc.link && !linked {
			t.Logf("%s: skipped, no symlinks here", tc.name)
			continue
		}
		st := &fakeBeamDownloadStream{ctx: ctx}
		if err := tc.run(st); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(st.chunks) == 0 {
			t.Fatalf("%s: nothing was sent", tc.name)
		}
		if got := st.chunks[0].TotalSize; got != tc.want {
			t.Errorf("%s: TotalSize = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// R53-05: a Beam audit record named the actor by username only, so after a
// rename Core pinned the session's changes on whoever took the old name. It
// carries the ticket's user id now; a ticket from a Core older than the id
// still logs, by name.
func TestABeamAuditRecordCarriesTheTicketsUserID(t *testing.T) {
	for _, tc := range []struct {
		name, userID string
	}{
		{"ticket with the id", "6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b"},
		{"ticket from an older Core", ""},
	} {
		_, rdb := gaugeRedis(t)
		sub := rdb.Subscribe(context.Background(), queue.SFTPAuditChannel(nodeID))
		if _, err := sub.Receive(context.Background()); err != nil {
			t.Fatal(err)
		}
		s := &beamServer{rdb: rdb}
		addr := &net.TCPAddr{IP: net.ParseIP("10.0.1.7"), Port: 43122}
		s.usernameByPeer.Store(addr.String(), "carol")
		s.sessionByPeer.Store(addr.String(), &beamSession{claims: &beamauth.BeamClaims{UserID: tc.userID}, opened: time.Now()})
		ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: addr})
		s.auditBeam(ctx, "srv", "write", "plugins/x.yml")
		select {
		case msg := <-sub.Channel():
			var rec queue.SFTPAuditRecord
			if err := json.Unmarshal([]byte(msg.Payload), &rec); err != nil {
				t.Fatal(err)
			}
			if rec.UserID != tc.userID || rec.Username != "carol" {
				t.Errorf("%s: record = %+v, want user id %q and username carol", tc.name, rec, tc.userID)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: no audit record was published", tc.name)
		}
		sub.Close()
	}
}

// R53-05, SFTP side: the record takes the account id Core puts in the server
// list, the one the daily quota is booked to; none from an older Core.
func TestAnSFTPAuditRecordCarriesTheListsUserID(t *testing.T) {
	for _, tc := range []struct {
		name    string
		servers []sftpServerRef
		want    string
	}{
		{"list with the id", []sftpServerRef{{UUID: "srv", UserID: "6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b"}}, "6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b"},
		{"list from an older Core", []sftpServerRef{{UUID: "srv"}}, ""},
		{"empty list", nil, ""},
	} {
		a := newSFTPAudit("alice", listUserID(tc.servers), "203.0.113.5")
		a.note("srv", "write", "x.yml")
		recs := a.take()
		if len(recs) != 1 || recs[0].UserID != tc.want || recs[0].Username != "alice" {
			t.Errorf("%s: records = %+v, want user id %q", tc.name, recs, tc.want)
		}
	}
}
