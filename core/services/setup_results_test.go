package services

import (
	"errors"
	"testing"

	"dylaris-core/models"
)

// fakeImportStore records what an import wrote, so a test can assert on the
// WRITES rather than on a database.
type fakeImportStore struct {
	installs []models.SubServerInstall
	loader   []struct {
		serverID                              int
		installerType, mcVersion, buildNumber string
	}
	mods []struct {
		serverID  int
		subServer string
		rows      []models.ServerMod
	}
	installErr error
}

func (f *fakeImportStore) UpsertSubServerInstall(in models.SubServerInstall) error {
	if f.installErr != nil {
		return f.installErr
	}
	f.installs = append(f.installs, in)
	return nil
}

func (f *fakeImportStore) UpdateServerLoaderMetadata(id int, installerType, minecraftVersion, buildNumber string) error {
	f.loader = append(f.loader, struct {
		serverID                              int
		installerType, mcVersion, buildNumber string
	}{id, installerType, minecraftVersion, buildNumber})
	return nil
}

func (f *fakeImportStore) ReplaceServerMods(serverID int, subServerName string, rows []models.ServerMod) error {
	f.mods = append(f.mods, struct {
		serverID  int
		subServer string
		rows      []models.ServerMod
	}{serverID, subServerName, rows})
	return nil
}

func targetServer() *models.Server {
	return &models.Server{ID: 7, UUID: "target-uuid", ActiveSubServer: "main"}
}

func scopedManifest() *BackupManifest {
	return &BackupManifest{
		Schema: BackupManifestSchema,
		Scope:  "survival",
		Installs: []models.SubServerInstall{{
			ServerID: 99, SubServerName: "survival",
			InstallerType: "fabric", McVersion: "1.21.1", BuildVersion: "0.16.5",
			Loader: "fabric", PackID: 42, PackBuildID: 108,
		}},
		Mods: []BackupManifestMods{{
			SubServer: "survival",
			Mods: []BackupManifestMod{
				{ModrinthProjectID: "spark", ModrinthVersionID: "v1", FileName: "spark.jar", TargetDir: "mods"},
			},
		}},
	}
}

func TestApplyImportedManifestWritesTheTargetsOwnIdentity(t *testing.T) {
	st := &fakeImportStore{}
	ApplyImportedManifest(st, targetServer(), "main", scopedManifest())

	if len(st.installs) != 1 {
		t.Fatalf("wrote %d install records, want 1", len(st.installs))
	}
	got := st.installs[0]
	// The archive says server 99 and sub-server "survival". Both are the SOURCE
	// platform's names; the import is landing on server 7 as "main".
	if got.ServerID != 7 {
		t.Errorf("serverId = %d, want 7 - an id read out of the archive addresses another platform's row", got.ServerID)
	}
	if got.SubServerName != "main" {
		t.Errorf("subServer = %q, want main - the node reports the directory it installed into", got.SubServerName)
	}
	if got.InstallerType != "fabric" || got.McVersion != "1.21.1" || got.BuildVersion != "0.16.5" {
		t.Errorf("the loader description did not survive: %+v", got)
	}
}

// PackID and PackBuildID are primary keys in the database that wrote the
// archive. Carried over, they address whatever happens to hold that id on the
// importing platform, which is somebody else's pack.
func TestApplyImportedManifestDropsTheSourcePlatformsPackIds(t *testing.T) {
	st := &fakeImportStore{}
	ApplyImportedManifest(st, targetServer(), "main", scopedManifest())

	got := st.installs[0]
	if got.PackID != 0 || got.PackBuildID != 0 {
		t.Errorf("packId=%d packBuildId=%d survived the import", got.PackID, got.PackBuildID)
	}
}

// Modrinth ids identify the same project on every platform, so they are exactly
// what SHOULD travel.
func TestApplyImportedManifestKeepsModrinthIdentity(t *testing.T) {
	m := scopedManifest()
	m.Installs[0].ModrinthProjectID = "p"
	m.Installs[0].ModrinthVersionID = "v"
	m.Installs[0].ModrinthProjectSlug = "cool-pack"

	st := &fakeImportStore{}
	ApplyImportedManifest(st, targetServer(), "main", m)

	got := st.installs[0]
	if got.ModrinthProjectID != "p" || got.ModrinthVersionID != "v" || got.ModrinthProjectSlug != "cool-pack" {
		t.Errorf("the Modrinth reference did not survive: %+v", got)
	}
}

func TestApplyImportedManifestWritesTheModRows(t *testing.T) {
	st := &fakeImportStore{}
	ApplyImportedManifest(st, targetServer(), "main", scopedManifest())

	if len(st.mods) != 1 {
		t.Fatalf("wrote %d mod sets, want 1", len(st.mods))
	}
	if st.mods[0].serverID != 7 || st.mods[0].subServer != "main" {
		t.Errorf("mods went to server %d/%q, want 7/main", st.mods[0].serverID, st.mods[0].subServer)
	}
	if len(st.mods[0].rows) != 1 || st.mods[0].rows[0].ModrinthProjectID != "spark" {
		t.Errorf("rows = %+v", st.mods[0].rows)
	}
}

