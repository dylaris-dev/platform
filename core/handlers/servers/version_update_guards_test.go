package servers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
	pb "dylaris-proto/node"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

type versionGuardStore struct {
	store.Store
	srv  models.Server
	mods []models.ServerMod
}

// The real-IP switch as these tests set it by hand: mode "on", which refuses.
func (f *versionGuardStore) GetServerProxyMode(int) (string, error) { return store.ProxyModeOn, nil }

func (f *versionGuardStore) GetServerByID(int) (*models.Server, error) { s := f.srv; return &s, nil }
func (f *versionGuardStore) ListServerMods(int, string) ([]models.ServerMod, error) {
	return f.mods, nil
}

// A version update reinstalls the server, so it is refused where the result
// would lock players out (Real player IP on, a target the node cannot
// configure) or bring a switched-off mod back on.
func TestVersionUpdateRefusesWhatItWouldBreak(t *testing.T) {
	cases := []struct {
		name     string
		srv      models.Server
		mods     []models.ServerMod
		body     string
		wantCode string
	}{
		{"real IP on, moving to Fabric",
			models.Server{ID: 1, ActiveSubServer: "main", Status: "stopped", InstallerType: "paper", ProxyProtocol: true},
			nil, `{"minecraft":"1.21.4","loader":"fabric"}`, "proxy_protocol_on"},
		{"real IP on, moving to Paper 1.18",
			models.Server{ID: 1, ActiveSubServer: "main", Status: "stopped", InstallerType: "paper", ProxyProtocol: true},
			nil, `{"minecraft":"1.18.2","loader":"paper"}`, "proxy_protocol_on"},
		{"a switched-off mod",
			models.Server{ID: 1, ActiveSubServer: "main", Status: "stopped", InstallerType: "fabric"},
			[]models.ServerMod{{ID: 2, Title: "Lithium", FileName: "lithium.jar", Disabled: true}},
			`{"minecraft":"1.21.4","loader":"fabric"}`, "mods_switched_off"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := NewServerModsHandler(testDeps(Deps{Store: &versionGuardStore{srv: c.srv, mods: c.mods}}))
			req := httptest.NewRequest(http.MethodPost, "/api/servers/1/version-update", strings.NewReader(c.body))
			req = mux.SetURLVars(req, map[string]string{"id": "1"})
			rec := httptest.NewRecorder()
			h.VersionUpdate(rec, req)
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), c.wantCode) {
				t.Fatalf("answered %d %s, want 409 %s", rec.Code, rec.Body.String(), c.wantCode)
			}
		})
	}
}

