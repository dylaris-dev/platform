package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// undoFakeStore is statusWatcherFakeStore plus the two writes an undo makes.
type undoFakeStore struct {
	statusWatcherFakeStore

	loaderCalls []loaderMetadataCall
	modUpserts  []models.ServerMod
	notes       []models.Notification
}

type loaderMetadataCall struct {
	id                                        int
	installerType, minecraftVersion, buildNum string
}

func (f *undoFakeStore) UpdateServerLoaderMetadata(id int, installerType, minecraftVersion, buildNumber string) error {
	f.loaderCalls = append(f.loaderCalls, loaderMetadataCall{id, installerType, minecraftVersion, buildNumber})
	return nil
}

func (f *undoFakeStore) InsertNotification(n *models.Notification) (int64, error) {
	f.notes = append(f.notes, *n)
	return 1, nil
}

func (f *undoFakeStore) UpsertServerMod(m *models.ServerMod) (int, error) {
	f.modUpserts = append(f.modUpserts, *m)
	return len(f.modUpserts), nil
}

func newUndoTest(t *testing.T) (*StatusWatcherService, *undoFakeStore) {
	t.Helper()
	fs := &undoFakeStore{}
	fs.serversByUUID = map[string]models.Server{
		"u-1": {ID: 7, UUID: "u-1", Name: "SMP", OwnerID: "owner-1", ActiveSubServer: "server", MinecraftVersion: "1.21.4"},
	}
	rdb := newQueueTestRedis(t)
	return &StatusWatcherService{store: fs, redis: rdb}, fs
}

func seedUndo(t *testing.T, s *StatusWatcherService, uuid string, undo VersionUpdateUndo) {
	t.Helper()
	raw, err := json.Marshal(undo)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.redis.Set(context.Background(), VersionUpdateUndoKey(uuid), raw, 0).Err(); err != nil {
		t.Fatal(err)
	}
}

// A version move the node refused to make must leave the database describing
// the server that is actually running.
//
// Core writes the new Minecraft version and the new mod rows BEFORE dispatching
// the command, the same ordering a reinstall uses. The node now stages and
// verifies every jar before touching anything, so a download it cannot complete
// aborts with the machine untouched - and those writes then describe a move
// that did not happen. minecraft_version is what the next mod install and the
// next availability check resolve against, so leaving it wrong silently
// installs jars for a version the server is not running.
func TestConsumeVersionUpdateFailures_PutsTheVersionAndTheModsBack(t *testing.T) {
	s, fs := newUndoTest(t)
	ctx := context.Background()

	seedUndo(t, s, "u-1", VersionUpdateUndo{
		InstallerType:    "fabric",
		MinecraftVersion: "1.20.1",
		BuildNumber:      "0.16.9",
		SubServerName:    "server",
		Mods: []VersionUpdateUndoMod{
			{ModrinthProjectID: "AANobbMI", Title: "Sodium", ModrinthVersionID: "v-old", FileName: "sodium-1.20.1.jar", SHA512: "aa", TargetDir: "mods"},
			// The one the move dropped as unavailable: its row was DELETED, so
			// the restore has to recreate it, not just correct it.
			{ModrinthProjectID: "gvQqBUqZ", Title: "Lithium", ModrinthVersionID: "v-lith", FileName: "lithium-1.20.1.jar", SHA512: "bb", TargetDir: "mods"},
		},
	})
	if err := s.redis.Set(ctx, VersionUpdateFailedKey("u-1"), "sodium.jar could not be fetched", 0).Err(); err != nil {
		t.Fatal(err)
	}

	if !s.consumeVersionUpdateFailures(ctx) {
		t.Fatal("reported nothing changed")
	}

	if len(fs.loaderCalls) != 1 {
		t.Fatalf("loader metadata calls = %+v, want exactly one", fs.loaderCalls)
	}
	got := fs.loaderCalls[0]
	if got.id != 7 || got.installerType != "fabric" || got.minecraftVersion != "1.20.1" || got.buildNum != "0.16.9" {
		t.Errorf("restored %+v, want the pre-move fabric/1.20.1/0.16.9 on server 7", got)
	}

	if len(fs.modUpserts) != 2 {
		t.Fatalf("restored %d mod rows, want 2", len(fs.modUpserts))
	}
	for _, m := range fs.modUpserts {
		if m.ServerID != 7 || m.SubServerName != "server" {
			t.Errorf("restored a row onto the wrong server/sub-server: %+v", m)
		}
	}
	if fs.modUpserts[0].ModrinthVersionID != "v-old" || fs.modUpserts[0].FileName != "sodium-1.20.1.jar" {
		t.Errorf("Sodium restored as %+v, want its pre-move version and file name", fs.modUpserts[0])
	}

	// The owner is told why; before, the panel only showed a stopped server.
	if len(fs.notes) != 1 || fs.notes[0].UserID != "owner-1" || !strings.Contains(fs.notes[0].Body, "sodium.jar could not be fetched") {
		t.Errorf("notifications = %+v, want one to the owner carrying the node's reason", fs.notes)
	}

	// Both keys are gone, so the next tick does not restore over a later move.
	for _, key := range []string{VersionUpdateFailedKey("u-1"), VersionUpdateUndoKey("u-1")} {
		if n, _ := s.redis.Exists(ctx, key).Result(); n != 0 {
			t.Errorf("%s survived the restore", key)
		}
	}
}

