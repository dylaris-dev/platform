package servers

import (
	"context"
	"encoding/json"
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
