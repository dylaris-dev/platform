package database

import (
	"testing"
	"time"

	"dylaris-core/models"
)

// The reaper lists only restores still open past the cutoff, and closing one
// never overwrites a result that arrived in the meantime.
func TestIntegrationAbandonedRestoresAreListedAndClosedOnlyWhileOpen(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)
	job, err := st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: uniqueName("j_"), Schedule: "manual", RetentionCount: 1, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateBackupRun(&models.BackupRun{JobID: job, Status: "running", StorageKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(status string, age time.Duration) int {
		id, err := st.CreateBackupRestore(&models.BackupRestore{RunID: run, ServerID: f.server.ID, Status: status})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE backup_restores SET requested_at = NOW() - $1::interval WHERE id = $2`, age.String(), id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	oldQueued := mk("queued", 7*time.Hour)
	oldRunning := mk("running", 8*time.Hour)
	mk("queued", time.Hour)
	mk("success", 9*time.Hour)

	got, err := st.ListAbandonedBackupRestores(time.Now().Add(-6*time.Hour), 25)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int]bool{}
	for _, r := range got {
		ids[r.ID] = true
		if r.ID == oldQueued && (r.ServerUUID != f.server.UUID || r.NodeID != f.server.NodeID) {
			t.Errorf("server uuid %q, want %q", r.ServerUUID, f.server.UUID)
		}
	}
	if len(ids) != 2 || !ids[oldQueued] || !ids[oldRunning] {
		t.Fatalf("listed %v, want only the two old open restores", got)
	}

	// The node's result lands between the list and the close.
	if _, err := st.UpdateBackupRestoreStatus(oldRunning, "success", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if closed, err := st.CloseAbandonedBackupRestore(oldRunning, "reaped", time.Now()); err != nil || closed {
		t.Fatalf("closed a restore that had succeeded: %v %v", closed, err)
	}
	if r, _ := st.GetBackupRestore(oldRunning); r.Status != "success" {
		t.Fatalf("status %q, want the node's success to stand", r.Status)
	}
	// A redelivered restore that the node then reports failed must not
	// overwrite a restore that already succeeded.
	if _, err := st.UpdateBackupRestoreStatus(oldRunning, "failed", "restore is no longer running", time.Now()); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.GetBackupRestore(oldRunning); r.Status != "success" || r.ErrorMessage != "" {
		t.Fatalf("status %q (%q), want a late failure not to overwrite success", r.Status, r.ErrorMessage)
	}
	if closed, err := st.CloseAbandonedBackupRestore(oldQueued, "reaped", time.Now()); err != nil || !closed {
		t.Fatalf("an open abandoned restore was not closed: %v %v", closed, err)
	}
	if r, _ := st.GetBackupRestore(oldQueued); r.Status != "failed" || r.CompletedAt == nil || r.ErrorMessage != "reaped" {
		t.Fatalf("closed restore = %+v", r)
	}
}

// The row decides which of two callers holding one restore result reconciles,
// so the write reports whether it changed the row; and every open restore is
// listed for its kept result, not only a capped batch (R50-01, R50-02).
func TestIntegrationRestoreResultChangesTheRowOnce(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	job, err := st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: uniqueName("j_"), Schedule: "manual", RetentionCount: 1, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateBackupRun(&models.BackupRun{JobID: job, Status: "success", StorageKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		row        string
		writes     []string // statuses written in order
		wantChange []bool
		wantStatus string
	}{
		{name: "two callers with one success", row: "queued", writes: []string{"success", "success"}, wantChange: []bool{true, false}, wantStatus: "success"},
		{name: "running to failed", row: "running", writes: []string{"failed", "failed"}, wantChange: []bool{true, false}, wantStatus: "failed"},
		{name: "a late failure after success", row: "success", writes: []string{"failed"}, wantChange: []bool{false}, wantStatus: "success"},
		{name: "a failed restore that then succeeded", row: "failed", writes: []string{"success", "success"}, wantChange: []bool{true, false}, wantStatus: "success"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := st.CreateBackupRestore(&models.BackupRestore{RunID: run, ServerID: f.server.ID, Status: tt.row})
			if err != nil {
				t.Fatal(err)
			}
			for i, s := range tt.writes {
				changed, err := st.UpdateBackupRestoreStatus(id, s, "", time.Now())
				if err != nil {
					t.Fatal(err)
				}
				if changed != tt.wantChange[i] {
					t.Errorf("write %d (%s): changed=%v, want %v", i, s, changed, tt.wantChange[i])
				}
			}
			if r, _ := st.GetBackupRestore(id); r.Status != tt.wantStatus {
				t.Errorf("status %q, want %q", r.Status, tt.wantStatus)
			}
		})
	}

	var open []int
	for i := 0; i < 30; i++ {
		id, err := st.CreateBackupRestore(&models.BackupRestore{RunID: run, ServerID: f.server.ID, Status: "queued"})
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, id)
	}
	done, err := st.CreateBackupRestore(&models.BackupRestore{RunID: run, ServerID: f.server.ID, Status: "success"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.ListOpenBackupRestores()
	if err != nil {
		t.Fatal(err)
	}
	listed := map[int]string{}
	for _, r := range got {
		listed[r.ID] = r.ServerUUID
	}
	for _, id := range open {
		if listed[id] != f.server.UUID {
			t.Errorf("open restore %d listed with uuid %q, want %q", id, listed[id], f.server.UUID)
		}
	}
	if _, ok := listed[done]; ok {
		t.Errorf("a finished restore was listed as open")
	}
}

// The overlay decision sent at dispatch survives to the result, and a row
// written without one reads back as unrecorded rather than as "full" (R41-04).
func TestIntegrationRestoreRecordsTheOverlayDecision(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	job, err := st.CreateBackupJob(&models.BackupJob{ServerID: f.server.ID, Name: uniqueName("j_"), Schedule: "manual", RetentionCount: 1, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateBackupRun(&models.BackupRun{JobID: job, Status: "success", StorageKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	for _, tt := range []struct {
		name    string
		overlay *bool
	}{{"overlay", &yes}, {"full", &no}, {"not recorded", nil}} {
		t.Run(tt.name, func(t *testing.T) {
			id, err := st.CreateBackupRestore(&models.BackupRestore{RunID: run, ServerID: f.server.ID, Status: "queued", Overlay: tt.overlay})
			if err != nil {
				t.Fatal(err)
			}
			r, err := st.GetBackupRestore(id)
			if err != nil {
				t.Fatal(err)
			}
			if (r.Overlay == nil) != (tt.overlay == nil) || (r.Overlay != nil && *r.Overlay != *tt.overlay) {
				t.Errorf("overlay = %v, want %v", r.Overlay, tt.overlay)
			}
		})
	}
}
