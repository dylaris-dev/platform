package services

import (
	"context"
	"testing"

	"dylaris-core/models"
)

type switchFailFakeStore struct {
	statusWatcherFakeStore

	active []string
	loader [][3]string
	notes  []models.Notification
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
