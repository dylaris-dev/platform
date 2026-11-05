package services

import (
	"context"
	"testing"

	"dylaris-core/models"
)

type copyFailFakeStore struct {
	statusWatcherFakeStore

	modsCleared     []string
	installsDeleted []string
	contentsCleared []string
	notes           []models.Notification
}

func (f *copyFailFakeStore) ReplaceServerMods(serverID int, sub string, mods []models.ServerMod) error {
	if len(mods) == 0 {
		f.modsCleared = append(f.modsCleared, sub)
	}
	return nil
}
func (f *copyFailFakeStore) DeleteSubServerInstall(serverID int, sub string) error {
	f.installsDeleted = append(f.installsDeleted, sub)
	return nil
}
func (f *copyFailFakeStore) ReplaceServerModpackContents(serverID int, sub string, rows []models.ServerModpackContent) error {
	if len(rows) == 0 {
		f.contentsCleared = append(f.contentsCleared, sub)
	}
	return nil
}
func (f *copyFailFakeStore) InsertNotification(n *models.Notification) (int64, error) {
	f.notes = append(f.notes, *n)
	return 1, nil
}

// A copy the node could not make leaves no sub-server behind in the panel, and
// the owner hears why. A report naming the running sub-server, or a name that
// is not one, removes nothing.
func TestConsumeCopySubFailures(t *testing.T) {
	for _, c := range []struct {
		name       string
		report     string
		wantTarget string // "" = nothing removed
	}{
		{"disk limit", `{"target":"main-copy","reason":"the copy does not fit in the server's disk limit"}`, "main-copy"},
		{"the active sub-server", `{"target":"Main","reason":"x"}`, ""},
		{"not a sub-server name", `{"target":"../x","reason":"x"}`, ""},
		{"unreadable", `not json`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs := &copyFailFakeStore{}
			fs.serversByUUID = map[string]models.Server{
				"u-1": {ID: 7, UUID: "u-1", Name: "SMP", OwnerID: "owner-1", ActiveSubServer: "main"},
			}
			s := &StatusWatcherService{store: fs, redis: newQueueTestRedis(t)}
			ctx := context.Background()
			if err := s.redis.Set(ctx, CopySubFailedKey("u-1"), c.report, 0).Err(); err != nil {
				t.Fatal(err)
			}

			changed := s.consumeCopySubFailures(ctx)

			if n, _ := s.redis.Exists(ctx, CopySubFailedKey("u-1")).Result(); n != 0 {
				t.Error("the report was left to be applied again on the next tick")
			}
			if c.wantTarget == "" {
				if changed || len(fs.modsCleared)+len(fs.installsDeleted)+len(fs.contentsCleared)+len(fs.notes) != 0 {
					t.Errorf("acted on it: mods=%v installs=%v contents=%v notes=%d", fs.modsCleared, fs.installsDeleted, fs.contentsCleared, len(fs.notes))
				}
				return
			}
			if !changed {
				t.Error("reported nothing changed")
			}
			for what, got := range map[string][]string{"mod rows": fs.modsCleared, "install record": fs.installsDeleted, "modpack snapshot": fs.contentsCleared} {
				if len(got) != 1 || got[0] != c.wantTarget {
					t.Errorf("%s removed for %v, want [%s]", what, got, c.wantTarget)
				}
			}
			if len(fs.notes) != 1 || fs.notes[0].UserID != "owner-1" {
				t.Fatalf("notifications = %+v, want one to the owner", fs.notes)
			}
		})
	}
}
