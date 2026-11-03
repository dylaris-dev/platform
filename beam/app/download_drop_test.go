package main

import (
	"context"
	"net"
	"path/filepath"
	"testing"

	pb "dylaris-proto/beam"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type droppingNode struct {
	pb.UnimplementedBeamNodeServiceServer
	sent chan struct{}
	hold chan struct{}
}

func (d droppingNode) DownloadFile(_ *pb.BeamDownloadReq, s grpc.ServerStreamingServer[pb.BeamChunk]) error {
	if err := s.Send(&pb.BeamChunk{Data: make([]byte, 1024), TotalSize: 1 << 20}); err != nil {
		return err
	}
	close(d.sent)
	<-d.hold
	return nil
}

// The retry loop is only worth anything if a connection that dies mid-transfer
// comes back as an error it retries. Pinned against a real gRPC stream whose
// server goes away after the first chunk, not against a hand-made status.
func TestDroppedConnectionIsRetryable(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	node := droppingNode{sent: make(chan struct{}), hold: make(chan struct{})}
	srv := grpc.NewServer()
	pb.RegisterBeamNodeServiceServer(srv, node)
	go srv.Serve(lis)
	defer close(node.hold)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rc := &BeamNodeClient{conn: conn, svc: pb.NewBeamNodeServiceClient(conn)}

	go func() {
		<-node.sent
		srv.Stop() // closes every connection, like a node restart or a cable pull
	}()
	err = rc.DownloadFile(context.Background(), "world", true, filepath.Join(t.TempDir(), "w.zip"), nil)
	if err == nil {
		t.Fatal("download succeeded although the server went away")
	}
	if !retryableDownloadErr(err) {
		t.Fatalf("a dropped connection is not retried: %v", err)
	}
}
