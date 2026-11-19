package servers

import (
	"context"
	"net/http/httptest"
	"testing"
)

// R39-01: setup and reinstall claimed the 30-second install cooldown before
// most of their refusals, so a refused request blocked start, stop, restart
// and a corrected retry for 30 seconds although nothing was installed. Only a
// dispatched install holds it.
func TestInstallCooldownHeldOnlyByADispatchedInstall(t *testing.T) {
	const key = "dylaris:server:srv-uuid:install-start"
	for _, tc := range []struct {
		name      string
		reinstall bool
		subName   string // setup's name; reinstall's active sub-server
		wantHeld  bool
	}{
		{"setup refused for its name", false, "bad name", false},
		{"setup dispatched", false, "main", true},
		{"reinstall refused without a sub-server", true, "", false},
		{"reinstall dispatched", true, "main", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, fs, rdb := newInstallTest(t)
			rec := httptest.NewRecorder()
			if tc.reinstall {
				fs.server.Status = "stopped"
				fs.server.ActiveSubServer = tc.subName
				h.ReinstallServer(rec, installRequest("reinstall", map[string]any{"javaImage": "ghcr.io/dylaris-dev/platform-mc-java21:latest"}, false))
			} else {
				body := setupBody(map[string]string{"type": "paper", "version": "1.21.1"})
				body["subServerName"] = tc.subName
				h.SetupServer(rec, installRequest("setup", body, false))
			}
			if dispatched := len(queuedInstaller(t, rdb)) > 0; dispatched != tc.wantHeld {
				t.Fatalf("dispatched=%v, want %v (answered %d %s)", dispatched, tc.wantHeld, rec.Code, rec.Body.String())
			}
			n, err := rdb.Exists(context.Background(), key).Result()
			if err != nil {
				t.Fatal(err)
			}
			if held := n == 1; held != tc.wantHeld {
				t.Errorf("cooldown held=%v, want %v (answered %d %s)", held, tc.wantHeld, rec.Code, rec.Body.String())
			}
		})
	}
}
