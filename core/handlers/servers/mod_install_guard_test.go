package servers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

func TestModVersionFits(t *testing.T) {
	for _, tt := range []struct {
		name           string
		loader, mc     string
		vLoaders, vMCs []string
		want           bool
		reasonHas      string
	}{
		{"exact match", "neoforge", "1.21.1", []string{"neoforge"}, []string{"1.21", "1.21.1"}, true, ""},
		{"forge build on neoforge", "neoforge", "1.21.1", []string{"forge"}, []string{"1.21.1"}, false, "for forge, and this server runs neoforge"},
		{"neoforge build on forge", "forge", "1.21.1", []string{"neoforge"}, []string{"1.21.1"}, false, "runs forge"},
		{"line is not the exact version", "forge", "1.21.1", []string{"forge"}, []string{"1.21"}, false, "this server runs 1.21.1"},
		{"newer MC", "neoforge", "1.21.1", []string{"neoforge"}, []string{"26.3"}, false, "Minecraft 26.3"},
		{"quilt takes fabric", "quilt", "1.21.1", []string{"fabric"}, []string{"1.21.1"}, true, ""},
		{"fabric does not take quilt", "fabric", "1.21.1", []string{"quilt"}, []string{"1.21.1"}, false, "for quilt"},
		{"multi-loader build", "neoforge", "1.21.1", []string{"fabric", "neoforge"}, []string{"1.21.1"}, true, ""},
		{"case and space", " NeoForge ", " 1.21.1 ", []string{"NeoForge"}, []string{"1.21.1"}, true, ""},
		{"paper takes spigot", "paper", "1.21.4", []string{"spigot"}, []string{"1.21.4"}, true, ""},
		{"spigot does not take paper", "spigot", "1.21.4", []string{"paper"}, []string{"1.21.4"}, false, "for paper"},
		{"purpur takes paper", "purpur", "1.21.4", []string{"paper"}, []string{"1.21.4"}, true, ""},
		{"plugin takes an older patch of its line", "paper", "1.21.4", []string{"paper"}, []string{"1.21.3"}, true, ""},
		{"plugin takes the bare line", "spigot", "1.21.4", []string{"spigot"}, []string{"1.21"}, true, ""},
		{"plugin on the 26 scheme", "purpur", "26.1.2", []string{"paper"}, []string{"26.1"}, true, ""},
		{"plugin refuses another line", "paper", "1.21.4", []string{"paper"}, []string{"1.20.6"}, false, "Pick a build for 1.21.x"},
		{"plugin line is not a prefix", "paper", "1.2.5", []string{"bukkit"}, []string{"1.21"}, false, "Pick a build for 1.2.x"},
		{"plugin refuses the next 26 line", "paper", "26.1.2", []string{"paper"}, []string{"26.2"}, false, "Pick a build for 26.1.x"},
		{"mod stays exact on a patch", "fabric", "1.21.4", []string{"fabric"}, []string{"1.21.3"}, false, "Pick a build for 1.21.4"},
		{"proxy ignores MC", "velocity", "3.4.0", []string{"velocity"}, []string{"1.21.4"}, true, ""},
		{"proxy loader still checked", "velocity", "", []string{"paper"}, []string{"1.21.4"}, false, "for paper"},
		{"waterfall takes bungeecord", "waterfall", "", []string{"bungeecord"}, nil, true, ""},
		{"unknown loader", "upload", "1.21.1", []string{"forge"}, []string{"1.21.1"}, false, "loader is unknown"},
		{"unknown MC", "forge", "", []string{"forge"}, []string{"1.21.1"}, false, "Minecraft version is unknown"},
		{"no loaders listed", "forge", "1.21.1", nil, []string{"1.21.1"}, false, "for nothing"},
		{"long list keeps the newest", "forge", "1.21.1", []string{"forge"}, []string{"1.20", "1.20.1", "1.20.2", "1.20.4", "1.20.6"}, false, "1.20.2, 1.20.4, 1.20.6 (and 2 older)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ok, reason := handlers.ModVersionFits(tt.loader, tt.mc, tt.vLoaders, tt.vMCs)
			if ok != tt.want {
				t.Fatalf("fits = %v (%q), want %v", ok, reason, tt.want)
			}
			if ok && reason != "" {
				t.Errorf("a fit carries reason %q", reason)
			}
			if !strings.Contains(reason, tt.reasonHas) {
				t.Errorf("reason %q, want it to contain %q", reason, tt.reasonHas)
			}
		})
	}
}

