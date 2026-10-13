package services

import (
	"os"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

type movingStore struct {
	store.Store
	srv   map[int]models.Server
	calls int
}

func (m *movingStore) GetServerByID(id int) (*models.Server, error) {
	m.calls++
	s := m.srv[id]
	return &s, nil
}

// SFTP is read-only for a server being moved: its source copy is deleted after
// cutover, so a write landing meanwhile was lost. Asked once per server per
// sync, not once per user.
func TestSFTPIsReadOnlyWhileTheServerMoves(t *testing.T) {
	st := &movingStore{srv: map[int]models.Server{1: {ID: 1, Status: "migrating"}, 2: {ID: 2, Status: "online"}}}
	m := newServerMoving(st)
	if !m.of(1) || m.of(2) || !m.of(1) || st.calls != 2 {
		t.Fatalf("moving: 1=%v 2=%v, %d lookups", m.of(1), m.of(2), st.calls)
	}
	b, err := os.ReadFile("sftp_sync.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "if moving.of(a.ServerID) {\n\t\t\t\tperms.Write, perms.Delete = false, false") &&
		!strings.Contains(string(b), "if moving.of(a.ServerID) {\r\n\t\t\t\tperms.Write, perms.Delete = false, false") {
		t.Error("the sync no longer publishes a moving server read-only")
	}
	o, err := os.ReadFile("migration_orchestrator.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(o), "o.bumpBeamAccess(srv.UUID)") || !strings.Contains(string(o), "defer o.bumpBeamAccess(srv.UUID)") {
		t.Error("a move no longer ends the open Beam sessions at its start and end")
	}
}

// A move republishes SFTP at once instead of a minute later; repeated kicks
// collapse into one pending sync and never block the mover.
func TestAMoveRepublishesSFTPAtOnce(t *testing.T) {
	s := NewSFTPSyncService(nil, nil, nil)
	for i := 0; i < 5; i++ {
		s.SyncSoon()
	}
	if len(s.kick) != 1 {
		t.Fatalf("%d syncs pending, want one", len(s.kick))
	}
	called := 0
	o := &MigrationOrchestrator{}
	o.SetOnMoving(func() { called++ })
	o.bumpBeamAccess("srv")
	if called != 1 {
		t.Fatalf("the move did not republish file access (%d)", called)
	}
}

// Progress is activity: only an object-storage part request counted, so a
// node-local run reporting every few seconds was reaped as abandoned at 6h.
func TestBackupProgressCountsAsActivity(t *testing.T) {
	b, err := os.ReadFile("backup_scheduler.go")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	// Forward progress only, so a hung run still ages out.
	if !strings.Contains(s, "if status == \"running\" && size > run.SizeBytes {\n\t\t\t\tif err := b.store.TouchBackupRunTransfer(result.RunID); err != nil {") {
		t.Error("a progress report no longer counts as activity")
	}
}
