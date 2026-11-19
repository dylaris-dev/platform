package servers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

// R37-01: setup, reinstall and a version move were let through while a
// start, stop or restart was in flight, as their delete and copy siblings are
// not, and the node ran the old JVM beside the install. Refused unless an
// operator overrides, as the power handler is.
func TestInstallsRefuseAServerInAPowerTransition(t *testing.T) {
	for _, tc := range []struct {
		status   string
		operator bool
		refused  bool
	}{
		{"restarting", false, true},
		{"starting", false, true},
		{"stopping", false, true},
		{"restarting", true, false},
		{"stopped", false, false},
	} {
		name := tc.status
		if tc.operator {
			name += "/operator"
		}
		t.Run(name, func(t *testing.T) {
			check := func(what string, rec *httptest.ResponseRecorder, queued int64) {
				t.Helper()
				refused := rec.Code == http.StatusConflict && strings.Contains(rec.Body.String(), "server_in_transition")
				if refused != tc.refused {
					t.Errorf("%s answered %d %s, want refused=%v", what, rec.Code, rec.Body.String(), tc.refused)
				}
				if tc.refused && queued != 0 {
					t.Errorf("%s queued %d command(s) while refused", what, queued)
				}
			}

			h, fs, rdb := newInstallTest(t)
			fs.server.Status = tc.status
			rec := httptest.NewRecorder()
			h.SetupServer(rec, installRequest("setup", setupBody(map[string]string{"type": "paper", "version": "1.21.1"}), tc.operator))
			check("setup", rec, int64(len(queuedInstaller(t, rdb))))

			h, fs, rdb = newInstallTest(t)
			fs.server.Status = tc.status
			rec = httptest.NewRecorder()
			h.ReinstallServer(rec, installRequest("reinstall", map[string]any{"javaImage": "ghcr.io/dylaris-dev/platform-mc-java21:latest"}, tc.operator))
			check("reinstall", rec, int64(len(queuedInstaller(t, rdb))))

			mr := miniredis.RunT(t)
			mrdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			ms := &moveStore{}
			ms.srv = models.Server{ID: 1, UUID: "u-1", NodeID: 3, ActiveSubServer: "main", Status: tc.status,
				InstallerType: "fabric", MinecraftVersion: "1.20.1", GameImage: "img-java17"}
			mh := NewServerModsHandler(testDeps(Deps{Store: ms, Redis: mrdb, Queue: services.NewQueueService(mrdb)}))
			req := httptest.NewRequest(http.MethodPost, "/api/servers/1/version-update",
				strings.NewReader(`{"minecraft":"1.21.1","javaImage":"`+platformImagePrefix+`21"}`))
			req = req.WithContext(context.WithValue(req.Context(), "isAdmin", tc.operator))
			rec = httptest.NewRecorder()
			mh.VersionUpdate(rec, mux.SetURLVars(req, map[string]string{"id": "1"}))
			n, _ := mrdb.XLen(context.Background(), "dylaris:node:tok:cmds").Result()
			check("version move", rec, n)
		})
	}
}

// orderMoveStore notes how many commands were queued when the move rewrote
// its mod rows and its install record.
type orderMoveStore struct {
	moveStore
	rdb             *redis.Client
	queuedAtDrop    int64
	queuedAtInstall int64
	deleted         []int
}

func (f *orderMoveStore) queued() int64 {
	n, _ := f.rdb.XLen(context.Background(), "dylaris:node:tok:cmds").Result()
	return n
}
func (f *orderMoveStore) DeleteServerMod(id, _ int) error {
	f.queuedAtDrop = f.queued()
	f.deleted = append(f.deleted, id)
	return nil
}
func (f *orderMoveStore) UpsertSubServerInstall(in models.SubServerInstall) error {
	f.queuedAtInstall = f.queued()
	return f.moveStore.UpsertSubServerInstall(in)
}

