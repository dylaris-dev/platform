package services

import (
	"encoding/json"
	"testing"

	"dylaris-core/models"
)

type installsFakeStore struct {
	manifestFakeStore
	upserted []models.SubServerInstall
}

func (f *installsFakeStore) UpsertSubServerInstall(rec models.SubServerInstall) error {
	f.upserted = append(f.upserted, rec)
	return nil
}

// R31-05: the install snapshot is written when the upload finishes, hours after
// the archive was packed on a spooled run, so it can describe a version update
// made in between. The manifest is built when the run is dispatched, from the
// same rows the archive was taken under, and is what a restore puts back.
func TestRestoreInstalls_PrefersTheManifestOverTheSuccessTimeSnapshot(t *testing.T) {
	manifest, _ := json.Marshal(&BackupManifest{
		Schema:   BackupManifestSchema,
		Installs: []models.SubServerInstall{{ServerID: 3, SubServerName: "survival", InstallerType: "paper", McVersion: "1.20.1"}},
	})
	snapshot, _ := json.Marshal([]models.SubServerInstall{{ServerID: 3, SubServerName: "survival", InstallerType: "paper", McVersion: "1.21"}})
	for _, tc := range []struct {
		name string
		run  models.BackupRun
		want string // McVersion upserted, "" for nothing
	}{
		{"manifest wins", models.BackupRun{ID: 1, Manifest: string(manifest), InstallSnapshot: string(snapshot)}, "1.20.1"},
		{"no manifest falls back to the snapshot", models.BackupRun{ID: 1, InstallSnapshot: string(snapshot)}, "1.21"},
		{"unreadable manifest falls back to the snapshot", models.BackupRun{ID: 1, Manifest: "{not json", InstallSnapshot: string(snapshot)}, "1.21"},
		{"neither changes nothing", models.BackupRun{ID: 1}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := tc.run
			st := &installsFakeStore{manifestFakeStore: manifestFakeStore{run: &run}}
			(&BackupScheduler{store: st}).restoreInstalls(1, 7)
			if tc.want == "" {
				if len(st.upserted) != 0 {
					t.Fatalf("upserted %+v, want nothing", st.upserted)
				}
				return
			}
			if len(st.upserted) != 1 || st.upserted[0].McVersion != tc.want || st.upserted[0].ServerID != 7 {
				t.Fatalf("upserted %+v, want one record at %s on server 7", st.upserted, tc.want)
			}
		})
	}
}
