package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dylaris-pkg/fileperms"
	pb "dylaris-proto/beam"

	"google.golang.org/grpc/peer"
)

// A Beam save waits on the upload throttle, which at a low cap is tens of
// seconds. A session ended during that wait must not still write: the check in
// front of the wait was the only one.
func TestABeamSaveQueuedByAnEndedSessionDoesNotLand(t *testing.T) {
	sm := NewStorageManager(t.TempDir(), nil)
	th := &BeamThrottle{}
	th.setLimits(64*1024, 0) // burst 64 KiB, so the rest of the save waits
	bs := &beamServer{storageMgr: sm, throttle: th}
	const uuid = "24242424-0808-0808-0808-080808080808"
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 42408}
	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: addr})
	bs.serverUUIDByPeer.Store(addr.String(), uuid)
	bindTestSession(bs, addr.String())
	full := fileperms.Full()
	bs.permsByPeer.Store(addr.String(), &full)
	dir := filepath.Join(sm.GetServerDir(uuid), "survival")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		v, _ := bs.sessionByPeer.Load(addr.String())
		v.(*beamSession).ended.Store(true)
	}()
	resp, err := bs.SaveFileContent(ctx, &pb.BeamFileSaveReq{Path: "survival/x.txt", Content: strings.Repeat("x", 96*1024)})
	if err == nil && resp.GetSuccess() {
		t.Error("a save whose session ended while it waited on the throttle still succeeded")
	}
	if _, serr := os.Stat(filepath.Join(dir, "x.txt")); serr == nil {
		t.Error("the save was written after its session ended")
	}
}

// The metadata writer creates .dylaris.json.tmp and renames it over
// .dylaris.json. A tenant open of the temp name in that window chowned the
// node's inode to the container user, and with it the renamed .dylaris.json.
func TestTheMetadataTempNameIsProtected(t *testing.T) {
	for _, p := range []string{metadataTempName, "./" + metadataTempName} {
		if !isProtectedFile(p) {
			t.Errorf("%q is writable through the file APIs", p)
		}
	}
}

// With the storage path not mounted, GetServerDir returns "" and the sub-server
// path became the bare name relative to the node's working directory: deleting
// a sub-server named "binary" renamed the node agent away. The delete must stop
// before it touches the container, the disk or the status.
func TestDeleteSubServerStopsWithoutAServerDirectory(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	i := strings.Index(s, "\tcase \"delete_sub_server\":")
	j := strings.Index(s[i:], "\tcase \"reinstall\":")
	if i < 0 || j < 0 {
		t.Fatal("the delete_sub_server case moved; move this assertion with it")
	}
	body := s[i : i+j]
	g := strings.Index(body, "if serverPath == \"\" {")
	if g < 0 {
		t.Fatal("delete_sub_server joins the sub-server name onto an unresolved server directory")
	}
	for _, after := range []string{"teardown()", "os.Rename(subServerPath", "dropSubServerStash(", "rdb.Set(ctx, statusKey", "refreshServerMetadata("} {
		if k := strings.Index(body, after); k >= 0 && k < g {
			t.Errorf("delete_sub_server reaches %q before it checks the server directory", after)
		}
	}
}
