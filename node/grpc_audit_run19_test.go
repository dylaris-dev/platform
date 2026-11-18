package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "dylaris-proto/node"
)

// Core's WsClose only reached the bridge through its queue, which the
// Core->container pump alone reads. A pump stuck writing to a container that
// stopped reading never saw it, so the bridge kept its slot until TCP gave up.
func TestACoreWsCloseTearsTheBridgeDownWithoutThePump(t *testing.T) {
	m := &MeshManager{wsBridges: make(map[string]*wsBridge)}
	b := &wsBridge{server: "s", inbound: make(chan *pb.NodeMessage, 4), done: make(chan struct{})}
	m.wsBridges["r"] = b
	m.routeWSInbound("r", &pb.NodeMessage{RequestId: "r", Payload: &pb.NodeMessage_WsClose{WsClose: &pb.WsClose{Code: 1000}}})
	if _, ok := m.wsBridges["r"]; ok {
		t.Fatal("bridge still registered after Core closed it")
	}
	select {
	case <-b.done:
	default:
		t.Fatal("bridge not closed")
	}
}

// An upload that ends without a commit removed its staging file but left the
// folder size a listing had cached with that file in it.
func TestAnAbortedUploadDropsTheFolderSizes(t *testing.T) {
	h, uuid, _ := seedNodeOwned(t)
	clock := time.Unix(1000, 0)
	nowDirSize = func() time.Time { return clock }
	t.Cleanup(func() { nowDirSize = time.Now })

	survivalSize := func() int64 {
		t.Helper()
		for _, f := range h.handleList("l", uuid, &pb.ListFilesReq{Path: "."}).GetListResp().GetFiles() {
			if f.Name == "survival" {
				return f.Size
			}
		}
		t.Fatal("survival not listed")
		return 0
	}

	before := survivalSize()
	dirSizes.forget(h.serverDir(uuid))
	f, tmp, err := h.createUploadTemp(h.serverDir(uuid), "survival/world.zip")
	if err != nil {
		t.Fatal(err)
	}
	f.Write(make([]byte, 100))
	f.Close()
	if got := survivalSize(); got != before+100 {
		t.Fatalf("during the upload: %d, want %d", got, before+100)
	}
	h.removeUploadTemp(h.serverDir(uuid), "survival/world.zip", tmp)
	if got := survivalSize(); got != before {
		t.Fatalf("after the abort: %d, want %d", got, before)
	}
}

// One selected entry deleted between the listing and the click aborted the
// whole download instead of zipping the rest.
func TestASelectiveZipSkipsAVanishedSelection(t *testing.T) {
	h := NewStreamHandler(NewStorageManager(t.TempDir(), nil))
	const uuid = "19191919-1919-1919-1919-191919191919"
	plugins := filepath.Join(h.serverDir(uuid), "plugins")
	if err := os.MkdirAll(plugins, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugins, "ok.jar"), []byte("JAR"), 0644); err != nil {
		t.Fatal(err)
	}
	raw := collectZipFromStream(t, func(send func(*pb.NodeMessage) error) {
		h.streamSelectiveZip("r", uuid, &pb.SelectiveReadReq{
			BasePath: "plugins",
			Selected: []string{"gone.jar", "ok.jar"},
		}, send)
	})
	_, bodies := zipNamesAndBodies(t, raw)
	if bodies["ok.jar"] != "JAR" {
		t.Errorf("ok.jar = %q, want JAR", bodies["ok.jar"])
	}
}