// R37-02: the move rewrote its mod rows and install record after the
// dispatch, so a node refusal whose undo the status watcher had already
// applied was written over, and the rows named jars that were never placed.
// Written before the dispatch, as setup, reinstall and the copy do.
func TestVersionUpdateWritesItsRowsBeforeTheCommand(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	fs := &orderMoveStore{rdb: rdb, queuedAtDrop: -1, queuedAtInstall: -1}
	fs.install = &models.SubServerInstall{ServerID: 1, SubServerName: "main", InstallerType: "fabric", McVersion: "1.20.1"}
	fs.srv = models.Server{ID: 1, UUID: "u-1", NodeID: 3, ActiveSubServer: "main", Status: "stopped",
		InstallerType: "fabric", MinecraftVersion: "1.20.1"}
	fs.mods = []models.ServerMod{{ID: 2, ServerID: 1, Title: "U", FileName: "u.jar", TargetDir: "mods"}}
	h := NewServerModsHandler(testDeps(Deps{Store: fs, Redis: rdb, Queue: services.NewQueueService(rdb)}))
	req := httptest.NewRequest(http.MethodPost, "/api/servers/1/version-update",
		strings.NewReader(`{"minecraft":"1.21.1","dropUnavailable":true,"javaImage":"`+platformImagePrefix+`21"}`))
	rec := httptest.NewRecorder()
	h.VersionUpdate(rec, mux.SetURLVars(req, map[string]string{"id": "1"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("answered %d %s", rec.Code, rec.Body.String())
	}
	// No project id: the undo cannot restore it, so it goes after the send.
	if len(fs.deleted) != 1 || fs.queuedAtDrop != 1 {
		t.Errorf("unlinked unavailable mod dropped %v with %d command(s) queued, want once after the dispatch", fs.deleted, fs.queuedAtDrop)
	}
	if fs.queuedAtInstall != 0 {
		t.Errorf("install record written with %d command(s) queued, want before the dispatch", fs.queuedAtInstall)
	}
	if fs.queued() != 1 {
		t.Errorf("%d command(s) queued, want 1", fs.queued())
	}
}

// Written before the dispatch, the rows are put back from the undo when the
// command never reached the queue: no node will report, so nothing else would.
func TestVersionUpdateSendFailurePutsTheRowsBack(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	dead := miniredis.RunT(t)
	queueRdb := redis.NewClient(&redis.Options{Addr: dead.Addr()})
	dead.Close()

	fs := &orderMoveStore{rdb: rdb}
	prev := models.SubServerInstall{ServerID: 1, SubServerName: "main", InstallerType: "fabric", McVersion: "1.20.1", BuildVersion: "0.16.9"}
	fs.install = &prev
	fs.srv = models.Server{ID: 1, UUID: "u-1", NodeID: 3, ActiveSubServer: "main", Status: "stopped",
		InstallerType: "fabric", MinecraftVersion: "1.20.1"}
	fs.mods = []models.ServerMod{{ID: 2, ServerID: 1, Title: "U", FileName: "u.jar", TargetDir: "mods"}}
	h := NewServerModsHandler(testDeps(Deps{Store: fs, Redis: rdb, Queue: services.NewQueueService(queueRdb)}))
	req := httptest.NewRequest(http.MethodPost, "/api/servers/1/version-update",
		strings.NewReader(`{"minecraft":"1.21.1","dropUnavailable":true,"javaImage":"`+platformImagePrefix+`21"}`))
	rec := httptest.NewRecorder()
	h.VersionUpdate(rec, mux.SetURLVars(req, map[string]string{"id": "1"}))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("answered %d %s, want 500", rec.Code, rec.Body.String())
	}
	last := fs.upserted[len(fs.upserted)-1]
	if last.McVersion != "1.20.1" || last.BuildVersion != "0.16.9" {
		t.Errorf("install record left as %+v, want the one before the move", last)
	}
	if len(fs.deleted) != 0 {
		t.Errorf("mod rows %v deleted for a move that never reached the node", fs.deleted)
	}
}
