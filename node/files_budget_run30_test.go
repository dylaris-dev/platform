package main

import (
	"context"
	"testing"
	"time"

	nodepb "dylaris-proto/node"
)

// R30-06: a running upload's streamed bytes stayed reserved on the server's
// in-flight count, and a measurement of the server taken meanwhile (the disk
// ticker, a delete's remeasure, a copy's headroom walk) held them as well, so
// every check adding the two counted them twice and refused writes that fit.
func TestStreamedUploadBytesCountOnceAgainstTheServerLimit(t *testing.T) {
	const limitMB = 1
	for _, tc := range []struct {
		name string
		// run uploads 200 of a declared 500 bytes into the server, calling mid
		// once they are written; it returns the server's directory.
		run func(t *testing.T, setup func(base, uuid string) context.Context, mid func()) string
	}{
		{"beam", func(t *testing.T, setup func(base, uuid string) context.Context, mid func()) string {
			bs, uuid, ctx := newTestBeamServer(t)
			setup(bs.storageMgr.Paths()[0], uuid)
			bs.rdb = globalStorageMgr.rdb
			s := &midUploadStream{fakeBeamUploadStream: &fakeBeamUploadStream{ctx: ctx, msgs: uploadMsgs("survival", "w.zip", 500, make([]byte, 120), make([]byte, 80))}, mid: mid}
			if err := bs.UploadFile(s); err != nil {
				t.Fatal(err)
			}
			return bs.storageMgr.GetServerDir(uuid)
		}},
		{"mesh", func(t *testing.T, setup func(base, uuid string) context.Context, mid func()) string {
			m, uuid := meshForBudget(t)
			setup(m.handler.storageMgr.Paths()[0], uuid)
			m.rdb = globalStorageMgr.rdb
			cc := &coreConnection{stream: &fakeCoreStream{}}
			m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "w", ServerUuid: uuid, Payload: &nodepb.NodeMessage_WriteReq{WriteReq: &nodepb.WriteFileReq{Path: "survival/w.bin", TotalSize: 500}}})
			m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "w", Payload: &nodepb.NodeMessage_Chunk{Chunk: &nodepb.DataChunk{Data: make([]byte, 120)}}})
			m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "w", Payload: &nodepb.NodeMessage_Chunk{Chunk: &nodepb.DataChunk{Data: make([]byte, 80), Offset: 120}}})
			mid()
			m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "w", Payload: &nodepb.NodeMessage_TransferDone{TransferDone: &nodepb.TransferDone{TotalBytes: 200}}})
			return m.handler.storageMgr.GetServerDir(uuid)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withDiskBudget(t, 1<<20, 1000)
			_, rdb := gaugeRedis(t)
			prevMgr := globalStorageMgr
			t.Cleanup(func() { globalStorageMgr = prevMgr })
			var uuid, dir string
			var base int64
			setup := func(root, id string) context.Context {
				uuid = id
				globalStorageMgr = NewStorageManager(root, rdb)
				dir = globalStorageMgr.GetServerDir(uuid)
				// What the server is charged: its node-local backups (the
				// mesh fixture has one) are a separate budget.
				base = serverDiskBytes(dir, false)
				ctx := context.Background()
				recordDiskLimit(ctx, rdb, uuid, limitMB)
				rdb.Set(ctx, diskGaugeKey(uuid), `{"total":0,"limit":1048576}`, 10*time.Minute)
				t.Cleanup(func() {
					unmeasured.Lock()
					delete(unmeasured.m, uuid)
					delete(unmeasured.last, uuid)
					unmeasured.Unlock()
				})
				return ctx
			}
			called := false
			tc.run(t, setup, func() {
				called = true
				if got := serverInflightOf(uuid).Load(); got != 500 {
					t.Fatalf("%d reserved on the server, want the declared 500", got)
				}
				remeasureServer(uuid)
				if got := gaugeTotal(t, rdb, uuid); got != base {
					t.Errorf("gauge %d mid-upload, want %d: the 200 streamed bytes are also reserved", got, base)
				}
				if _, left, ok := serverDiskHeadroom(dir); !ok || left != limitMB<<20-base {
					t.Errorf("headroom %d (ok %v) mid-upload, want %d", left, ok, limitMB<<20-base)
				}
			})
			if !called {
				t.Fatal("the upload never reached its middle")
			}
			if got := gaugeTotal(t, rdb, uuid); got != base+200 {
				t.Errorf("gauge %d after the upload, want %d", got, base+200)
			}
			if got := serverInflightOf(uuid).Load(); got != 0 {
				t.Errorf("%d still reserved on the server after the upload", got)
			}
			// A measurement from here on must take nothing off.
			if got := serverStreamedOf(uuid).live.Load(); got != 0 {
				t.Errorf("%d bytes still counted as streamed after the upload", got)
			}
		})
	}
}