// The server's own columns describe whichever sub-server is ACTIVE. Writing them
// from a description of an inactive one relabels the running server as something
// it is not.
func TestApplyImportedManifestOnlyRelabelsTheActiveSubServer(t *testing.T) {
	cases := []struct {
		name       string
		active     string
		subServer  string
		wantLoader bool
	}{
		{"importing into the active sub-server", "main", "main", true},
		{"importing into an inactive sub-server", "main", "creative", false},
		{"a server with no active sub-server yet", "", "main", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := targetServer()
			srv.ActiveSubServer = tc.active
			st := &fakeImportStore{}
			ApplyImportedManifest(st, srv, tc.subServer, scopedManifest())

			if got := len(st.loader) > 0; got != tc.wantLoader {
				t.Fatalf("loader metadata written = %v, want %v", got, tc.wantLoader)
			}
			// The install record itself is written either way - it is per
			// sub-server, so it says nothing about the running one.
			if len(st.installs) != 1 {
				t.Errorf("wrote %d install records, want 1", len(st.installs))
			}
		})
	}
}

// An import creates ONE sub-server. A description of several cannot be applied
// without choosing, and choosing wrongly writes the wrong loader onto a server
// that then does not boot.
func TestApplyImportedManifestRefusesToGuessBetweenSubServers(t *testing.T) {
	m := &BackupManifest{
		Schema: BackupManifestSchema,
		Scope:  "", // a whole-server archive
		Installs: []models.SubServerInstall{
			{SubServerName: "survival", InstallerType: "fabric"},
			{SubServerName: "creative", InstallerType: "paper"},
		},
		Mods: []BackupManifestMods{
			{SubServer: "survival", Mods: []BackupManifestMod{{ModrinthProjectID: "a"}}},
			{SubServer: "creative", Mods: []BackupManifestMod{{ModrinthProjectID: "b"}}},
		},
	}
	st := &fakeImportStore{}
	ApplyImportedManifest(st, targetServer(), "main", m)

	if len(st.installs) != 0 || len(st.mods) != 0 || len(st.loader) != 0 {
		t.Fatalf("an ambiguous archive was applied anyway: installs=%d mods=%d loader=%d",
			len(st.installs), len(st.mods), len(st.loader))
	}
}

// A whole-server archive of a server that only ever had one sub-server is not
// ambiguous, and refusing it would make the common single-sub-server backup
// unimportable.
func TestApplyImportedManifestAppliesAnUnambiguousWholeServerArchive(t *testing.T) {
	m := &BackupManifest{
		Schema:   BackupManifestSchema,
		Scope:    "",
		Installs: []models.SubServerInstall{{SubServerName: "survival", InstallerType: "paper", McVersion: "1.21.4"}},
		Mods:     []BackupManifestMods{{SubServer: "survival", Mods: nil}},
	}
	st := &fakeImportStore{}
	ApplyImportedManifest(st, targetServer(), "main", m)

	if len(st.installs) != 1 || st.installs[0].InstallerType != "paper" {
		t.Fatalf("installs = %+v", st.installs)
	}
	// An entry with an empty list SAYS there were no mods, and is applied.
	if len(st.mods) != 1 || len(st.mods[0].rows) != 0 {
		t.Fatalf("mods = %+v; an archive that says 'no mods' must clear the rows", st.mods)
	}
}

// An archive that says nothing about mods must leave the rows alone. Only an
// entry with an empty list means "there were none".
func TestApplyImportedManifestLeavesModsAloneWhenTheArchiveIsSilent(t *testing.T) {
	m := scopedManifest()
	m.Mods = nil

	st := &fakeImportStore{}
	ApplyImportedManifest(st, targetServer(), "main", m)

	if len(st.mods) != 0 {
		t.Fatalf("an archive that says nothing about mods still rewrote them: %+v", st.mods)
	}
	if len(st.installs) != 1 {
		t.Errorf("the install record should still have been written")
	}
}

func TestApplyImportedManifestIgnoresAReportWithNoSubServer(t *testing.T) {
	st := &fakeImportStore{}
	ApplyImportedManifest(st, targetServer(), "", scopedManifest())
	if len(st.installs) != 0 || len(st.mods) != 0 {
		t.Fatal("a report naming no sub-server was applied to something")
	}
}

func TestApplyImportedManifestSurvivesNils(t *testing.T) {
	st := &fakeImportStore{}
	ApplyImportedManifest(st, targetServer(), "main", nil)
	ApplyImportedManifest(st, nil, "main", scopedManifest())
	if len(st.installs) != 0 || len(st.mods) != 0 {
		t.Fatal("something was written from nothing")
	}
}

// A failed install write must not be followed by relabelling the server: the
// columns would then describe an install record that does not exist.
func TestApplyImportedManifestDoesNotRelabelWhenTheInstallWriteFails(t *testing.T) {
	st := &fakeImportStore{installErr: errors.New("db down")}
	ApplyImportedManifest(st, targetServer(), "main", scopedManifest())

	if len(st.loader) != 0 {
		t.Fatal("the server was relabelled after the install record failed to save")
	}
}
