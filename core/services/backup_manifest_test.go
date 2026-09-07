package services

import (
	"encoding/json"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

type manifestFakeStore struct {
	store.Store

	installs   map[string]*models.SubServerInstall
	allInstall []models.SubServerInstall
	mods       map[string][]models.ServerMod
	modSubs    []string

	run *models.BackupRun

	// Captured for assertions.
	replaced     []replacedMods
	manifestSet  string
	replaceCalls int
}

type replacedMods struct {
	serverID  int
	subServer string
	mods      []models.ServerMod
}

func (f *manifestFakeStore) GetSubServerInstall(_ int, sub string) (*models.SubServerInstall, error) {
	return f.installs[sub], nil
}

func (f *manifestFakeStore) ListSubServerInstalls(int) ([]models.SubServerInstall, error) {
	return f.allInstall, nil
}

func (f *manifestFakeStore) ListServerMods(_ int, sub string) ([]models.ServerMod, error) {
	return f.mods[sub], nil
}

func (f *manifestFakeStore) ListServerModSubServers(int) ([]string, error) {
	return f.modSubs, nil
}

func (f *manifestFakeStore) GetBackupRun(int) (*models.BackupRun, error) {
	return f.run, nil
}

func (f *manifestFakeStore) ReplaceServerMods(serverID int, sub string, mods []models.ServerMod) error {
	f.replaceCalls++
	f.replaced = append(f.replaced, replacedMods{serverID: serverID, subServer: sub, mods: mods})
	return nil
}

func (f *manifestFakeStore) SetBackupRunManifest(_ int, m string) error {
	f.manifestSet = m
	return nil
}

// A scoped job describes exactly the sub-server it archives, and nothing else.
func TestBuildBackupManifest_ScopedJobCoversOneSubServer(t *testing.T) {
	st := &manifestFakeStore{
		installs: map[string]*models.SubServerInstall{
			"survival": {SubServerName: "survival", InstallerType: "fabric", McVersion: "1.21.1"},
			"creative": {SubServerName: "creative", InstallerType: "paper"},
		},
		mods: map[string][]models.ServerMod{
			"survival": {{ModrinthProjectID: "sodium", FileName: "sodium.jar", Status: models.ServerModInstalled}},
			"creative": {{ModrinthProjectID: "vault", FileName: "vault.jar", Status: models.ServerModInstalled}},
		},
	}
	m := BuildBackupManifest(st, 7, "survival", "2026.09.07")

	if m.Scope != "survival" {
		t.Errorf("Scope = %q, want survival", m.Scope)
	}
	if len(m.Mods) != 1 || m.Mods[0].SubServer != "survival" {
		t.Fatalf("Mods = %+v, want exactly the survival entry", m.Mods)
	}
	if len(m.Installs) != 1 || m.Installs[0].SubServerName != "survival" {
		t.Fatalf("Installs = %+v, want exactly survival", m.Installs)
	}
}

// The empty list is the load-bearing case. A sub-server with no mods must still
// APPEAR in the manifest, because "there were none" is what makes a restore
// remove a mod installed after the backup. Leaving it out would be
// indistinguishable from "this archive says nothing", which changes nothing.
func TestBuildBackupManifest_SubServerWithNoModsStillGetsAnEntry(t *testing.T) {
	st := &manifestFakeStore{
		installs: map[string]*models.SubServerInstall{"survival": {SubServerName: "survival"}},
		mods:     map[string][]models.ServerMod{},
	}
	m := BuildBackupManifest(st, 7, "survival", "")

	if len(m.Mods) != 1 {
		t.Fatalf("Mods = %+v, want one entry for a sub-server with no mods", m.Mods)
	}
	if m.Mods[0].SubServer != "survival" || len(m.Mods[0].Mods) != 0 {
		t.Errorf("entry = %+v, want survival with an empty list", m.Mods[0])
	}
}

// A whole-server job covers the UNION. server_mods and sub_server_installs are
// written by different paths, so either source alone leaves the other's
// sub-servers undescribed - and an undescribed sub-server is one a restore
// silently leaves diverged.
func TestBuildBackupManifest_WholeServerTakesTheUnionOfBothSources(t *testing.T) {
	st := &manifestFakeStore{
		allInstall: []models.SubServerInstall{{SubServerName: "has-record"}},
		modSubs:    []string{"has-mods-only"},
		mods: map[string][]models.ServerMod{
			"has-mods-only": {{ModrinthProjectID: "lithium", FileName: "lithium.jar", Status: models.ServerModInstalled}},
		},
		installs: map[string]*models.SubServerInstall{"has-record": {SubServerName: "has-record"}},
	}
	m := BuildBackupManifest(st, 7, "", "")

	got := map[string]bool{}
	for _, e := range m.Mods {
		got[e.SubServer] = true
	}
	if !got["has-record"] || !got["has-mods-only"] {
		t.Fatalf("covered = %v, want both the install-record and the mods-only sub-server", got)
	}
}

// A mod the node never confirmed is not a state to restore into: replaying it
// would assert a jar the failed install never wrote, into a directory the
// restore has just replaced.
func TestBuildBackupManifest_SkipsUnconfirmedInstalls(t *testing.T) {
	st := &manifestFakeStore{
		installs: map[string]*models.SubServerInstall{"survival": {SubServerName: "survival"}},
		mods: map[string][]models.ServerMod{"survival": {
			{ModrinthProjectID: "ok", FileName: "ok.jar", Status: models.ServerModInstalled},
			{ModrinthProjectID: "broken", FileName: "broken.jar", Status: models.ServerModFailed},
			{ModrinthProjectID: "pending", FileName: "pending.jar", Status: models.ServerModInstalling},
		}},
	}
	m := BuildBackupManifest(st, 7, "survival", "")

	if len(m.Mods) != 1 || len(m.Mods[0].Mods) != 1 || m.Mods[0].Mods[0].ModrinthProjectID != "ok" {
		t.Fatalf("mods = %+v, want only the confirmed install", m.Mods)
	}
}

// The manifest leaves one platform and is read by another, so it must not carry
// anything that identifies a person or a row on the source instance.
func TestBackupManifest_CarriesNoInstanceLocalIdentity(t *testing.T) {
	who := "aaaaaaaa-1111-4111-8111-111111111111"
	st := &manifestFakeStore{
		installs: map[string]*models.SubServerInstall{"survival": {SubServerName: "survival"}},
		mods: map[string][]models.ServerMod{"survival": {{
			ID: 4242, ServerID: 7, ModrinthProjectID: "sodium", FileName: "sodium.jar",
			InstalledBy: &who, InstallID: "install-99", Status: models.ServerModInstalled,
		}}},
	}
	raw := string(EncodeBackupManifest(BuildBackupManifest(st, 7, "survival", "")))

	for _, leak := range []string{who, "install-99", "4242"} {
		if strings.Contains(raw, leak) {
			t.Errorf("manifest contains %q; it identifies the source instance", leak)
		}
	}
}

// Every archive written before manifests existed has none. That must change
// nothing at all, rather than clear a server's mod list on the strength of a
// description that does not exist.
func TestRestoreMods_NoManifestTouchesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  *models.BackupRun
	}{
		{"empty manifest", &models.BackupRun{ID: 1}},
		{"unreadable manifest", &models.BackupRun{ID: 1, Manifest: "{not json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &manifestFakeStore{run: tc.run}
			(&BackupScheduler{store: st}).restoreMods(1, 7)
			if st.replaceCalls != 0 {
				t.Errorf("ReplaceServerMods called %d times, want 0", st.replaceCalls)
			}
		})
	}
}

