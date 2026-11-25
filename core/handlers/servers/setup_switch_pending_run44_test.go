package servers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

// R44-01/R44-02: a setup or reinstall sent while a switch was still on its
// way overtook it and lost the intent from before it, so when both failed the
// reconciler started a server its owner had stopped. They, and the version
// move, now wait until the switch has landed or been refused, as the copy does.
func TestInstallsWaitForAPendingSwitch(t *testing.T) {
	for _, c := range []struct {
		name    string
		undo    string
		release string // the node's heartbeat release, "" for none
		refused bool
	}{
		{"switch pending", `{"target":"main","previous":"old","desiredState":"stopped"}`, "", true},
		{"switch pending, node on the release that forgets it", `{"target":"main","previous":"old","desiredState":"stopped"}`, switchUndoForgetSince, true},
		// R45-01: an older node lands the switch and never deletes the record.
		{"switch pending, node older than that", `{"target":"main","previous":"old","desiredState":"stopped"}`, "2026.11.23", false},
		{"switch overtaken by an earlier setup", `{"target":"main","previous":"old","overtaken":true}`, "", false},
		{"switch landed or refused", "", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			check := func(what string, rec *httptest.ResponseRecorder, queued int64) {
				t.Helper()
				refused := rec.Code == http.StatusConflict && strings.Contains(rec.Body.String(), "switch_pending")
				if refused != c.refused {
					t.Errorf("%s answered %d %s, want refused=%v", what, rec.Code, rec.Body.String(), c.refused)
				}
				if c.refused && queued != 0 {
					t.Errorf("%s queued %d command(s) while refused", what, queued)
				}
				if !c.refused && queued != 1 {
					t.Errorf("%s queued %d command(s), want 1: %d %s", what, queued, rec.Code, rec.Body.String())
				}
			}
			ctx := context.Background()
			pending := func(rdb *redis.Client, uuid string) {
				if c.undo != "" {
					rdb.Set(ctx, services.SwitchUndoKey(uuid), c.undo, time.Hour)
				}
				if c.release != "" {
					hb, _ := json.Marshal(services.NodeHeartbeat{ReleaseVersion: c.release})
					for _, token := range []string{"node-lib", "tok"} {
						rdb.Set(ctx, "dylaris:discovery:"+token, hb, time.Hour)
					}
				}
			}

			h, _, rdb := newInstallTest(t)
			pending(rdb, "srv-uuid")
			rec := httptest.NewRecorder()
			h.SetupServer(rec, installRequest("setup", setupBody(map[string]string{"type": "paper", "version": "1.21.1"}), true))
			check("setup", rec, int64(len(queuedInstaller(t, rdb))))

			h, fs, rdb := newInstallTest(t)
			fs.server.Status = "stopped"
			pending(rdb, "srv-uuid")
			rec = httptest.NewRecorder()
			h.ReinstallServer(rec, installRequest("reinstall", map[string]any{"javaImage": "ghcr.io/dylaris-dev/platform-mc-java21:latest"}, true))
			check("reinstall", rec, int64(len(queuedInstaller(t, rdb))))

			mr := miniredis.RunT(t)
			mrdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			pending(mrdb, "u-1")
			ms := &moveStore{}
			ms.srv = models.Server{ID: 1, UUID: "u-1", NodeID: 3, ActiveSubServer: "main", Status: "stopped",
				InstallerType: "fabric", MinecraftVersion: "1.20.1", GameImage: "img-java17"}
			mh := NewServerModsHandler(testDeps(Deps{Store: ms, Redis: mrdb, Queue: services.NewQueueService(mrdb)}))
			req := httptest.NewRequest(http.MethodPost, "/api/servers/1/version-update",
				strings.NewReader(`{"minecraft":"1.21.1","javaImage":"`+platformImagePrefix+`21"}`))
			req = req.WithContext(context.WithValue(req.Context(), "isAdmin", true))
			rec = httptest.NewRecorder()
			mh.VersionUpdate(rec, mux.SetURLVars(req, map[string]string{"id": "1"}))
			n, _ := mrdb.XLen(ctx, "dylaris:node:tok:cmds").Result()
			check("version move", rec, n)
		})
	}
}