// A second tick with no signal must do nothing at all. The restore is a
// compensating write; repeating it would overwrite a move the operator has
// since made successfully.
func TestConsumeVersionUpdateFailures_DoesNothingWithoutASignal(t *testing.T) {
	s, fs := newUndoTest(t)
	ctx := context.Background()
	seedUndo(t, s, "u-1", VersionUpdateUndo{MinecraftVersion: "1.20.1"})

	if s.consumeVersionUpdateFailures(ctx) {
		t.Error("reported a change with no give-up signal present")
	}
	if len(fs.loaderCalls) != 0 || len(fs.modUpserts) != 0 {
		t.Errorf("wrote something: loader=%+v mods=%+v", fs.loaderCalls, fs.modUpserts)
	}
	// The undo record is left for the move that is presumably still in flight.
	if n, _ := s.redis.Exists(ctx, VersionUpdateUndoKey("u-1")).Result(); n != 1 {
		t.Error("the undo record was cleared by a tick that had nothing to do")
	}
}

// The signal is cleared even when there is no undo record to act on, or the
// restore cannot proceed. Left in place it would be retried every five seconds
// forever, and a half-applied restore is worse than none.
func TestConsumeVersionUpdateFailures_ClearsTheSignalWithNoUndoRecord(t *testing.T) {
	s, fs := newUndoTest(t)
	ctx := context.Background()
	if err := s.redis.Set(ctx, VersionUpdateFailedKey("u-1"), "download failed", 0).Err(); err != nil {
		t.Fatal(err)
	}

	s.consumeVersionUpdateFailures(ctx)

	if n, _ := s.redis.Exists(ctx, VersionUpdateFailedKey("u-1")).Result(); n != 0 {
		t.Error("the signal was left to be retried on every tick")
	}
	if len(fs.loaderCalls) != 0 {
		t.Errorf("wrote loader metadata with nothing to restore from: %+v", fs.loaderCalls)
	}
}

// A mod with no Modrinth project id has no conflict key, so upserting it would
// insert a duplicate on every restore instead of replacing one.
func TestRestoreServerModsSkipsUnlinkedMods(t *testing.T) {
	s, fs := newUndoTest(t)
	s.restoreServerMods(7, VersionUpdateUndo{
		SubServerName: "server",
		Mods: []VersionUpdateUndoMod{
			{ModrinthProjectID: "AANobbMI", Title: "Sodium"},
			{ModrinthProjectID: "", Title: "somebody-dropped-this.jar"},
			{ModrinthProjectID: "   ", Title: "and-this.jar"},
		},
	})
	if len(fs.modUpserts) != 1 || fs.modUpserts[0].Title != "Sodium" {
		t.Fatalf("restored %+v, want only the linked mod", fs.modUpserts)
	}
}

var _ store.Store = (*undoFakeStore)(nil)
