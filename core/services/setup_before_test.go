package services

import (
	"context"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
	"dylaris-pkg/queue"
)

type setupBeforeStore struct {
	store.Store
	written  []string
	desired  []string
	install  *models.SubServerInstall // the sub-server's install record
	mods     []models.ServerMod
	replaced int
	srv      *models.Server
}

func (s *setupBeforeStore) GetSubServerInstall(serverID int, sub string) (*models.SubServerInstall, error) {
	return s.install, nil
}

func (s *setupBeforeStore) UpsertSubServerInstall(in models.SubServerInstall) error {
	s.install = &in
	return nil
}

func (s *setupBeforeStore) DeleteSubServerInstall(serverID int, sub string) error {
	s.install = nil
	return nil
}

func (s *setupBeforeStore) UpdateServerSetup(id int, image, command, active, flags, installer, mc, build string) error {
	s.written = append(s.written, active+"|"+installer+"|"+mc)
	return nil
}

// A failed install left Core on the new sub-server and loader while the node
// never switched. The columns as they were before are put back - only for the
// setup they were recorded for, and only once.
func TestAFailedSetupPutsTheServerBack(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx := context.Background()
	st := &setupBeforeStore{}
	srv := &models.Server{ID: 3, UUID: "srv-3", ActiveSubServer: "survival", InstallerType: "paper", MinecraftVersion: "1.21.1"}

	RememberSetupBefore(ctx, rdb, st, srv, "modded")
	if RestoreSetupBefore(ctx, rdb, st, srv, "other") {
		t.Fatal("restored for a setup to a different sub-server")
	}
	if !RestoreSetupBefore(ctx, rdb, st, srv, "modded") {
		t.Fatal("did not restore the setup it recorded")
	}
	if len(st.written) != 1 || st.written[0] != "survival|paper|1.21.1" {
		t.Fatalf("restored %v, want the previous sub-server and loader", st.written)
	}
	if RestoreSetupBefore(ctx, rdb, st, srv, "modded") {
		t.Fatal("restored a second time")
	}
}

// The install record names the version the edit form and the mod checks use,
// and it is written before the command goes out like the servers row. A
// failed install puts it back too, or removes the one the install wrote.
func TestAFailedSetupPutsTheInstallRecordBack(t *testing.T) {
	for _, c := range []struct {
		name   string
		before *models.SubServerInstall
	}{
		{"had a record", &models.SubServerInstall{ServerID: 3, SubServerName: "modded", InstallerType: "paper", McVersion: "1.21.1"}},
		{"had none", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			rdb := newQueueTestRedis(t)
			ctx := context.Background()
			st := &setupBeforeStore{install: c.before}
			srv := &models.Server{ID: 3, UUID: "srv-3", ActiveSubServer: "modded", InstallerType: "paper", MinecraftVersion: "1.21.1"}

			RememberSetupBefore(ctx, rdb, st, srv, "modded")
			st.install = &models.SubServerInstall{ServerID: 3, SubServerName: "modded", InstallerType: "paper", McVersion: "1.21.4"}
			if !RestoreSetupBefore(ctx, rdb, st, srv, "modded") {
				t.Fatal("did not restore the setup it recorded")
			}
			if c.before == nil {
				if st.install != nil {
					t.Fatalf("install record = %+v, want it removed", st.install)
				}
				return
			}
			if st.install == nil || st.install.ServerID != 3 || st.install.McVersion != "1.21.1" {
				t.Fatalf("install record = %+v, want the one from before", st.install)
			}
		})
	}
}

func (s *setupBeforeStore) UpdateServerDesiredState(id int, state string) error {
	s.desired = append(s.desired, state)
	return nil
}

// A setup or reinstall flips the intent to online before it is dispatched. One
// the node refused or that failed installed nothing, and the reconciler then
// started a server its owner had stopped. A failure puts a stopped intent back,
// unless something since has already changed it from the online the dispatch
// wrote.
func TestAFailedSetupPutsAStoppedIntentBack(t *testing.T) {
	for _, c := range []struct {
		name, before, now string
		want              []string // desired-state writes
		wantRedis         string
	}{
		{"was stopped", "stopped", "online", []string{"stopped"}, "stopped"},
		{"was online", "online", "online", nil, ""},
		{"stopped again since", "stopped", "stopped", nil, ""},
		{"recorded by an older core", "", "online", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			rdb := newQueueTestRedis(t)
			ctx := context.Background()
			st := &setupBeforeStore{}
			srv := &models.Server{ID: 3, UUID: "srv-3", ActiveSubServer: "survival", DesiredState: c.before}

			RememberSetupBefore(ctx, rdb, st, srv, "survival")
			now := *srv
			now.DesiredState = c.now
			if !RestoreSetupBefore(ctx, rdb, st, &now, "survival") {
				t.Fatal("did not restore the setup it recorded")
			}
			if len(st.desired) != len(c.want) || (len(c.want) > 0 && st.desired[0] != c.want[0]) {
				t.Fatalf("desired-state writes = %v, want %v", st.desired, c.want)
			}
			got, _ := rdb.Get(ctx, DesiredStateKey("srv-3")).Result()
			if got != c.wantRedis {
				t.Fatalf("published desired_state = %q, want %q", got, c.wantRedis)
			}
		})
	}
}

