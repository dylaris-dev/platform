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

type beamSaveSink struct {
	pb.UnimplementedBeamNodeServiceServer
}

func (beamSaveSink) SaveFileContent(context.Context, *pb.BeamFileSaveReq) (*pb.BeamOpResp, error) {
	return &pb.BeamOpResp{Success: true}, nil
}

// A save is one unary message. ReadFileContent serves files up to
// maxBeamReadContent, so a save of a file that size must reach the handler
// rather than be refused by gRPC's 4 MiB default.
func TestBeamServerAcceptsASaveAsLargeAsARead(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(beamServerOptions(&beamServer{})...)
	pb.RegisterBeamNodeServiceServer(srv, beamSaveSink{})
	go srv.Serve(lis)
	defer srv.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	resp, err := pb.NewBeamNodeServiceClient(conn).SaveFileContent(context.Background(),
		&pb.BeamFileSaveReq{Path: "big.json", Content: strings.Repeat("a", maxBeamReadContent)})
	if err != nil {
		t.Fatalf("save of %d bytes refused: %v", maxBeamReadContent, err)
	}
	if !resp.Success {
		t.Fatalf("save failed: %s", resp.Message)
	}
}
