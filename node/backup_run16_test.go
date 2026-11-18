package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "dylaris-proto/node"
)

// W08: deleting a run that was still being written removed only the final
// name, which did not exist yet. The writer then renamed its .partial into
// place, Core had already dropped the row, and the archive stayed on disk for
// good, counted against the backup quota with nothing that could delete it.
func TestABackupDeleteAlsoRemovesTheArchiveBeingWritten(t *testing.T) {
	for _, tc := range []struct {
		name     string
		files    []string
		wantCode int32 // 0 = success
	}{
		{"only the partial", []string{"run-1.tar.gz" + partialBackupSuffix}, 0},
		{"final and partial", []string{"run-1.tar.gz", "run-1.tar.gz" + partialBackupSuffix}, 0},
		{"only the final", []string{"run-1.tar.gz"}, 0},
		{"neither", nil, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm := NewStorageManager(t.TempDir(), nil)
			h := &StreamHandler{storageMgr: sm}
			const uuid = "08080808-0808-0808-0808-080808080808"
			dir := filepath.Join(resolveServerRoot(sm, uuid), backupDirName)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, f := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("A"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			m := h.handleBackupDelete("r", uuid, &pb.BackupDeleteReq{Key: "run-1.tar.gz"})
			if got := m.GetError().GetCode(); got != tc.wantCode {
				t.Fatalf("delete = %v, want code %d", m.GetError(), tc.wantCode)
			}
			left, _ := os.ReadDir(dir)
			if len(left) != 0 {
				t.Fatalf("left behind: %v", left)
			}
		})
	}
}

// W03: a backup delete during a storage move waited out the move inside one of
// the server's four file slots. Four of them (one retention sweep) held every
// slot, and every later request for the server was answered "busy" after 20s
// instead of the immediate 409 Handle gives during a move.
func TestBackupDeletesDuringAMoveDoNotTakeTheFileSlots(t *testing.T) {
	h, uuid, _ := seedNodeOwned(t)
	prev := requestSlotWait
	t.Cleanup(func() { requestSlotWait = prev })
	requestSlotWait = 2 * time.Second
	release, err := h.storageMgr.fenceMove(uuid)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	stream := &fakeCoreStream{}
	cc := &coreConnection{stream: stream}
	m := &MeshManager{handler: h}

	for i := 0; i < requestsPerServer; i++ {
		m.handleRequest(cc, &pb.NodeMessage{RequestId: "bd", ServerUuid: uuid, Payload: &pb.NodeMessage_BackupDeleteReq{BackupDeleteReq: &pb.BackupDeleteReq{Key: "gone.tar.gz"}}})
	}
	time.Sleep(50 * time.Millisecond)
	m.handleRequest(cc, &pb.NodeMessage{RequestId: "ls", ServerUuid: uuid, Payload: &pb.NodeMessage_ListReq{ListReq: &pb.ListFilesReq{Path: "survival"}}})

	deadline := time.Now().Add(time.Second)
	var ls *pb.NodeMessage
	for ls == nil && time.Now().Before(deadline) {
		for _, msg := range stream.messages() {
			if msg.RequestId == "ls" {
				ls = msg
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if ls.GetError().GetCode() != 409 {
		t.Fatalf("listing during the move = %v, want an immediate 409", ls)
	}

	// The deletes are still waited for, not refused, and answered after the move.
	release()
	released = true
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n := 0
		for _, msg := range stream.messages() {
			if msg.RequestId == "bd" {
				n++
				if msg.GetError().GetCode() != 404 {
					t.Fatalf("backup delete after the move = %v, want 404 for a missing archive", msg)
				}
			}
		}
		if n == requestsPerServer {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the backup deletes were not answered after the move")
}