func TestModFitKnown(t *testing.T) {
	for _, tt := range []struct {
		loader, mc string
		want       bool
	}{
		{"neoforge", "1.21.1", true},
		{"neoforge", "", false},
		{"upload", "1.21.1", false},
		{"", "", false},
		{"velocity", "", true},
		{"Paper", "1.21.4", true},
	} {
		if got := handlers.ModFitKnown(tt.loader, tt.mc); got != tt.want {
			t.Errorf("modFitKnown(%q, %q) = %v, want %v", tt.loader, tt.mc, got, tt.want)
		}
	}
}

type modFitFakeStore struct {
	installStatusFakeStore
	server *models.Server
}

func (f *modFitFakeStore) GetServerByID(int) (*models.Server, error) { return f.server, nil }

func fakeVersion(project string, loaders, mcs []string) *services.ModrinthVersion {
	return &services.ModrinthVersion{ID: "V1", ProjectID: project, Loaders: loaders, GameVersions: mcs,
		Files: []services.ModrinthFile{
			{Filename: "other.jar", URL: "https://cdn.modrinth.com/data/P1/versions/V1/other.jar"},
			{Filename: "real.jar", URL: "https://cdn.modrinth.com/data/P1/versions/V1/real.jar", Primary: true, Hashes: map[string]string{"sha512": "abc"}},
		}}
}

// Install refuses a build the server cannot load, and installs the file
// Modrinth names for that version rather than the one the request claims.
func TestInstallChecksTheBuildAgainstTheServer(t *testing.T) {
	neo := &models.Server{ID: 7, UUID: "srv-uuid", OwnerID: "alice", NodeID: 1, InstallerType: "neoforge", MinecraftVersion: "1.21.1", ActiveSubServer: "survival"}
	imported := &models.Server{ID: 7, UUID: "srv-uuid", OwnerID: "alice", NodeID: 1, InstallerType: "upload", ActiveSubServer: "survival"}
	notFound := &services.ModrinthHTTPError{Path: "/version/V1", Status: http.StatusNotFound}
	for _, tt := range []struct {
		name      string
		server    *models.Server
		version   *services.ModrinthVersion
		fetchErr  error
		wantCode  int
		wantFile  string
		wantFetch bool
	}{
		{"fits", neo, fakeVersion("P1", []string{"neoforge"}, []string{"1.21.1"}), nil, http.StatusOK, "real.jar", true},
		{"forge build", neo, fakeVersion("P1", []string{"forge"}, []string{"1.21.1"}), nil, http.StatusConflict, "", true},
		{"wrong MC", neo, fakeVersion("P1", []string{"neoforge"}, []string{"26.3"}), nil, http.StatusConflict, "", true},
		{"other project", neo, fakeVersion("P2", []string{"neoforge"}, []string{"1.21.1"}), nil, http.StatusBadRequest, "", true},
		{"unknown version", neo, nil, notFound, http.StatusBadRequest, "", true},
		{"modrinth down", neo, nil, errors.New("timeout"), http.StatusBadGateway, "", true},
		{"modrinth rate limits", neo, nil, &services.ModrinthHTTPError{Path: "/version/V1", Status: http.StatusTooManyRequests}, http.StatusServiceUnavailable, "", true},
		{"imported server unchecked", imported, nil, errors.New("must not be asked"), http.StatusOK, "spark.jar", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fetched := false
			old := fetchInstallVersion
			fetchInstallVersion = func(_ context.Context, id string) (*services.ModrinthVersion, error) {
				fetched = true
				return tt.version, tt.fetchErr
			}
			t.Cleanup(func() { fetchInstallVersion = old })

			m := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: m.Addr()})
			fs := &modFitFakeStore{installStatusFakeStore: installStatusFakeStore{mr: m}, server: tt.server}
			h := &ServerModsHandler{state: &handlers.AppState{Store: fs, Redis: rdb, Queue: services.NewQueueService(rdb)}}

			body := `{"projectId":"P1","versionId":"V1","fileName":"spark.jar","downloadUrl":"https://cdn.modrinth.com/data/P1/versions/V1/spark.jar","sha512":"client"}`
			req := httptest.NewRequest(http.MethodPost, "/api/servers/7/mods", strings.NewReader(body))
			req = req.WithContext(context.WithValue(req.Context(), "userID", "alice"))
			rw := httptest.NewRecorder()
			h.Install(rw, mux.SetURLVars(req, map[string]string{"id": "7"}))

			if rw.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body %s)", rw.Code, tt.wantCode, rw.Body.String())
			}
			if rw.Code == http.StatusServiceUnavailable && rw.Header().Get("Retry-After") == "" {
				t.Error("503 without Retry-After")
			}
			if fetched != tt.wantFetch {
				t.Errorf("Modrinth asked = %v, want %v", fetched, tt.wantFetch)
			}
			if tt.wantFile == "" {
				if fs.upserted != nil {
					t.Errorf("a refused install wrote row %+v", fs.upserted)
				}
				return
			}
			if fs.upserted == nil || fs.upserted.FileName != tt.wantFile {
				t.Fatalf("row = %+v, want file %q", fs.upserted, tt.wantFile)
			}
			if tt.wantFetch && fs.upserted.SHA512 != "abc" {
				t.Errorf("sha512 = %q, want Modrinth's", fs.upserted.SHA512)
			}
		})
	}
}

