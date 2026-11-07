package main

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	pb "dylaris-proto/beam"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// beamRawNames answers with names a disk can hold and proto3 cannot carry.
type beamRawNames struct {
	pb.UnimplementedBeamNodeServiceServer
}

func (beamRawNames) ListFiles(context.Context, *pb.BeamFileListReq) (*pb.BeamFileListResp, error) {
	return &pb.BeamFileListResp{Files: []*pb.BeamFileInfo{{Name: "caf\xe9.txt"}, {Name: "ok.txt"}}}, nil
}

func (beamRawNames) DeleteFile(context.Context, *pb.BeamFileDeleteReq) (*pb.BeamOpResp, error) {
	return &pb.BeamOpResp{Message: "remove caf\xe9.txt: permission denied"}, nil
}

func (beamRawNames) DownloadFile(_ *pb.BeamDownloadReq, s grpc.ServerStreamingServer[pb.BeamChunk]) error {
	return s.Send(&pb.BeamChunk{Data: []byte{0xe9, 0xff}, Filename: "caf\xe9.txt"})
}

// Q11: one non-UTF-8 file name failed the whole reply with "string field
// contains invalid UTF-8": the folder could not be listed in Beam, the
// download died on its first chunk, and an error quoting the path hid itself.
// Every reply is repaired on its way out, as the node does for Core.
func TestBeamRepliesCarryNonUTF8Names(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(beamServerOptions(&beamServer{})...)
	pb.RegisterBeamNodeServiceServer(srv, beamRawNames{})
	go srv.Serve(lis)
	defer srv.Stop()
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := pb.NewBeamNodeServiceClient(conn)
	ctx := context.Background()

	list, err := c.ListFiles(ctx, &pb.BeamFileListReq{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Files) != 2 || list.Files[0].Name != "caf�.txt" || list.Files[1].Name != "ok.txt" {
		t.Errorf("list: got %v", list.Files)
	}

	op, err := c.DeleteFile(ctx, &pb.BeamFileDeleteReq{})
	if err != nil || op.Message != "remove caf�.txt: permission denied" {
		t.Errorf("delete: %v, %v", op, err)
	}

	dl, err := c.DownloadFile(ctx, &pb.BeamDownloadReq{})
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := dl.Recv()
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if chunk.Filename != "caf�.txt" || string(chunk.Data) != "\xe9\xff" {
		t.Errorf("download: filename %q data %x; the bytes must stay as they are", chunk.Filename, chunk.Data)
	}
	if _, err := dl.Recv(); err != io.EOF {
		t.Errorf("download end: %v", err)
	}
}

// Q11: the editor saves what it was given. A Latin-1 or binary file opened as
// repaired text would be written back with the replaced bytes, so it is not
// opened at all.
func TestBeamWillNotOpenNonUTF8Content(t *testing.T) {
	bs, uuid, ctx := newTestBeamServer(t)
	dir := bs.storageMgr.GetServerDir(uuid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"latin1.properties", "motd=caf\xe9", false},
		{"utf8.properties", "motd=café", true},
	} {
		if err := os.WriteFile(filepath.Join(dir, tc.name), []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		resp, err := bs.ReadFileContent(ctx, &pb.BeamFileReadReq{Path: tc.name})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if resp.Success != tc.ok || (tc.ok && resp.Content != tc.body) || (!tc.ok && (resp.Content != "" || resp.Message == "")) {
			t.Errorf("%s: success=%v content=%q message=%q", tc.name, resp.Success, resp.Content, resp.Message)
		}
	}
}