func (s *setupBeforeStore) ListServerMods(serverID int, sub string) ([]models.ServerMod, error) {
	return append([]models.ServerMod(nil), s.mods...), nil
}

// As the real store: new ids, and a pin only carried over from rows that are
// still there.
func (s *setupBeforeStore) ReplaceServerMods(serverID int, sub string, rows []models.ServerMod) error {
	s.replaced++
	s.mods = nil
	for i, m := range rows {
		m.ID, m.UpdatesPaused = 100+i, false
		s.mods = append(s.mods, m)
	}
	return nil
}

func (s *setupBeforeStore) SetServerModUpdatesPaused(id, serverID int, paused bool) error {
	for i := range s.mods {
		if s.mods[i].ID == id {
			s.mods[i].UpdatesPaused = paused
		}
	}
	return nil
}

func (s *setupBeforeStore) GetServerByUUID(uuid string) (*models.Server, error) { return s.srv, nil }
func (s *setupBeforeStore) GetNodeByID(id int) (*models.Node, error) {
	return &models.Node{ID: id, Token: "node-tok"}, nil
}
func (s *setupBeforeStore) InsertNotification(n *models.Notification) (int64, error) { return 1, nil }

// Core drops the mod rows of a setup that wipes mods/ when it dispatches it.
// A setup that failed with the files back as they were (refused, or rolled
// back by the node's stash) left jars that loaded with no rows to manage
// them; those rows come back. Only for a setup that recorded them, to the
// sub-server it recorded them for, with the pins they had.
func TestASetupThatLeftTheFilesPutsTheModRowsBack(t *testing.T) {
	before := []models.ServerMod{
		{ID: 1, ServerID: 3, SubServerName: "modded", ModrinthProjectID: "sodium", FileName: "sodium.jar", TargetDir: "mods"},
		{ID: 2, ServerID: 3, SubServerName: "modded", ModrinthProjectID: "lithium", FileName: "lithium.jar", TargetDir: "mods", UpdatesPaused: true},
	}
	for _, c := range []struct {
		name     string
		snapshot bool   // the setup recorded the rows (it wipes mods)
		restore  string // the sub-server the failure is about
		want     bool
	}{
		{"recorded", true, "modded", true},
		{"not recorded (reinstall, older core)", false, "modded", false},
		{"another sub-server", true, "other", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rdb := newQueueTestRedis(t)
			ctx := context.Background()
			st := &setupBeforeStore{mods: append([]models.ServerMod(nil), before...)}
			srv := &models.Server{ID: 3, UUID: "srv-3", ActiveSubServer: "modded"}

			RememberSetupBefore(ctx, rdb, st, srv, "modded")
			if c.snapshot {
				RememberSetupMods(ctx, rdb, st, srv, "modded")
			}
			st.mods = []models.ServerMod{{ID: 9, ModrinthProjectID: "template-mod"}} // dropped, template rows written
			if got := RestoreSetupMods(ctx, rdb, st, srv, c.restore); got != c.want {
				t.Fatalf("RestoreSetupMods = %v, want %v", got, c.want)
			}
			if !c.want {
				if st.replaced != 0 {
					t.Fatal("rows were replaced")
				}
				return
			}
			if len(st.mods) != 2 || st.mods[0].ModrinthProjectID != "sodium" || st.mods[1].ModrinthProjectID != "lithium" {
				t.Fatalf("rows = %+v, want the two from before", st.mods)
			}
			if st.mods[0].UpdatesPaused || !st.mods[1].UpdatesPaused {
				t.Errorf("pins = %v/%v, want false/true", st.mods[0].UpdatesPaused, st.mods[1].UpdatesPaused)
			}
			// The rest of the record is still there for RestoreSetupBefore.
			if !RestoreSetupBefore(ctx, rdb, st, srv, "modded") {
				t.Error("the setup record was consumed")
			}
		})
	}
}

// The node says "restored" when the files are back; an older node never does,
// and then the rows stay forgotten, as its wipe left nothing behind them.
func TestASetupFailureReportPutsTheRowsBackOnlyWhenTheFilesAre(t *testing.T) {
	for _, restored := range []bool{true, false} {
		rdb := newQueueTestRedis(t)
		ctx := context.Background()
		srv := &models.Server{ID: 3, UUID: "srv-3", NodeID: 1, ActiveSubServer: "modded"}
		st := &setupBeforeStore{srv: srv, mods: []models.ServerMod{{ID: 1, ModrinthProjectID: "sodium", TargetDir: "mods"}}}
		RememberSetupBefore(ctx, rdb, st, srv, "modded")
		RememberSetupMods(ctx, rdb, st, srv, "modded")
		st.mods = nil

		s := NewSetupResultService(st, rdb, nil, nil)
		s.apply(queue.SetupResultsChannel("node-tok"), setupReport{ServerUUID: "srv-3", SubServer: "modded", Error: "download failed", Restored: restored})
		if got := len(st.mods) == 1; got != restored {
			t.Errorf("restored=%v: rows back = %v", restored, got)
		}
		if len(st.written) != 1 {
			t.Errorf("restored=%v: setup columns restored %d times, want 1", restored, len(st.written))
		}
	}
}
