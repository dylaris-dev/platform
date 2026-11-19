package servers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/services"
)

// R34-02/R34-06: a setup or reinstall refused before its command goes out
// leaves a pending switch's undo record as it was and writes no setup_before;
// one that goes out marks the switch overtaken instead of deleting its record
// (R34-04), so the switch's refusal is not put back over the install.
func TestASetupOvertakesAPendingSwitchOnlyOnceItIsSent(t *testing.T) {
	const undo = `{"target":"main","previous":"survival","installerType":"paper","desiredState":"stopped"}`
	for _, c := range []struct {
		name, route   string
		refused       bool
		wantCode      int
		wantOvertaken bool
	}{
		{"setup refused", "setup", true, http.StatusInternalServerError, false},
		{"reinstall refused", "reinstall", true, http.StatusInternalServerError, false},
		{"setup sent", "setup", false, http.StatusOK, true},
		{"reinstall sent", "reinstall", false, http.StatusOK, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, fs, rdb := newInstallTest(t)
			if c.route == "reinstall" {
				fs.server.Status = "stopped"
			}
			if c.refused {
				h.d.Store = &setupWriteFailsStore{fs}
			}
			ctx := context.Background()
			rdb.Set(ctx, services.SwitchUndoKey("srv-uuid"), undo, time.Hour)

			rec := httptest.NewRecorder()
			if c.route == "setup" {
				h.SetupServer(rec, installRequest("setup", setupBody(map[string]string{"type": "fabric", "version": "1.21.1", "mcVersion": "1.21.1"}), true))
			} else {
				h.ReinstallServer(rec, installRequest("reinstall", map[string]any{"javaImage": "ghcr.io/dylaris-dev/platform-mc-java21:latest"}, true))
			}
			if rec.Code != c.wantCode {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.wantCode, rec.Body.String())
			}

			raw, err := rdb.Get(ctx, services.SwitchUndoKey("srv-uuid")).Result()
			if err != nil {
				t.Fatalf("the pending switch's undo record is gone: %v", err)
			}
			var got services.SwitchUndo
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatal(err)
			}
			if got.Target != "main" || got.Previous != "survival" || got.Overtaken != c.wantOvertaken {
				t.Errorf("undo record = %s, want the switch's record with overtaken=%v", raw, c.wantOvertaken)
			}
			if n, _ := rdb.Exists(ctx, "dylaris:server:srv-uuid:setup_before").Result(); (n == 1) == c.refused {
				t.Errorf("setup_before present = %v after a %s", n == 1, c.name)
			}
		})
	}
}