type wipeModsFakeStore struct {
	*installFakeStore
	mods    []models.ServerMod
	deleted []int
}

func (f *wipeModsFakeStore) ListServerMods(int, string) ([]models.ServerMod, error) {
	return f.mods, nil
}
func (f *wipeModsFakeStore) DeleteServerMod(id, _ int) error {
	f.deleted = append(f.deleted, id)
	return nil
}

// A setup that wipes mods/ forgets the mods recorded there, and only those: the
// Installed list used to keep naming jars the wipe had deleted.
func TestSetupWithModsWipeForgetsTheModRows(t *testing.T) {
	for _, tt := range []struct {
		name string
		wipe []string
		want []int
	}{
		{"mods wiped", []string{"libraries", "mods"}, []int{1, 3}},
		{"mods kept", []string{"libraries", "config"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, fs, _ := newInstallTest(t)
			fs.server.InstallerType = "fabric"
			ws := &wipeModsFakeStore{installFakeStore: fs, mods: []models.ServerMod{
				{ID: 1, TargetDir: "mods"},
				{ID: 2, TargetDir: "plugins"},
				{ID: 3}, // written before target_dir: placed by the fabric loader, in mods/
			}}
			h.state.Store = ws

			body := setupBody(map[string]string{"type": "neoforge", "loader": "21.1.264"})
			inst := body["installer"].(map[string]string)
			body["installer"] = map[string]any{"type": inst["type"], "loader": inst["loader"], "wipePaths": tt.wipe}
			rec := httptest.NewRecorder()
			h.SetupServer(rec, installRequest("setup", body, false))
			if rec.Code != http.StatusOK {
				t.Fatalf("setup status %d: %s", rec.Code, rec.Body.String())
			}
			if len(ws.deleted) != len(tt.want) {
				t.Fatalf("deleted %v, want %v", ws.deleted, tt.want)
			}
			for i := range tt.want {
				if ws.deleted[i] != tt.want[i] {
					t.Fatalf("deleted %v, want %v", ws.deleted, tt.want)
				}
			}
		})
	}
}

// The install record follows a reinstall; the edit form prefers it over the
// servers row, and it used to keep the version the reinstall replaced.
func TestReinstallRecordsTheSubServerInstall(t *testing.T) {
	h, fs, _ := newInstallTest(t)
	fs.server.Status = "stopped"
	fs.server.InstallerType = "forge"
	fs.server.MinecraftVersion = "1.20.1"
	fs.server.BuildNumber = "1.20.1"
	rec := httptest.NewRecorder()
	h.ReinstallServer(rec, installRequest("reinstall", map[string]any{
		"javaImage": "ghcr.io/dylaris-dev/platform-mc-java21:latest",
		"installer": map[string]string{"type": "neoforge", "loader": "21.1.264"},
	}, true))
	if rec.Code != http.StatusOK {
		t.Fatalf("reinstall status %d: %s", rec.Code, rec.Body.String())
	}
	want := models.SubServerInstall{ServerID: 1, SubServerName: "main", InstallerType: "neoforge", McVersion: "1.21.1", BuildVersion: "21.1.264", Loader: "21.1.264"}
	if fs.installRow != want {
		t.Errorf("install row = %+v, want %+v", fs.installRow, want)
	}
}