// A move the node cannot carry out is refused before Core writes anything:
// a loader with no installer behind it, or a server still being installed.
func TestVersionUpdateRefusesWhatTheNodeCannotDo(t *testing.T) {
	cases := []struct {
		name       string
		srv        models.Server
		body       string
		wantStatus int
		wantCode   string
	}{
		{"purpur", models.Server{ID: 1, ActiveSubServer: "main", Status: "stopped", InstallerType: "purpur"},
			`{"minecraft":"1.21.4"}`, http.StatusUnprocessableEntity, "loader_not_installable"},
		{"quilt asked for", models.Server{ID: 1, ActiveSubServer: "main", Status: "stopped", InstallerType: "fabric"},
			`{"minecraft":"1.21.4","loader":"quilt"}`, http.StatusUnprocessableEntity, "loader_not_installable"},
		{"spigot", models.Server{ID: 1, ActiveSubServer: "main", Status: "stopped", InstallerType: "spigot"},
			`{"minecraft":"1.21.4"}`, http.StatusUnprocessableEntity, "loader_not_installable"},
		{"still installing", models.Server{ID: 1, ActiveSubServer: "main", Status: "installing", InstallerType: "paper"},
			`{"minecraft":"1.21.4"}`, http.StatusConflict, "server_installing"},
		{"being moved", models.Server{ID: 1, ActiveSubServer: "main", Status: "migrating", InstallerType: "paper"},
			`{"minecraft":"1.21.4"}`, http.StatusConflict, ""},
		// A proxy is versioned by its own release, not by Minecraft's: the node
		// was sent 1.21.4 as the Velocity version and failed after the stop.
		{"velocity", models.Server{ID: 1, ActiveSubServer: "main", Status: "stopped", InstallerType: "velocity", MinecraftVersion: "3.5"},
			`{"minecraft":"1.21.4"}`, http.StatusUnprocessableEntity, "proxy_not_movable"},
		{"bungeecord", models.Server{ID: 1, ActiveSubServer: "main", Status: "stopped", InstallerType: "bungeecord"},
			`{"minecraft":"1.21.4"}`, http.StatusUnprocessableEntity, "proxy_not_movable"},
		{"waterfall asked for", models.Server{ID: 1, ActiveSubServer: "main", Status: "stopped", InstallerType: "paper"},
			`{"minecraft":"1.21.4","loader":"waterfall"}`, http.StatusUnprocessableEntity, "proxy_not_movable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := NewServerModsHandler(testDeps(Deps{Store: &versionGuardStore{srv: c.srv}}))
			req := httptest.NewRequest(http.MethodPost, "/api/servers/1/version-update", strings.NewReader(c.body))
			req = mux.SetURLVars(req, map[string]string{"id": "1"})
			rec := httptest.NewRecorder()
			h.VersionUpdate(rec, req)
			if rec.Code != c.wantStatus || !strings.Contains(rec.Body.String(), c.wantCode) {
				t.Fatalf("answered %d %s, want %d %s", rec.Code, rec.Body.String(), c.wantStatus, c.wantCode)
			}
		})
	}
}

// moveStore is what a version move that gets as far as the dispatch reads and
// writes.
type moveStore struct {
	versionGuardStore
	install  *models.SubServerInstall
	upserted []models.SubServerInstall
}

func (f *moveStore) GetSetting(string) (string, error) { return "", nil }
func (f *moveStore) GetNodeByID(int) (*models.Node, error) {
	return &models.Node{ID: 3, Token: "tok"}, nil
}
func (f *moveStore) UpdateServerStatus(int, string) error { return nil }
func (f *moveStore) UpdateServerSetup(int, string, string, string, string, string, string, string) error {
	return nil
}
func (f *moveStore) GetSubServerInstall(int, string) (*models.SubServerInstall, error) {
	return f.install, nil
}
func (f *moveStore) UpsertSubServerInstall(in models.SubServerInstall) error {
	f.upserted = append(f.upserted, in)
	return nil
}

func runMove(t *testing.T, fs *moveStore, rdb *redis.Client, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewServerModsHandler(testDeps(Deps{Store: fs, Redis: rdb, Queue: services.NewQueueService(rdb)}))
	req := httptest.NewRequest(http.MethodPost, "/api/servers/1/version-update", strings.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"id": "1"})
	rec := httptest.NewRecorder()
	h.VersionUpdate(rec, req)
	return rec
}

