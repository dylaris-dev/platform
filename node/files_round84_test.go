package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "dylaris-proto/node"
)

// seedNodeOwned is a server directory holding the node's own files next to a
// sub-server: its config (with the JVM flags a member without
// server.settings.write must not see) and a node-local backup.
func seedNodeOwned(t *testing.T) (*StreamHandler, string, string) {
	t.Helper()
	sm := NewStorageManager(t.TempDir(), nil)
	h := NewStreamHandler(sm)
	const uuid = "84848484-8484-8484-8484-848484848484"
	root := sm.GetServerDir(uuid)
	for name, body := range map[string]string{
		".node_config.json":               `{"extraJvmFlags":"-Dsecret=1"}`,
		".dylaris-backups/job-1/a.tar.gz": "ARCHIVE",
		"survival/server.properties":      "motd=hi",
		"survival/plugins/ok.jar":         "JAR",
	} {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return h, uuid, root
}

func readVia(h *StreamHandler, uuid string, req *pb.NodeMessage) (body []byte, errCode int32) {
	var buf bytes.Buffer
	req.ServerUuid = uuid
	h.HandleStreaming(req, func(m *pb.NodeMessage) error {
		if c := m.GetChunk(); c != nil {
			buf.Write(c.Data)
		}
		if e := m.GetError(); e != nil {
			errCode = e.Code
		}
		return nil
	})
	return buf.Bytes(), errCode
}

func zipNames(t *testing.T, b []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	var out []string
	for _, f := range zr.File {
		out = append(out, f.Name)
	}
	return out
}

// The read side never checked the node's own files: files.read was enough to
// read .node_config.json and to download backups without backups.read.
func TestTheNodesOwnFilesAreNotReadable(t *testing.T) {
	h, uuid, _ := seedNodeOwned(t)

	for _, p := range []string{".node_config.json", ".dylaris-backups/job-1/a.tar.gz"} {
		body, code := readVia(h, uuid, &pb.NodeMessage{RequestId: "r", Payload: &pb.NodeMessage_ReadReq{ReadReq: &pb.ReadFileReq{Path: p}}})
		if code != 404 || len(body) != 0 {
			t.Errorf("read %s: code %d, %d bytes", p, code, len(body))
		}
	}

	// The whole server as a zip, plain and selective, leaves them out.
	for name, req := range map[string]*pb.NodeMessage{
		"dir zip":    {RequestId: "z", Payload: &pb.NodeMessage_ReadReq{ReadReq: &pb.ReadFileReq{Path: "", ZipIfDir: true}}},
		"select all": {RequestId: "s", Payload: &pb.NodeMessage_SelectiveReadReq{SelectiveReadReq: &pb.SelectiveReadReq{BasePath: "", SelectAll: true}}},
		"selected":   {RequestId: "x", Payload: &pb.NodeMessage_SelectiveReadReq{SelectiveReadReq: &pb.SelectiveReadReq{BasePath: "", Selected: []string{".node_config.json", ".dylaris-backups", "survival"}}}},
	} {
		body, code := readVia(h, uuid, req)
		if code != 0 {
			t.Fatalf("%s: error %d", name, code)
		}
		names := strings.Join(zipNames(t, body), " ")
		if strings.Contains(names, "node_config") || strings.Contains(names, "dylaris-backups") || !strings.Contains(names, "ok.jar") {
			t.Errorf("%s: archive holds %s", name, names)
		}
	}

	list := h.handleList("l", uuid, &pb.ListFilesReq{Path: ".dylaris-backups/job-1"})
	if files := list.GetListResp().GetFiles(); len(files) != 0 {
		t.Errorf("the backup store was listed: %v", files)
	}

	copied := h.handleCopy("c", uuid, &pb.CopyFileReq{SrcPath: ".dylaris-backups/job-1/a.tar.gz", DstPath: "survival/a.tar.gz"})
	if copied.GetError() == nil {
		t.Error("a backup was copied out of the store")
	}
}

// Requests other than a WriteReq ran on the read loop for their Core
// connection, so a slow one - a large copy, a delete of millions of files, an
// open that blocked - stopped the node reading anything else from that Core.
func TestAFileRequestDoesNotHoldTheReadLoop(t *testing.T) {
	h, uuid, _ := seedNodeOwned(t)
	stream := &fakeCoreStream{}
	cc := &coreConnection{stream: stream}
	m := &MeshManager{handler: h}

	// Every slot of the server taken: the request can only be answered once
	// one frees, so an answer before that means it ran on the loop.
	slots := requestSlotsFor(uuid)
	for i := 0; i < requestsPerServer; i++ {
		slots <- struct{}{}
	}
	t.Cleanup(func() {
		for len(slots) > 0 {
			<-slots
		}
	})
	m.handleRequest(cc, &pb.NodeMessage{RequestId: "ls", ServerUuid: uuid, Payload: &pb.NodeMessage_ListReq{ListReq: &pb.ListFilesReq{Path: "survival"}}})
	if n := len(stream.messages()); n != 0 {
		t.Fatalf("answered %d messages on the read loop", n)
	}
	<-slots
	deadline := time.Now().Add(2 * time.Second)
	for len(stream.messages()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	for i := 1; i < requestsPerServer; i++ {
		<-slots
	}
	msgs := stream.messages()
	if len(msgs) != 1 || msgs[0].GetListResp() == nil {
		t.Fatalf("got %v, want the listing once a slot was free", msgs)
	}
}

// A request that waited for a slot ran whenever one freed, long after Core
// had answered the user with a timeout: a delete or copy then ran twice.
func TestARequestThatWaitsTooLongIsAnsweredBusy(t *testing.T) {
	h, uuid, _ := seedNodeOwned(t)
	prev := requestSlotWait
	t.Cleanup(func() { requestSlotWait = prev })
	requestSlotWait = 50 * time.Millisecond
	stream := &fakeCoreStream{}
	cc := &coreConnection{stream: stream}
	m := &MeshManager{handler: h}

	slots := requestSlotsFor(uuid)
	for i := 0; i < requestsPerServer; i++ {
		slots <- struct{}{}
	}
	t.Cleanup(func() {
		for len(slots) > 0 {
			<-slots
		}
	})
	m.handleRequest(cc, &pb.NodeMessage{RequestId: "rm", ServerUuid: uuid, Payload: &pb.NodeMessage_DeleteReq{DeleteReq: &pb.DeleteFileReq{Path: "survival/plugins/ok.jar"}}})
	deadline := time.Now().Add(2 * time.Second)
	for len(stream.messages()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	msgs := stream.messages()
	if len(msgs) != 1 || msgs[0].GetError().GetCode() != 503 {
		t.Fatalf("got %v, want one busy answer", msgs)
	}
	for len(slots) > 0 {
		<-slots
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(h.serverDir(uuid), "survival", "plugins", "ok.jar")); err != nil {
		t.Fatal("the delete ran after it was answered busy")
	}
}
