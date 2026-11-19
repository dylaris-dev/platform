package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
)

type switchFailFakeStore struct {
	statusWatcherFakeStore

	active   []string
	loader   [][3]string
	notes    []models.Notification
	desired  []string
	installs map[string]models.SubServerInstall
}

func (f *switchFailFakeStore) UpdateServerActiveSubServer(id int, sub string) error {
	f.active = append(f.active, sub)
	return nil
}
func (f *switchFailFakeStore) UpdateServerLoaderMetadata(id int, it, mv, build string) error {
	f.loader = append(f.loader, [3]string{it, mv, build})
	return nil
}
func (f *switchFailFakeStore) InsertNotification(n *models.Notification) (int64, error) {
	f.notes = append(f.notes, *n)
	return 1, nil
}

// R30-01/R30-04: a switch the node could not make puts Core back on the
// sub-server that is still running, with its loader columns, and tells the
// owner. A report about a switch Core has since moved past changes nothing.
func TestConsumeSwitchSubFailures(t *testing.T) {
	const paperUndo = `{"target":"modded","previous":"main","installerType":"paper","minecraftVersion":"1.21.4","buildNumber":"100"}`
	for _, c := range []struct {
		name, report, undo string
		wantActive         string // "" = nothing restored
		wantLoader         bool
	}{
		{"undo record", `{"target":"modded","previous":"main","reason":"x"}`, paperUndo, "main", true},
		{"no undo, node names the previous", `{"target":"modded","previous":"main","reason":"x"}`, "", "main", false},
		{"undo for another switch", `{"target":"modded","previous":"main","reason":"x"}`, `{"target":"other","previous":"x"}`, "main", false},
		{"Core switched again since", `{"target":"old","previous":"main","reason":"x"}`, paperUndo, "", false},
		{"previous unknown", `{"target":"modded","previous":"","reason":"x"}`, "", "", false},
		{"previous not a name", `{"target":"modded","previous":"../x","reason":"x"}`, "", "", false},
		{"unreadable", `not json`, paperUndo, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs := &switchFailFakeStore{}
			fs.serversByUUID = map[string]models.Server{
				"u-1": {ID: 7, UUID: "u-1", Name: "SMP", OwnerID: "owner-1", ActiveSubServer: "modded"},
			}
			s := &StatusWatcherService{store: fs, redis: newQueueTestRedis(t)}
			ctx := context.Background()
			s.redis.Set(ctx, SwitchSubFailedKey("u-1"), c.report, 0)
			if c.undo != "" {
				s.redis.Set(ctx, SwitchUndoKey("u-1"), c.undo, 0)
			}

			changed := s.consumeSwitchSubFailures(ctx)

			if n, _ := s.redis.Exists(ctx, SwitchSubFailedKey("u-1")).Result(); n != 0 {
				t.Error("the report was left to be applied again on the next tick")
			}
			if c.wantActive == "" {
				if changed || len(fs.active)+len(fs.loader)+len(fs.notes) != 0 {
					t.Errorf("acted on it: active=%v loader=%v notes=%d", fs.active, fs.loader, len(fs.notes))
				}
				return
			}
			if !changed {
				t.Error("reported nothing changed")
			}
			if len(fs.active) != 1 || fs.active[0] != c.wantActive {
				t.Errorf("active_sub_server writes = %v, want [%s]", fs.active, c.wantActive)
			}
			if c.wantLoader != (len(fs.loader) == 1 && fs.loader[0] == [3]string{"paper", "1.21.4", "100"}) {
				t.Errorf("loader writes = %v, want restored = %v", fs.loader, c.wantLoader)
			}
			if len(fs.notes) != 1 || fs.notes[0].UserID != "owner-1" {
				t.Errorf("notifications = %+v, want one to the owner", fs.notes)
			}
			if n, _ := s.redis.Exists(ctx, SwitchUndoKey("u-1")).Result(); n != 0 {
				t.Error("the undo record was left behind")
			}
		})
	}
}

func (f *switchFailFakeStore) UpdateServerDesiredState(id int, state string) error {
	f.desired = append(f.desired, state)
	return nil
}

