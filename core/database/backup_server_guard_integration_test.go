package database

import (
	"testing"
	"time"

	"dylaris-core/models"
)

// One running backup per SERVER: per job, a "full" and a "world" job of one
// server ran side by side, and the first to finish switched world saving back
// on under the other's archive.
func TestIntegrationOneRunningBackupPerServer(t *testing.T) {
	_, st := integrationDB(t)
	a, b := newFixture(t, st), newFixture(t, st)
	mkJob := func(serverID int) int {
		id, err := st.CreateBackupJob(&models.BackupJob{ServerID: serverID, Name: uniqueName("j_"), Schedule: "manual", RetentionCount: 1, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	full, world, other := mkJob(a.server.ID), mkJob(a.server.ID), mkJob(b.server.ID)
	start := func(job int) (int, bool) {
		id, started, err := st.StartBackupRunIfIdle(&models.BackupRun{JobID: job, Status: "running", StorageKey: "k"})
		if err != nil {
			t.Fatal(err)
		}
		return id, started
	}

	first, ok := start(full)
	if !ok {
		t.Fatal("the first backup of the server did not start")
	}
	if _, ok := start(world); ok {
		t.Error("a second job of the same server started while the first ran")
	}
	if _, ok := start(full); ok {
		t.Error("the same job started twice")
	}
	if _, ok := start(other); !ok {
		t.Error("another server's backup waited for this one")
	}
	if err := st.UpdateBackupRunStatus(first, "success", "", 1, "k", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, ok := start(world); !ok {
		t.Error("the second job did not start once the first finished")
	}
}

// Runs and restores on an offline node wait for it; listed first, they filled
// every batch and nothing behind them was ever closed.
func TestIntegrationAbandonedOnAnOfflineNodeComeLast(t *testing.T) {
	db, st := integrationDB(t)
	off, on := newFixture(t, st), newFixture(t, st)
	if _, err := db.Exec(`UPDATE nodes SET status = 'offline' WHERE id = $1`, off.server.NodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE nodes SET status = 'online' WHERE id = $1`, on.server.NodeID); err != nil {
		t.Fatal(err)
	}
	run := func(serverID int, age string) int {
		job, err := st.CreateBackupJob(&models.BackupJob{ServerID: serverID, Name: uniqueName("j_"), Schedule: "manual", RetentionCount: 1, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		id, err := st.CreateBackupRun(&models.BackupRun{JobID: job, Status: "running", StorageKey: "k"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE backup_runs SET started_at = NOW() - $1::interval WHERE id = $2`, age, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	run(off.server.ID, "9 hours")
	onRun := run(on.server.ID, "7 hours")
	got, err := st.ListAbandonedBackupRuns(time.Now().Add(-6*time.Hour), 1)
	if err != nil || len(got) != 1 || got[0].ID != onRun {
		t.Fatalf("listed %+v, %v; want the run on the online node first", got, err)
	}

	restore := func(serverID, runID int, age string) int {
		id, err := st.CreateBackupRestore(&models.BackupRestore{RunID: runID, ServerID: serverID, Status: "queued"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE backup_restores SET requested_at = NOW() - $1::interval WHERE id = $2`, age, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	restore(off.server.ID, run(off.server.ID, "1 hour"), "9 hours")
	onRestore := restore(on.server.ID, onRun, "7 hours")
	rs, err := st.ListAbandonedBackupRestores(time.Now().Add(-6*time.Hour), 1)
	if err != nil || len(rs) != 1 || rs[0].ID != onRestore || rs[0].NodeStatus != "online" {
		t.Fatalf("listed %+v, %v; want the restore on the online node first, with its status", rs, err)
	}
}
