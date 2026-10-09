package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"dylaris-pkg/queue"
)

// Two runs of one server side by side: the first to finish switched world
// saving back on under the other's archive. A node back from days away had
// several queued for the same server.
func TestASecondBackupOfTheServerIsRefused(t *testing.T) {
	rdb := newMiniRedis(t)
	const uuid = "srv-round93"
	if !backupsInFlight.enter("server:" + uuid) {
		t.Fatal("the server was already marked")
	}
	defer backupsInFlight.leave("server:" + uuid)

	cmd := BackupRunCommand{RunID: 93, JobID: 2, ServerUUID: uuid, StorageKey: "backups/x.tar.gz"}
	report := terminalReport(t, rdb, queue.BackupResultsChannel(nodeID), func() {
		RunBackup(context.Background(), rdb, &StorageManager{paths: []string{t.TempDir()}}, nil, cmd)
	})
	if report["status"] != "failed" || !strings.Contains(report["error"].(string), "another backup of this server") {
		t.Fatalf("report = %v", report)
	}
	if !backupsInFlight.enter("93") {
		t.Fatal("the refused run stayed marked")
	}
	backupsInFlight.leave("93")
}

// The redelivery guard comes first: a second delivery of the run that is
// running must stay silent, not report it failed.
func TestARedeliveryOfTheRunningBackupStaysSilent(t *testing.T) {
	rdb := newMiniRedis(t)
	const uuid = "srv-round93b"
	for _, k := range []string{"94", "server:" + uuid} {
		if !backupsInFlight.enter(k) {
			t.Fatal("already marked")
		}
		defer backupsInFlight.leave(k)
	}
	sub := rdb.Subscribe(context.Background(), queue.BackupResultsChannel(nodeID))
	defer sub.Close()
	if _, err := sub.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	RunBackup(context.Background(), rdb, &StorageManager{paths: []string{t.TempDir()}}, nil,
		BackupRunCommand{RunID: 94, JobID: 2, ServerUUID: uuid, StorageKey: "backups/x.tar.gz"})
	if msg, err := sub.ReceiveTimeout(context.Background(), 300*time.Millisecond); err == nil {
		t.Fatalf("the running backup got a report: %v", msg)
	}
}