// R31-01/R31-02: a switch that did not happen puts back the run intent the
// dispatch overwrote, so the reconciler does not start the old sub-server an
// owner had stopped. Only over the online the dispatch wrote.
// R31-03: a previous sub-server deleted since the switch is not restored.
func TestConsumeSwitchSubFailures_IntentAndDeletedPrevious(t *testing.T) {
	const report = `{"target":"modded","previous":"main","reason":"x"}`
	for _, c := range []struct {
		name, undo, rowDesired string
		deletePrevious         bool
		wantActive             string
		wantDesired            string // "" = not written
		wantStatus             string // "" = not written
	}{
		{"stopped before, row online", `{"target":"modded","previous":"main","desiredState":"stopped"}`, "online", false, "main", "stopped", ""},
		{"online before", `{"target":"modded","previous":"main","desiredState":"online"}`, "online", false, "main", "", ""},
		{"stopped since", `{"target":"modded","previous":"main","desiredState":"stopped"}`, "stopped", false, "main", "", ""},
		{"undo from an older Core", `{"target":"modded","previous":"main"}`, "online", false, "main", "", ""},
		{"previous deleted", `{"target":"modded","previous":"main","desiredState":"online"}`, "online", true, "", "stopped", "pending_setup"},
		// R41-01: a chained record names the first switch's previous, and the
		// node is on the first target: that switch landed and started it.
		{"an earlier switch landed", `{"target":"modded","previous":"a","desiredState":"stopped"}`, "online", false, "main", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs := &switchFailFakeStore{}
			fs.serversByUUID = map[string]models.Server{
				"u-1": {ID: 7, UUID: "u-1", Name: "SMP", OwnerID: "owner-1", ActiveSubServer: "modded", DesiredState: c.rowDesired},
			}
			s := &StatusWatcherService{store: fs, redis: newQueueTestRedis(t)}
			ctx := context.Background()
			s.redis.Set(ctx, SwitchSubFailedKey("u-1"), report, 0)
			s.redis.Set(ctx, SwitchUndoKey("u-1"), c.undo, time.Minute)
			if c.deletePrevious {
				MarkSwitchPreviousDeleted(ctx, s.redis, "u-1", "MAIN")
				MarkSwitchPreviousDeleted(ctx, s.redis, "u-1", "other") // not the previous: no effect
			}

			if !s.consumeSwitchSubFailures(ctx) {
				t.Fatal("reported nothing changed")
			}
			if len(fs.active) != 1 || fs.active[0] != c.wantActive {
				t.Errorf("active_sub_server writes = %q, want [%q]", fs.active, c.wantActive)
			}
			var gotDesired, gotStatus string
			if len(fs.desired) > 0 {
				gotDesired = fs.desired[len(fs.desired)-1]
			}
			if len(fs.statusCalls) > 0 {
				gotStatus = fs.statusCalls[len(fs.statusCalls)-1].status
			}
			if gotDesired != c.wantDesired || gotStatus != c.wantStatus {
				t.Errorf("desired=%q status=%q, want desired=%q status=%q", gotDesired, gotStatus, c.wantDesired, c.wantStatus)
			}
			pub, _ := s.redis.Get(ctx, DesiredStateKey("u-1")).Result()
			if pub != c.wantDesired {
				t.Errorf("published desired_state = %q, want %q", pub, c.wantDesired)
			}
			if c.deletePrevious && (len(fs.notes) != 1 || !strings.Contains(fs.notes[0].Body, "deleted")) {
				t.Errorf("notification = %+v, want one saying the previous sub-server was deleted", fs.notes)
			}
		})
	}
}

// R32-01: a switch from a server with no active sub-server (its active one was
// deleted) that the node refused is put back to "none active", like a deleted
// previous: the report and the undo record both name no previous, and that is
// a known state, not an unknown one.
func TestConsumeSwitchSubFailures_NoneActiveBefore(t *testing.T) {
	for _, c := range []struct {
		name, undo  string
		wantChanged bool
	}{
		{"undo names none active", `{"target":"modded","previous":"","desiredState":"stopped"}`, true},
		{"no undo record", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs := &switchFailFakeStore{}
			fs.serversByUUID = map[string]models.Server{
				"u-1": {ID: 7, UUID: "u-1", Name: "SMP", OwnerID: "owner-1", ActiveSubServer: "modded", DesiredState: "online"},
			}
			s := &StatusWatcherService{store: fs, redis: newQueueTestRedis(t)}
			ctx := context.Background()
			s.redis.Set(ctx, SwitchSubFailedKey("u-1"), `{"target":"modded","previous":"","reason":"x"}`, 0)
			if c.undo != "" {
				s.redis.Set(ctx, SwitchUndoKey("u-1"), c.undo, time.Minute)
			}

			if got := s.consumeSwitchSubFailures(ctx); got != c.wantChanged {
				t.Fatalf("changed = %v, want %v", got, c.wantChanged)
			}
			if !c.wantChanged {
				return
			}
			if len(fs.active) != 1 || fs.active[0] != "" {
				t.Errorf("active_sub_server writes = %q, want [\"\"]", fs.active)
			}
			if len(fs.statusCalls) == 0 || fs.statusCalls[len(fs.statusCalls)-1].status != "pending_setup" {
				t.Errorf("status writes = %+v, want pending_setup", fs.statusCalls)
			}
			if len(fs.desired) == 0 || fs.desired[len(fs.desired)-1] != "stopped" {
				t.Errorf("desired writes = %q, want stopped", fs.desired)
			}
			if pub, _ := s.redis.Get(ctx, DesiredStateKey("u-1")).Result(); pub != "stopped" {
				t.Errorf("published desired_state = %q, want stopped", pub)
			}
			if len(fs.notes) != 1 || fs.notes[0].UserID != "owner-1" {
				t.Errorf("notifications = %+v, want one to the owner", fs.notes)
			}
			if n, _ := s.redis.Exists(ctx, SwitchUndoKey("u-1")).Result(); n != 0 {
				t.Error("the undo record was left behind")
			}
		})
	}
}

