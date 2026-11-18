package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	nodepb "dylaris-proto/node"
)

// A01: a small read (Players tab, server.properties, the editor) behind a
// server's four long downloads waited with no bound, and Core waited on it
// with none either. It is answered busy, like every other file request.
func TestADownloadThatWaitsTooLongIsAnsweredBusy(t *testing.T) {
	h, uuid, _ := seedNodeOwned(t)
	prev := requestSlotWait
	t.Cleanup(func() { requestSlotWait = prev })
	requestSlotWait = 50 * time.Millisecond
	slots := downloadSlotsFor(uuid)
	for i := 0; i < downloadsPerServer; i++ {
		slots <- struct{}{}
	}
	t.Cleanup(func() {
		for len(slots) > 0 {
			<-slots
		}
	})
	stream := &fakeCoreStream{}
	cc := &coreConnection{stream: stream}
	m := &MeshManager{handler: h}
	m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "props", ServerUuid: uuid, Payload: &nodepb.NodeMessage_ReadReq{ReadReq: &nodepb.ReadFileReq{Path: "survival/server.properties"}}})
	deadline := time.Now().Add(2 * time.Second)
	for len(stream.messages()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	msgs := stream.messages()
	if len(msgs) != 1 || msgs[0].GetError().GetCode() != 503 {
		t.Fatalf("got %v, want one busy answer", msgs)
	}
}

// A02: the upload budget charged the full size of a file that replaces an
// existing one, so a server at its disk limit could not save even a shorter
// server.properties. Only the growth counts against the server's limit.
func TestAMeshUploadOverAFileChargesOnlyItsGrowth(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing int // size of survival/server.properties; -1 = none
		upload   int64
		want     bool
	}{
		{"a shorter save at the limit", 500, 300, true},
		{"a same-size save at the limit", 500, 500, true},
		{"a save that grows past the limit", 500, 600, false},
		{"a new file at the limit", -1, 300, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, uuid, root := seedNodeOwned(t)
			withDiskBudget(t, 1<<30, 1000)
			_, rdb := gaugeRedis(t)
			rdb.Set(context.Background(), diskGaugeKey(uuid), `{"total":1000,"limit":1000}`, 10*time.Minute)
			props := filepath.Join(root, "survival", "server.properties")
			if tc.existing < 0 {
				os.Remove(props)
			} else if err := os.WriteFile(props, make([]byte, tc.existing), 0o644); err != nil {
				t.Fatal(err)
			}
			stream := &fakeCoreStream{}
			cc := &coreConnection{stream: stream}
			m := &MeshManager{handler: h, rdb: rdb, pendingWrites: map[string]*pendingWrite{}}
			m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "w", ServerUuid: uuid, Payload: &nodepb.NodeMessage_WriteReq{WriteReq: &nodepb.WriteFileReq{Path: "survival/server.properties", TotalSize: tc.upload}}})
			var got = true
			for _, msg := range stream.messages() {
				if msg.GetError() != nil {
					got = false
				}
			}
			if got != tc.want {
				t.Errorf("accepted = %v, want %v (%s)", got, tc.want, fmt.Sprint(stream.messages()))
			}
			m.handleRequest(cc, &nodepb.NodeMessage{RequestId: "w", Payload: &nodepb.NodeMessage_TransferDone{TransferDone: &nodepb.TransferDone{}}})
		})
	}
}
