package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	pb "dylaris-proto/node"

	"github.com/docker/docker/client"
)

// V03: a listing, hash, backup list/usage or orphan inspect during a storage
// move took one of the server's four file slots, then waited inside the path
// lookup for the whole move. Core gave up after 30s, and once four were stuck
// every later request for that server, the writers' immediate 409 included,
// waited 20s for a slot and was answered "busy". Downloads waited the same way
// in their own slot. They are refused at once now, as the writers are.
func TestReadsDuringAStorageMoveAreRefusedWithoutWaiting(t *testing.T) {
	h, uuid, _ := seedNodeOwned(t)
	release, err := h.storageMgr.fenceMove(uuid)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	handled := []struct {
		name string
		msg  *pb.NodeMessage
	}{
		{"list", &pb.NodeMessage{Payload: &pb.NodeMessage_ListReq{ListReq: &pb.ListFilesReq{Path: "survival"}}}},
		{"hash", &pb.NodeMessage{Payload: &pb.NodeMessage_HashFilesReq{HashFilesReq: &pb.HashFilesReq{Path: "survival/plugins", Names: []string{"ok.jar"}}}}},
		{"backup list", &pb.NodeMessage{Payload: &pb.NodeMessage_BackupListReq{BackupListReq: &pb.BackupListReq{}}}},
		{"backup usage", &pb.NodeMessage{Payload: &pb.NodeMessage_BackupUsageReq{BackupUsageReq: &pb.BackupUsageReq{}}}},
		{"inspect orphan", &pb.NodeMessage{Payload: &pb.NodeMessage_InspectOrphanReq{InspectOrphanReq: &pb.InspectOrphanReq{}}}},
	}
	for _, op := range handled {
		t.Run(op.name, func(t *testing.T) {
			op.msg.RequestId, op.msg.ServerUuid = op.name, uuid
			got := make(chan []*pb.NodeMessage, 1)
			go func() { got <- h.Handle(op.msg) }()
			select {
			case reply := <-got:
				if len(reply) != 1 || reply[0].GetError().GetCode() != 409 {
					t.Fatalf("got %v, want one 409", reply)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the request waited for the storage move")
			}
		})
	}

	streamed := []struct {
		name string
		msg  *pb.NodeMessage
	}{
		{"read", &pb.NodeMessage{Payload: &pb.NodeMessage_ReadReq{ReadReq: &pb.ReadFileReq{Path: "survival/server.properties"}}}},
		{"selective read", &pb.NodeMessage{Payload: &pb.NodeMessage_SelectiveReadReq{SelectiveReadReq: &pb.SelectiveReadReq{Selected: []string{"survival"}}}}},
		{"backup open", &pb.NodeMessage{Payload: &pb.NodeMessage_BackupOpenReq{BackupOpenReq: &pb.BackupOpenReq{Key: "a.tar.gz"}}}},
	}
	for _, op := range streamed {
		t.Run(op.name, func(t *testing.T) {
			op.msg.RequestId = op.name
			got := make(chan int32, 1)
			go func() { _, code := readVia(h, uuid, op.msg); got <- code }()
			select {
			case code := <-got:
				if code != 409 {
					t.Fatalf("got error code %d, want 409", code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the download waited for the storage move")
			}
		})
	}
}

// V14: the collector's inspect of a gateway-routed server (no port binding)
// read the saved container port through the path lookup, which waits out a
// storage move. The collector sat parked for the whole move, blind to its
// context, and woke later against the server's NEW container.
func TestTheCollectorDoesNotWaitOutAStorageMove(t *testing.T) {
	_, uuid, _ := seedNodeOwned(t)
	sm := NewStorageManager(t.TempDir(), nil)
	unfence, err := sm.fenceMove(uuid)
	if err != nil {
		t.Fatal(err)
	}
	release := sync.OnceFunc(unfence)
	defer release()
	prev := globalStorageMgr
	globalStorageMgr = sm
	t.Cleanup(func() { globalStorageMgr = prev })

	docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/containers/mc_"+uuid+"/json") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"Id":"c1","State":{"Running":false,"StartedAt":"2026-11-18T10:00:00Z"},"HostConfig":{"NanoCPUs":2000000000},"Config":{"WorkingDir":"/data/survival"},"NetworkSettings":{}}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(docker.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(docker.URL, "http://")), client.WithVersion("1.44"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	dm := &DockerManager{cli: cli, ctx: t.Context(), storageMgr: sm}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		collectForContainer(ctx, newMiniRedis(t), dm, uuid, "mc_"+uuid, 10, nil, &containerSnapshot{})
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		release()
		<-done
		t.Fatal("the collector waited for the storage move instead of seeing its context end")
	}
}