// R32-04: the undo record is what a refusal is put back from, and a switch to
// a node that is away or backlogged is refused long after it was queued. The
// node deletes the record once the switch lands, so its TTL only bounds a
// command that is never delivered.
func TestSwitchUndoOutlivesAQueuedSwitch(t *testing.T) {
	if SwitchUndoTTL < 24*time.Hour {
		t.Errorf("SwitchUndoTTL = %v, want at least a day", SwitchUndoTTL)
	}
}

func (f *switchFailFakeStore) GetSubServerInstall(_ int, sub string) (*models.SubServerInstall, error) {
	if in, ok := f.installs[sub]; ok {
		return &in, nil
	}
	return nil, errors.New("no install record")
}

// R40-01: two switches refused back to back. The node reports both into one
// key, so Core reads only the second, whose undo record names the first
// target as previous. The record a chained switch writes keeps the first
// pending switch's previous, loader columns and run intent, so the refusal is
// put back to what really runs.
func TestChainedSwitchUndo(t *testing.T) {
	const pendingB = `{"target":"b","previous":"a","installerType":"paper","minecraftVersion":"1.21.4","buildNumber":"100","desiredState":"stopped"}`
	// What SwitchSubServer would write for C from a row already moved to B.
	next := SwitchUndo{Target: "c", Previous: "b", InstallerType: "fabric", MinecraftVersion: "1.20.1", BuildNumber: "0.15", DesiredState: "online", At: 5}
	for _, c := range []struct {
		name, pending string
		want          SwitchUndo
	}{
		{"no pending switch", "", next},
		{"pending switch to the active sub-server", pendingB, SwitchUndo{Target: "c", Previous: "a", InstallerType: "paper", MinecraftVersion: "1.21.4", BuildNumber: "100", DesiredState: "stopped", At: 5}},
		{"pending switch overtaken by a setup", `{"target":"b","previous":"a","desiredState":"stopped","overtaken":true}`, next},
		{"pending switch to another sub-server", `{"target":"x","previous":"a","desiredState":"stopped"}`, next},
		{"pending switch from a deleted previous", `{"target":"b","previous":"a","desiredState":"stopped","previousDeleted":true}`, SwitchUndo{Target: "c", Previous: "a", DesiredState: "stopped", PreviousDeleted: true, At: 5}},
		{"unreadable", `nope`, next},
	} {
		t.Run(c.name, func(t *testing.T) {
			rdb := newQueueTestRedis(t)
			ctx := context.Background()
			if c.pending != "" {
				rdb.Set(ctx, SwitchUndoKey("u-1"), c.pending, time.Minute)
			}
			if got := ChainSwitchUndo(ctx, rdb, "u-1", next); got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
	if got := ChainSwitchUndo(context.Background(), nil, "u-1", next); got != next {
		t.Errorf("no Redis: got %+v, want %+v", got, next)
	}
}

// R40-01: a refusal whose undo record does not name the sub-server the node
// went back to still puts that sub-server's own loader columns back, from its
// install record, instead of leaving the refused target's.
func TestConsumeSwitchSubFailures_UnmatchedPreviousTakesItsInstallRecord(t *testing.T) {
	for _, c := range []struct {
		name       string
		installs   map[string]models.SubServerInstall
		wantLoader [][3]string
	}{
		{"install record", map[string]models.SubServerInstall{"main": {InstallerType: "paper", McVersion: "1.21.4", BuildVersion: "100"}}, [][3]string{{"paper", "1.21.4", "100"}}},
		{"no install record", nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs := &switchFailFakeStore{installs: c.installs}
			fs.serversByUUID = map[string]models.Server{
				"u-1": {ID: 7, UUID: "u-1", Name: "SMP", OwnerID: "owner-1", ActiveSubServer: "c", InstallerType: "fabric"},
			}
			s := &StatusWatcherService{store: fs, redis: newQueueTestRedis(t)}
			ctx := context.Background()
			s.redis.Set(ctx, SwitchSubFailedKey("u-1"), `{"target":"c","previous":"main","reason":"x"}`, 0)
			s.redis.Set(ctx, SwitchUndoKey("u-1"), `{"target":"c","previous":"b","installerType":"fabric"}`, time.Minute)

			if !s.consumeSwitchSubFailures(ctx) {
				t.Fatal("reported nothing changed")
			}
			if len(fs.active) != 1 || fs.active[0] != "main" {
				t.Errorf("active_sub_server writes = %q, want [main]", fs.active)
			}
			if fmt.Sprint(fs.loader) != fmt.Sprint(c.wantLoader) {
				t.Errorf("loader writes = %v, want %v", fs.loader, c.wantLoader)
			}
		})
	}
}
