package servers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
)

// Setup writes the new install record before it dispatches. The record a
// failure report restores has to be the one from BEFORE that write: it used
// to be read after it, so a failed setup "restored" the record of the failed
// target, and on a first install never deleted it.
func TestSetupRemembersTheInstallRecordFromBeforeTheSetup(t *testing.T) {
	old := &models.SubServerInstall{ServerID: 1, SubServerName: "main", InstallerType: "paper", McVersion: "1.20.4", BuildVersion: "1.20.4"}
	for _, tt := range []struct {
		name   string
		stored *models.SubServerInstall
	}{
		{"over an installed sub-server", old},
		{"first install", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, fs, rdb := newInstallTest(t)
			fs.storedInstall = tt.stored
			rec := httptest.NewRecorder()
			h.SetupServer(rec, installRequest("setup", setupBody(map[string]string{"type": "fabric", "version": "1.21.1", "mcVersion": "1.21.1"}), false))
			if rec.Code != http.StatusOK {
				t.Fatalf("setup status %d: %s", rec.Code, rec.Body.String())
			}
			raw, err := rdb.Get(context.Background(), "dylaris:server:srv-uuid:setup_before").Result()
			if err != nil {
				t.Fatalf("no setup_before record: %v", err)
			}
			var got struct {
				InstallKnown bool
				Install      *models.SubServerInstall
			}
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatal(err)
			}
			if !got.InstallKnown {
				t.Fatal("the install record was not remembered")
			}
			switch {
			case tt.stored == nil && got.Install != nil:
				t.Errorf("remembered %+v, want no record (a failure must delete the new one)", *got.Install)
			case tt.stored != nil && (got.Install == nil || got.Install.McVersion != "1.20.4" || got.Install.InstallerType != "paper"):
				t.Errorf("remembered %+v, want the paper 1.20.4 record from before the setup", got.Install)
			}
		})
	}
}

type modRowsInstallStore struct {
	*installFakeStore
	mods []models.ServerMod
}

func (f *modRowsInstallStore) ListServerMods(int, string) ([]models.ServerMod, error) {
	return append([]models.ServerMod(nil), f.mods...), nil
}

func (f *modRowsInstallStore) DeleteServerMod(id, _ int) error {
	for i := range f.mods {
		if f.mods[i].ID == id {
			f.mods = append(f.mods[:i], f.mods[i+1:]...)
			return nil
		}
	}
	return nil
}

// A setup that wipes mods/ drops the rows, and records them first: a failure
// that leaves the files as they were (the node says "restored") puts them
// back. Dropped before the command, so a node that refuses at once cannot
// report before Core gets to drop them.
func TestSetupRecordsTheModRowsItDrops(t *testing.T) {
	h, base, rdb := newInstallTest(t)
	fs := &modRowsInstallStore{installFakeStore: base, mods: []models.ServerMod{
		{ID: 5, ServerID: 1, SubServerName: "main", ModrinthProjectID: "sodium", FileName: "sodium.jar", TargetDir: "mods"},
	}}
	h.d.Store = fs
	body := setupBody(nil)
	body["installer"] = map[string]any{"type": "fabric", "version": "1.21.1", "mcVersion": "1.21.1", "wipePaths": []string{"mods"}}

	rec := httptest.NewRecorder()
	h.SetupServer(rec, installRequest("setup", body, false))
	if rec.Code != http.StatusOK {
		t.Fatalf("setup status %d: %s", rec.Code, rec.Body.String())
	}
	if len(fs.mods) != 0 {
		t.Fatalf("rows %+v left, want the mods row dropped", fs.mods)
	}
	before, err := rdb.Get(context.Background(), "dylaris:server:srv-uuid:setup_before").Result()
	if err != nil {
		t.Fatalf("no setup_before record: %v", err)
	}
	var got struct {
		ModsKnown bool
		Mods      []models.ServerMod
	}
	if err := json.Unmarshal([]byte(before), &got); err != nil {
		t.Fatal(err)
	}
	if !got.ModsKnown || len(got.Mods) != 1 || got.Mods[0].ModrinthProjectID != "sodium" {
		t.Fatalf("recorded %+v, want the sodium row from before the setup", got)
	}
}

type setupWriteFailsStore struct{ *installFakeStore }

func (f *setupWriteFailsStore) UpdateServerSetup(int, string, string, string, string, string, string, string) error {
	return errors.New("db down")
}

// R28-02: a setup refused after the install record was written left the new
// record behind with nothing dispatched, so the edit form and the mod checks
// resolved against software that was never installed. The record is now
// written past the last refusal, as reinstall does.
func TestSetupRefusedLateKeepsTheOldInstallRecord(t *testing.T) {
	old := models.SubServerInstall{ServerID: 1, SubServerName: "main", InstallerType: "paper", McVersion: "1.20.4", BuildVersion: "1.20.4"}
	for _, tt := range []struct {
		name   string
		stored *models.SubServerInstall
	}{
		{"over an installed sub-server", &old},
		{"first install", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, base, rdb := newInstallTest(t)
			base.storedInstall = tt.stored
			h.d.Store = &setupWriteFailsStore{base}
			rec := httptest.NewRecorder()
			h.SetupServer(rec, installRequest("setup", setupBody(map[string]string{"type": "fabric", "version": "1.21.1", "mcVersion": "1.21.1"}), false))
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("setup status %d, want 500: %s", rec.Code, rec.Body.String())
			}
			if n := len(queuedInstaller(t, rdb)); n != 0 {
				t.Fatalf("%d installs queued, want none", n)
			}
			if base.installRow != (models.SubServerInstall{}) {
				t.Errorf("install record written as %+v by a setup that was refused", base.installRow)
			}
			if tt.stored != nil && (base.storedInstall == nil || *base.storedInstall != old) {
				t.Errorf("stored record %+v, want the paper 1.20.4 one from before", base.storedInstall)
			}
		})
	}
}
