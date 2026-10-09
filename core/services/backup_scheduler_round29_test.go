package services

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"dylaris-core/models"
	backupstorage "dylaris-core/storage/backup"
)

// The scheduler created a run with no check for one in progress: an hourly
// job on a large world ran twice at once, and the first to finish switched
// saving back on under the second.
func TestScheduledRunWaitsForTheJobsRunInProgress(t *testing.T) {
	st := newTransferFakeStore()
	st.storage = ownedConnectionStorage()
	st.busy = true
	rdb := heartbeatRedis(t, map[string]string{"node-hosting": presignedMultipartSince})
	b := transferScheduler(st, &fakeMultipartStorage{}, rdb)

	if err := b.dispatch(context.Background(), models.BackupJob{ID: 10, ServerID: 100, Schedule: "every 1h"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if cmds := dispatchedCommands(t, rdb, "node-hosting"); len(cmds) != 0 {
		t.Fatalf("a second run reached the node: %v", cmds)
	}
	if st.advanced != 1 {
		t.Fatalf("schedule advanced %d times, want 1", st.advanced)
	}
}

// Two jobs of one server due together ran side by side; now the second waits.
// Advancing it a whole interval made the same job lose every time, so it is
// tried again next tick instead.
func TestAJobWaitsForAnotherJobOfItsServer(t *testing.T) {
	st := newTransferFakeStore()
	st.storage = ownedConnectionStorage()
	st.busy, st.busyOther = true, true
	rdb := heartbeatRedis(t, map[string]string{"node-hosting": presignedMultipartSince})
	b := transferScheduler(st, &fakeMultipartStorage{}, rdb)

	if err := b.dispatch(context.Background(), models.BackupJob{ID: 10, ServerID: 100, Schedule: "every 6h"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if cmds := dispatchedCommands(t, rdb, "node-hosting"); len(cmds) != 0 {
		t.Fatalf("a second backup of the server reached the node: %v", cmds)
	}
	if st.advanced != 0 {
		t.Fatalf("schedule advanced %d times; the job would skip its whole interval", st.advanced)
	}
}

// A storage Core could not open failed the run and left the job due, so every
// one-minute tick made another failed run until they pushed the good backups
// out of the 50-row list.
func TestAFailedScheduledRunWaitsForTheNextInterval(t *testing.T) {
	st := newTransferFakeStore()
	st.storage = ownedConnectionStorage()
	rdb := heartbeatRedis(t, map[string]string{"node-hosting": presignedMultipartSince})
	b := transferScheduler(st, &fakeMultipartStorage{}, rdb)
	b.SetConnection(func(int, string) (backupstorage.Storage, error) { return nil, errors.New("no such bucket") })

	if err := b.dispatch(context.Background(), models.BackupJob{ID: 10, ServerID: 100, Schedule: "every 1d"}); err == nil {
		t.Fatal("dispatch reported success for a storage it could not open")
	}
	if st.advanced != 1 {
		t.Fatalf("schedule advanced %d times, want 1: the job stays due every minute", st.advanced)
	}
}

// Scheduled runs wrote no manifest: their restore left the install and mod
// rows alone and could not say which sub-server the archive covered.
func TestAScheduledRunCarriesItsManifest(t *testing.T) {
	st := newTransferFakeStore()
	st.storage = ownedConnectionStorage()
	rdb := heartbeatRedis(t, map[string]string{"node-hosting": presignedMultipartSince})
	b := transferScheduler(st, &fakeMultipartStorage{}, rdb)
	lobby := "lobby"

	if err := b.dispatch(context.Background(), models.BackupJob{ID: 10, ServerID: 100, Schedule: "every 1d", SubServer: &lobby}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	cmds := dispatchedCommands(t, rdb, "node-hosting")
	if len(cmds) != 1 {
		t.Fatalf("commands = %d, want 1", len(cmds))
	}
	var cmd struct {
		Manifest *BackupManifest `json:"manifest"`
	}
	if err := json.Unmarshal([]byte(cmds[0]), &cmd); err != nil || cmd.Manifest == nil || cmd.Manifest.Scope != "lobby" {
		t.Fatalf("manifest = %+v (err %v), want one scoped to lobby", cmd.Manifest, err)
	}
}
