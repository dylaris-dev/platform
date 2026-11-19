package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	beampb "dylaris-proto/beam"
)

// R39-05: a Beam copy took no per-server request slot, so any number of
// connections ran whole-tree du walks at once where the panel's copy is
// bounded to requestsPerServer. With every slot taken it is answered busy and
// copies nothing.
func TestABeamCopyTakesARequestSlot(t *testing.T) {
	bs, uuid, ctx := newTestBeamServer(t)
	prev := requestSlotWait
	t.Cleanup(func() { requestSlotWait = prev })
	requestSlotWait = 50 * time.Millisecond
	dir := bs.storageMgr.GetServerDir(uuid)
	os.MkdirAll(filepath.Join(dir, "survival", "pack"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, "survival", "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	slots := requestSlotsFor(uuid)
	for i := 0; i < requestsPerServer; i++ {
		slots <- struct{}{}
	}
	t.Cleanup(func() {
		for len(slots) > 0 {
			<-slots
		}
	})
	for _, tc := range []struct{ src, dst string }{
		{"survival/a.txt", "survival/b.txt"},
		{"survival/pack", "survival/pack2"},
	} {
		resp, err := bs.CopyFile(ctx, &beampb.BeamFileCopyReq{SrcPath: tc.src, DstPath: tc.dst})
		if err == nil && resp.Success {
			t.Errorf("%s: copied with every request slot of the server taken", tc.src)
		}
		if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(tc.dst))); err == nil {
			t.Errorf("%s: %s was written", tc.src, tc.dst)
		}
	}
	if len(slots) != requestsPerServer {
		t.Errorf("%d slots held after the refusals, want %d", len(slots), requestsPerServer)
	}
}