// The install record follows a move, as it follows a reinstall: a switch back
// to this sub-server reads its Minecraft version from there, and it kept naming
// the version the move replaced. The undo record holds the old record and the
// old Java image, so an abandoned move puts both back.
func TestVersionUpdateMovesTheInstallRecordAndCanUndoIt(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	prev := &models.SubServerInstall{ServerID: 1, SubServerName: "main", InstallerType: "fabric", McVersion: "1.20.1",
		BuildVersion: "0.16.9", Loader: "0.16.9", ModrinthProjectID: "pack-1", PackID: 4, PackBuildID: 9}
	fs := &moveStore{install: prev}
	fs.srv = models.Server{ID: 1, UUID: "u-1", NodeID: 3, ActiveSubServer: "main", Status: "stopped",
		InstallerType: "fabric", MinecraftVersion: "1.20.1", BuildNumber: "0.16.9", GameImage: "img-java17"}

	rec := runMove(t, fs, rdb, `{"minecraft":"1.21.1","installerVersion":"0.16.10","javaImage":"`+platformImagePrefix+`21"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("answered %d %s", rec.Code, rec.Body.String())
	}
	if len(fs.upserted) != 1 {
		t.Fatalf("install record writes = %+v, want one", fs.upserted)
	}
	got := fs.upserted[0]
	if got.SubServerName != "main" || got.InstallerType != "fabric" || got.McVersion != "1.21.1" || got.BuildVersion != "0.16.10" || got.Loader != "0.16.10" {
		t.Errorf("install record = %+v, want fabric 1.21.1 build 0.16.10 on main", got)
	}
	if got.ModrinthProjectID != "pack-1" || got.PackID != 4 || got.PackBuildID != 9 {
		t.Errorf("install record lost its pack fields: %+v", got)
	}

	raw, err := rdb.Get(context.Background(), services.VersionUpdateUndoKey("u-1")).Result()
	if err != nil {
		t.Fatal(err)
	}
	var undo services.VersionUpdateUndo
	if err := json.Unmarshal([]byte(raw), &undo); err != nil {
		t.Fatal(err)
	}
	if undo.GameImage != "img-java17" {
		t.Errorf("undo game image = %q, want the pre-move img-java17", undo.GameImage)
	}
	if undo.Install == nil || undo.Install.McVersion != "1.20.1" || undo.Install.PackID != 4 {
		t.Errorf("undo install record = %+v, want the pre-move one", undo.Install)
	}
}

// "Copy first, then move": the move waits for the copy, never runs beside it,
// and never starts when the copy it was meant to follow did not land.
func TestCopyBeforeMoveVerdict(t *testing.T) {
	listing := func(names ...string) []*pb.FileInfo {
		out := make([]*pb.FileInfo, 0, len(names))
		for _, n := range names {
			out = append(out, &pb.FileInfo{Name: n, IsDir: true})
		}
		return out
	}
	for _, c := range []struct {
		name, pending, afterCopy string
		entries                  []*pb.FileInfo
		want                     string
	}{
		{"queued, not started", "main-old", "main-old", listing("main"), "copy_running"},
		{"queued, asked without afterCopy", "main-old", "", listing("main"), "copy_running"},
		{"landed", "main-old", "main-old", listing("main", "main-old"), ""},
		{"landed, marker already gone", "", "main-old", listing("main", "MAIN-OLD"), ""},
		{"failed", "", "main-old", listing("main"), "copy_failed"},
		{"another copy still queued", "other", "main-old", listing("main", "main-old"), "copy_running"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := copyBeforeMoveVerdict(c.pending, c.afterCopy, c.entries); got != c.want {
				t.Errorf("verdict = %q, want %q", got, c.want)
			}
		})
	}
}

// While the node is still copying, the move is answered "wait", not "busy",
// so the panel holds it back instead of reporting a failure.
func TestVersionUpdateWaitsForARunningCopy(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	fs := &moveStore{}
	fs.srv = models.Server{ID: 1, UUID: "u-1", NodeID: 3, ActiveSubServer: "main", Status: "stopped", InstallerType: "fabric"}
	mr.Set(services.CopyPendingKey("u-1"), "main-old")
	mr.Set(services.NodeBusyKey("u-1"), "installing")

	rec := runMove(t, fs, rdb, `{"minecraft":"1.21.1","afterCopy":"main-old"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "copy_running") {
		t.Fatalf("answered %d %s, want 409 copy_running", rec.Code, rec.Body.String())
	}
	if n, _ := rdb.Exists(context.Background(), services.VersionUpdateUndoKey("u-1")).Result(); n != 0 {
		t.Error("the move was prepared while the copy ran")
	}
}