// An entry with an empty list CLEARS its sub-server, and a sub-server the
// manifest does not mention is left alone. Those two must not collapse into each
// other: the first is the archive saying "there were none", the second is it
// saying nothing.
func TestRestoreMods_EmptyEntryClearsAndAbsentEntryDoesNot(t *testing.T) {
	blob, _ := json.Marshal(&BackupManifest{
		Schema: BackupManifestSchema,
		Mods: []BackupManifestMods{
			{SubServer: "survival", Mods: []BackupManifestMod{}},
			{SubServer: "creative", Mods: []BackupManifestMod{{ModrinthProjectID: "vault", FileName: "vault.jar"}}},
		},
	})
	st := &manifestFakeStore{run: &models.BackupRun{ID: 1, Manifest: string(blob)}}
	(&BackupScheduler{store: st}).restoreMods(1, 7)

	if len(st.replaced) != 2 {
		t.Fatalf("replaced %d scopes, want 2 (nether is absent and must not be touched)", len(st.replaced))
	}
	if st.replaced[0].subServer != "survival" || len(st.replaced[0].mods) != 0 {
		t.Errorf("survival = %+v, want cleared", st.replaced[0])
	}
	if st.replaced[1].subServer != "creative" || len(st.replaced[1].mods) != 1 {
		t.Errorf("creative = %+v, want one row", st.replaced[1])
	}
}

// An archive can be restored onto a DIFFERENT server. The rows must land on the
// server being restored, never on whatever the backup was taken from.
func TestRestoreMods_WritesToTheRestoredServer(t *testing.T) {
	blob, _ := json.Marshal(&BackupManifest{
		Schema: BackupManifestSchema,
		Mods:   []BackupManifestMods{{SubServer: "survival", Mods: []BackupManifestMod{{ModrinthProjectID: "sodium"}}}},
	})
	st := &manifestFakeStore{run: &models.BackupRun{ID: 1, Manifest: string(blob)}}
	(&BackupScheduler{store: st}).restoreMods(1, 99)

	if len(st.replaced) != 1 || st.replaced[0].serverID != 99 {
		t.Fatalf("replaced = %+v, want serverID 99 from the restore", st.replaced)
	}
}

// A restored row is a restatement, not an install by a person: no installer, and
// confirmed, because the archive only ever carried confirmed ones.
func TestRestoreMods_RestoredRowsAreConfirmedAndUnattributed(t *testing.T) {
	blob, _ := json.Marshal(&BackupManifest{
		Schema: BackupManifestSchema,
		Mods: []BackupManifestMods{{SubServer: "survival", Mods: []BackupManifestMod{
			{ModrinthProjectID: "sodium", FileName: "sodium.jar", TargetDir: "mods"},
		}}},
	})
	st := &manifestFakeStore{run: &models.BackupRun{ID: 1, Manifest: string(blob)}}
	(&BackupScheduler{store: st}).restoreMods(1, 7)

	got := st.replaced[0].mods[0]
	if got.InstalledBy != nil {
		t.Errorf("InstalledBy = %v, want nil: the archive carries no user id", *got.InstalledBy)
	}
	if got.FileName != "sodium.jar" || got.TargetDir != "mods" {
		t.Errorf("row = %+v, want the archived file name and target dir", got)
	}
}
