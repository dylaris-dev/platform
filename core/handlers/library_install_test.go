package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-core/services"
)

// installFakeStore is the power-handler fake plus what setup and reinstall write.
type installFakeStore struct {
	serverPowerFakeStore
	disabled    []string
	setupWrites int
}

func (f *installFakeStore) UpdateServerSetup(int, string, string, string, string, string, string, string) error {
	f.setupWrites++
	return nil
}
func (f *installFakeStore) UpsertSubServerInstall(models.SubServerInstall) error { return nil }
func (f *installFakeStore) ReplaceServerModpackContents(int, string, []models.ServerModpackContent) error {
	return nil
}
func (f *installFakeStore) ListDisabledLibraryPaths() ([]string, error) { return f.disabled, nil }

const testCoreURL = "https://panel.example.com"

// newInstallTest seeds a path-backed library holding mods/paper.jar and
// secret/hidden.jar, and a server on node token "node-lib".
func newInstallTest(t *testing.T) (*ServerHandler, *installFakeStore, *redis.Client) {
	t.Helper()
	dir := testConnectionProbeDir(t)
	for _, f := range []string{"mods/paper.jar", "secret/hidden.jar"} {
		p := filepath.Join(dir, CoreStoragePrefixLibrary, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("payload:"+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fs := &installFakeStore{serverPowerFakeStore: serverPowerFakeStore{
		server: &models.Server{ID: 1, UUID: "srv-uuid", Status: "pending_setup", OwnerID: "u1", OwnerName: "alice",
			NodeID: 7, ActiveSubServer: "main", InstallerType: "paper", BuildNumber: "1.21.1", MinecraftVersion: "1.21.1"},
		node: &models.Node{ID: 7, Token: "node-lib", Name: "n1", Status: "online"},
		settings: map[string]string{
			keyCoreStorageBackend:     "path",
			keyCoreStoragePath:        dir,
			keyCoreStoragePathConfirm: "true",
			"core_public_url":         testCoreURL,
			"feature_library_enabled": "true",
		},
	}}
	rdb := newServerPowerRedis(t)
	h := &ServerHandler{state: &AppState{
		Store:         fs,
		Redis:         rdb,
		Queue:         services.NewQueueService(rdb),
		FeatureFlags:  services.NewFeatureFlags(fs),
		ClusterSecret: "test-cluster-secret",
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

// The node used to be sent the library path and copied whatever that path
// named on its own disk. It now gets a signed URL on Core, which serves that
// one file and nothing else.
func TestLibrarySetupSendsASignedCoreURLNotAPath(t *testing.T) {
	h, _, rdb := newInstallTest(t)
	rec := httptest.NewRecorder()
	h.SetupServer(rec, installRequest("setup", setupBody(map[string]string{"type": "library", "path": "/mods/paper.jar"}), false))
	if rec.Code != http.StatusOK {
		t.Fatalf("setup status %d: %s", rec.Code, rec.Body.String())
	}
	inst := queuedInstaller(t, rdb)
	if len(inst) != 1 {
		t.Fatalf("queued %d installs, want 1", len(inst))
	}
	if p, _ := inst[0]["path"].(string); p != "" {
		t.Fatalf("the node was sent a path %q", p)
	}
	u, _ := inst[0]["url"].(string)
	if !strings.HasPrefix(u, testCoreURL+"/mirror/library/") || !strings.HasSuffix(u, "/mods/paper.jar") {
		t.Fatalf("url = %q, want a Core library mirror URL ending in the file", u)
	}

	mirror := mux.NewRouter()
	mirror.HandleFunc("/mirror/library/{exp}/{sig}/{rest:.*}", (&LibraryHandler{state: h.state}).LibraryMirror)
	get := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mirror.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		return rec
	}
	parsed, _ := url.Parse(u)
	if rec := get(parsed.Path); rec.Code != http.StatusOK || rec.Body.String() != "payload:mods/paper.jar" {
		t.Fatalf("mirror served %d %q", rec.Code, rec.Body.String())
	}

	// The signature names one path and one expiry: another file, a forged
	// signature or an expired URL all get the same 404.
	other := strings.Replace(parsed.Path, "/mods/paper.jar", "/secret/hidden.jar", 1)
	parts := strings.SplitN(strings.TrimPrefix(parsed.Path, "/mirror/library/"), "/", 3)
	forged := "/mirror/library/" + parts[0] + "/" + strings.Repeat("0", len(parts[1])) + "/" + parts[2]
	expired := "/mirror/" + libraryMirrorURL("", h.state.ClusterSecret, "mods/paper.jar", time.Now().Add(-libraryMirrorTTL-time.Minute))
	for name, target := range map[string]string{"other file": other, "forged signature": forged, "expired": expired} {
		if rec := get(target); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", name, rec.Code)
		}
	}
}

func TestLibrarySetupRefusesBeforeQueueing(t *testing.T) {
	cases := []struct {
		name string
		path string
		want int
	}{
		{"traversal", "../../etc/passwd", http.StatusBadRequest},
		{"a node path", "/proc/self/environ", http.StatusNotFound},
		{"disabled for a non-admin", "secret/hidden.jar", http.StatusForbidden},
		// The storage backends clean these to the disabled file while the
		// denylist compared the uncleaned string.
		{"disabled, spelled with a dot segment", "./secret/hidden.jar", http.StatusBadRequest},
		{"disabled, spelled with a double slash", "secret//hidden.jar", http.StatusBadRequest},
		// Opening a directory succeeds on the local backend; the node would
		// install an empty server.jar.
		{"a directory", "mods", http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, fs, rdb := newInstallTest(t)
			fs.disabled = []string{"secret"}
			rec := httptest.NewRecorder()
			h.SetupServer(rec, installRequest("setup", setupBody(map[string]string{"type": "library", "path": c.path}), false))
			if rec.Code != c.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.want, rec.Body.String())
			}
			if n := len(queuedInstaller(t, rdb)); n != 0 {
				t.Fatalf("%d install(s) queued after a refusal", n)
			}
		})
	}
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
