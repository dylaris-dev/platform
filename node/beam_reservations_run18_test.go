package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/metadata"
)

// gaugeWriteHook records the server's in-flight reservation at the moment the
// gauge key is written.
type gaugeWriteHook struct {
	key  string
	uuid string
	seen atomic.Int64
	hits atomic.Int64
}

func (h *gaugeWriteHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *gaugeWriteHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *gaugeWriteHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if args := cmd.Args(); len(args) > 1 && strings.EqualFold(cmd.Name(), "set") && args[1] == h.key {
			h.seen.Store(serverInflightOf(h.uuid).Load())
			h.hits.Add(1)
		}
		return next(ctx, cmd)
	}
}

// Y06: a finished copy dropped its reservation before the remeasure walk
// published the gauge that holds its bytes, so a Beam Start during the walk
// saw the copy's size as free headroom.
func TestAFinishedCopyHoldsItsReservationUntilTheGaugeHasIt(t *testing.T) {
	_, rdb := gaugeRedis(t)
	prevMgr := globalStorageMgr
	t.Cleanup(func() { globalStorageMgr = prevMgr })
	root := t.TempDir()
	globalStorageMgr = NewStorageManager(root, rdb)
	const uuid = "18181818-0606-0606-0606-181818181818"
	dir := filepath.Join(root, uuid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "copied.dat"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		unmeasured.Lock()
		delete(unmeasured.m, uuid)
		delete(unmeasured.last, uuid)
		unmeasured.Unlock()
	})
	hook := &gaugeWriteHook{key: diskGaugeKey(uuid), uuid: uuid}
	rdb.AddHook(hook)

	base := serverInflightOf(uuid).Load()
	b := nodeWriteBudget(dir)
	b.boundServer(uuid, 1<<30)
	if !b.spend(4096) {
		t.Fatal("spend refused")
	}
	b.release()

	if hook.hits.Load() == 0 {
		t.Fatal("release published no gauge")
	}
	if got := hook.seen.Load() - base; got != 4096 {
		t.Errorf("reserved while the gauge was written = %d, want the copy's 4096 still held", got)
	}
	if got := serverInflightOf(uuid).Load() - base; got != 0 {
		t.Errorf("reserved after release = %d, want 0", got)
	}
}

// Y07: a resume while the old stream was still open reserved the whole
// remaining size next to the old stream's untouched reservation, so an upload
// over half the headroom was refused as over the disk limit, and the refused
// resume also stopped the old stream.
func TestAResumeWhileTheOldStreamIsOpenTakesItsReservationOver(t *testing.T) {
	_, rdb := gaugeRedis(t)
	bs, serverUUID, ctx := newTestBeamServer(t)
	bs.rdb = rdb
	bs.storageMgr = NewStorageManager(t.TempDir(), rdb)
	rdb.Set(ctx, diskGaugeKey(serverUUID), `{"total":0,"limit":10}`, 10*time.Minute)
	t.Cleanup(func() {
		unmeasured.Lock()
		delete(unmeasured.m, serverUUID)
		delete(unmeasured.last, serverUUID)
		unmeasured.Unlock()
	})
	base := serverInflightOf(serverUUID).Load()
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("x-beam-upload-id", "Y07"))
	var second *fakeBeamUploadStream
	var secondErr error
	resume := uploadMsgs("survival", "w.zip", 8, []byte("abcd"), []byte("efgh"))
	resume = append(resume[:1], resume[2:]...) // only the chunk the old stream did not get to
	a := &hookedUploadStream{
		fakeBeamUploadStream: fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("survival", "w.zip", 8, []byte("abcd"), []byte("efgh"))},
		before: func(i int) {
			if i == 2 { // the old stream holds its 8-byte reservation and 4 written bytes
				second = &fakeBeamUploadStream{ctx: ctx, msgs: resume}
				secondErr = bs.UploadFile(second)
			}
		},
	}
	_ = bs.UploadFile(a)
	if secondErr != nil || second.resp == nil || !second.resp.Success {
		t.Fatalf("the resume: err=%v resp=%+v", secondErr, second.resp)
	}
	got, err := os.ReadFile(filepath.Join(bs.storageMgr.GetServerDir(serverUUID), "survival", "w.zip"))
	if err != nil || string(got) != "abcdefgh" {
		t.Errorf("landed %q, %v", got, err)
	}
	if total := gaugeTotal(t, rdb, serverUUID); total != 8 {
		t.Errorf("gauge = %d, want the 8 bytes counted once", total)
	}
	if left := serverInflightOf(serverUUID).Load() - base; left != 0 {
		t.Errorf("%d bytes still reserved after both streams ended", left)
	}
	if n := len(bs.uploadTemps); n != 0 {
		t.Errorf("%d temp claims left after every stream ended", n)
	}
}
