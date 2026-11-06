package main

import (
	"context"
	"net"
	"strings"
	"testing"

	pb "dylaris-proto/beam"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const bigBeamFile = 10 << 20 // the node's maxBeamReadContent

type bigFileNode struct {
	pb.UnimplementedBeamNodeServiceServer
}

func (bigFileNode) ReadFileContent(context.Context, *pb.BeamFileReadReq) (*pb.BeamFileContentResp, error) {
	return &pb.BeamFileContentResp{Success: true, Content: strings.Repeat("a", bigBeamFile)}, nil
}

// The node returns files up to 10 MiB in one reply; the client must accept it
// instead of failing with gRPC's 4 MiB receive default.
func TestRelayClientReadsAFileAsLargeAsTheNodeServes(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterBeamNodeServiceServer(srv, bigFileNode{})
	go srv.Serve(lis)
	defer srv.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), beamCallLimits, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rc := &BeamNodeClient{conn: conn, svc: pb.NewBeamNodeServiceClient(conn)}
	res, err := rc.GetFileContent("latest.log")
	if err != nil {
		t.Fatalf("reading a %d byte file: %v", bigBeamFile, err)
	}
	if len(res.Content) != bigBeamFile {
		t.Fatalf("got %d bytes, want %d", len(res.Content), bigBeamFile)
	}
}
