package servers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers"
	"dylaris-core/handlers/handlertest"
	"dylaris-core/services/corestorage"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-core/services"
)

// installFakeStore is the power-handler fake plus what setup and reinstall write.
type installFakeStore struct {
	serverPowerFakeStore
	setupWrites int
	setupType   string
	setupMC     string
	setupBuild  string
	installRow  models.SubServerInstall
	// What GetSubServerInstall answers; nil is "no record".
	storedInstall *models.SubServerInstall
}

func (f *installFakeStore) UpdateServerSetup(_ int, _, _, _, _, installerType, mcVersion, build string) error {
	f.setupWrites++
	f.setupType = installerType
	f.setupMC = mcVersion
	f.setupBuild = build
	return nil
}
func (f *installFakeStore) GetSubServerInstall(int, string) (*models.SubServerInstall, error) {
	return f.storedInstall, nil
}
func (f *installFakeStore) UpsertSubServerInstall(r models.SubServerInstall) error {
	f.installRow = r
	return nil
}
func (f *installFakeStore) ReplaceServerModpackContents(int, string, []models.ServerModpackContent) error {
	return nil
}
func (f *installFakeStore) GetModpackScan(int, string) (string, int, int, error) {
	return "", 0, 0, nil
}
func (f *installFakeStore) SetModpackScan(int, string, string, int) error { return nil }

// newInstallTest seeds a path-backed Core storage and a server on node token
// "node-lib".
func newInstallTest(t *testing.T) (*ServerHandler, *installFakeStore, *redis.Client) {
	t.Helper()
	dir := handlertest.ConnectionProbeDir(t)
	fs := &installFakeStore{serverPowerFakeStore: serverPowerFakeStore{
		server: &models.Server{ID: 1, UUID: "srv-uuid", Status: "pending_setup", OwnerID: "u1", OwnerName: "alice",
			NodeID: 7, ActiveSubServer: "main", InstallerType: "paper", BuildNumber: "1.21.1", MinecraftVersion: "1.21.1"},
		node: &models.Node{ID: 7, Token: "node-lib", Name: "n1", Status: "online"},
		settings: map[string]string{
			corestorage.KeyBackend:     "path",
			corestorage.KeyPath:        dir,
			corestorage.KeyPathConfirm: "true",
			"core_public_url":          "https://panel.example.com",
		},
	}}
	rdb := handlertest.RedisClient(t)
	h := &ServerHandler{state: &handlers.AppState{
		Store:         fs,
		Redis:         rdb,
		Queue:         services.NewQueueService(rdb),
		FeatureFlags:  services.NewFeatureFlags(fs),
		ClusterSecret: "test-cluster-secret",
		CoreStorage:   &corestorage.Manager{Store: fs, Redis: rdb, ClusterSecret: "test-cluster-secret"},
	}}
	return h, fs, rdb
}

func installRequest(route string, body any, isAdmin bool) *http.Request {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "/api/servers/1/"+route, bytes.NewReader(b))
	r = mux.SetURLVars(r, map[string]string{"id": "1"})
	ctx := context.WithValue(r.Context(), "username", "alice")
	ctx = context.WithValue(ctx, "isAdmin", isAdmin)
	ctx = context.WithValue(ctx, "userID", "u1")
	return r.WithContext(ctx)
}

// queuedInstaller returns the installer of every command queued for the node.
func queuedInstaller(t *testing.T, rdb *redis.Client) []map[string]any {
	t.Helper()
	msgs, err := rdb.XRange(context.Background(), "dylaris:node:node-lib:cmds", "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, m := range msgs {
		for _, v := range m.Values {
			var cmd struct {
				Installer map[string]any `json:"installer"`
			}
			if s, ok := v.(string); ok && json.Unmarshal([]byte(s), &cmd) == nil && cmd.Installer != nil {
				out = append(out, cmd.Installer)
			}
		}
	}
	return out
}

func setupBody(installer map[string]string) map[string]any {
	return map[string]any{"subServerName": "main", "javaImage": "ghcr.io/dylaris-dev/platform-mc-java21:latest", "installer": installer}
}

// Reinstall used to hand its raw fields to the node: any installer type with
// no validation, and an omitted type or version sent empty while only the
// database got the fallback - and the node deletes the jars before it looks.
func TestReinstallSendsWhatItValidated(t *testing.T) {
	h, fs, rdb := newInstallTest(t)
	fs.server.Status = "stopped"
	for _, typ := range []string{"library", "modpack", "technic", "pack", "import", "upload"} {
		rec := httptest.NewRecorder()
		h.ReinstallServer(rec, installRequest("reinstall", map[string]any{
			"javaImage": "ghcr.io/dylaris-dev/platform-mc-java21:latest",
			"installer": map[string]string{"type": typ, "path": "/proc/self/environ", "url": "http://169.254.169.254/"},
		}, true))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("reinstall type %q: status %d, want 400", typ, rec.Code)
		}
	}
	if n := len(queuedInstaller(t, rdb)); n != 0 {
		t.Fatalf("%d install(s) queued for refused types", n)
	}

	rec := httptest.NewRecorder()
	h.ReinstallServer(rec, installRequest("reinstall", map[string]any{"javaImage": "ghcr.io/dylaris-dev/platform-mc-java21:latest"}, true))
	if rec.Code != http.StatusOK {
		t.Fatalf("reinstall with fallbacks: status %d: %s", rec.Code, rec.Body.String())
	}
	inst := queuedInstaller(t, rdb)
	if len(inst) != 1 || inst[0]["type"] != "paper" || inst[0]["version"] != "1.21.1" {
		t.Fatalf("queued installer = %v, want type paper version 1.21.1 from the server", inst)
	}
}
