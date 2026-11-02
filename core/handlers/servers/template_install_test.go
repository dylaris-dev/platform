package servers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
)

// templateFakeStore is the install fake plus one template and the caller's
// share level on it.
type templateFakeStore struct {
	installFakeStore
	tmpl    *models.Template
	share   string
	refs    []models.TemplateModrinthRef
	modRows []models.ServerMod
}

func (f *templateFakeStore) ListTemplateModrinth(int) ([]models.TemplateModrinthRef, error) {
	return f.refs, nil
}
func (f *templateFakeStore) UpsertServerMod(m *models.ServerMod) (int, error) {
	f.modRows = append(f.modRows, *m)
	return len(f.modRows), nil
}

func (f *templateFakeStore) GetTemplate(id int) (*models.Template, error) {
	if f.tmpl == nil || f.tmpl.ID != id {
		return nil, nil
	}
	c := *f.tmpl
	return &c, nil
}
func (f *templateFakeStore) TemplateShareLevel(int, string) (string, error)         { return f.share, nil }
func (f *templateFakeStore) ListServerMods(int, string) ([]models.ServerMod, error) { return nil, nil }

type pushedTemplate struct {
	nodeID    int
	server    string
	subServer string
	template  int
}

// newTemplateInstallTest is newInstallTest with a template store and the node
// write replaced by a recorder.
func newTemplateInstallTest(t *testing.T, tmpl *models.Template, audience string) (*ServerHandler, *templateFakeStore, *[]pushedTemplate) {
	t.Helper()
	h, base, _ := newInstallTest(t)
	fs := &templateFakeStore{installFakeStore: *base, tmpl: tmpl}
	fs.settings["feature_templates"] = audience
	h.d.Store = fs
	h.d.CoreStorage.Store = h.d.Store
	h.d.FeatureFlags = services.NewFeatureFlags(fs)
	var pushes []pushedTemplate
	prev := templatePush
	templatePush = func(_ context.Context, _ Deps, nodeID int, serverUUID, subServer string, t *models.Template) error {
		pushes = append(pushes, pushedTemplate{nodeID, serverUUID, subServer, t.ID})
		return nil
	}
	t.Cleanup(func() { templatePush = prev })
	return h, fs, &pushes
}

func templateSetup(id int) map[string]any {
	return map[string]any{"subServerName": "main", "javaImage": "ghcr.io/dylaris-dev/platform-mc-java21:latest",
		"installer": map[string]any{"type": "template", "templateId": id}}
}

// Someone else's personal template, not shared with the caller: refused as if
// it did not exist, and nothing reaches the node.
func TestTemplateInstallRefusedWhenTheCallerCannotSeeIt(t *testing.T) {
	cases := []struct {
		name     string
		owner    string
		audience string
	}{
		{"foreign personal template", "u2", "all"},
		{"own template while the feature is admin-only", "u1", "admin"},
		{"own template while the feature is off", "u1", "off"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, fs, pushes := newTemplateInstallTest(t, &models.Template{ID: 5, OwnerID: c.owner, Kind: "server", Visibility: "personal", Software: "paper", McVersion: "1.21.4", SoftwareVersion: "1.21.4"}, c.audience)
			rec := httptest.NewRecorder()
			h.SetupServer(rec, installRequest("setup", templateSetup(5), false))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status %d, want 404: %s", rec.Code, rec.Body.String())
			}
			if len(*pushes) != 0 || fs.setupWrites != 0 {
				t.Fatalf("pushed %d, wrote %d after a refusal", len(*pushes), fs.setupWrites)
			}
			if n := len(queuedInstaller(t, h.d.Redis)); n != 0 {
				t.Fatalf("queued %d", n)
			}
		})
	}
}

func TestTemplateInstallRefusesFieldsThatDoNotApply(t *testing.T) {
	h, _, pushes := newTemplateInstallTest(t, &models.Template{ID: 5, OwnerID: "u1", Kind: "server", Visibility: "personal", Software: "paper", SoftwareVersion: "1.21.4"}, "all")
	for _, extra := range []map[string]any{
		{"templateId": 0},
		{"templateId": 5, "url": "http://169.254.169.254/"},
		{"templateId": 5, "software": "fabric"},
		{"templateId": 5, "version": "1.20.1"},
		{"templateId": 5, "structure": "subfolder"},
		{"templateId": 5, "packId": 3},
	} {
		inst := map[string]any{"type": "template"}
		for k, v := range extra {
			inst[k] = v
		}
		body := templateSetup(5)
		body["installer"] = inst
		h.d.Redis.Del(context.Background(), "dylaris:server:srv-uuid:install-start")
		rec := httptest.NewRecorder()
		h.SetupServer(rec, installRequest("setup", body, false))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%v: status %d, want 400: %s", extra, rec.Code, rec.Body.String())
		}
	}
	if len(*pushes) != 0 {
		t.Fatalf("pushed %d", len(*pushes))
	}
}

// A server template becomes the upload-zip install the node already knows,
// with the template's software laid over its files.
func TestTemplateInstallRewritesToUploadZip(t *testing.T) {
	h, fs, pushes := newTemplateInstallTest(t, &models.Template{ID: 5, OwnerID: "u1", Kind: "server", Visibility: "personal", Software: "fabric", McVersion: "1.21.4", SoftwareVersion: "0.16.10"}, "all")
	body := templateSetup(5)
	body["installer"].(map[string]any)["wipePaths"] = []string{"mods"}
	rec := httptest.NewRecorder()
	h.SetupServer(rec, installRequest("setup", body, false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(*pushes) != 1 || (*pushes)[0] != (pushedTemplate{7, "srv-uuid", "main", 5}) {
		t.Fatalf("pushes = %+v", *pushes)
	}
	inst := queuedInstaller(t, h.d.Redis)
	if len(inst) != 1 {
		t.Fatalf("queued %v", inst)
	}
	got := inst[0]
	if got["type"] != "upload-zip" || got["structure"] != "direct" || got["software"] != "fabric" ||
		got["version"] != "1.21.4" || got["loader"] != "0.16.10" {
		t.Fatalf("queued installer %v", got)
	}
	if w, _ := got["wipePaths"].([]any); len(w) != 1 || w[0] != "mods" {
		t.Fatalf("wipePaths = %v", got["wipePaths"])
	}
	if fs.setupType != "fabric" || fs.installRow.InstallerType != "fabric" || fs.setupMC != "1.21.4" {
		t.Fatalf("recorded %q %q / %+v", fs.setupType, fs.setupMC, fs.installRow)
	}
}

// A failed push answers an error and leaves the server as it was.
func TestTemplateInstallPushFailureQueuesNothing(t *testing.T) {
	h, fs, _ := newTemplateInstallTest(t, &models.Template{ID: 5, OwnerID: "u1", Kind: "server", Visibility: "personal", Software: "paper", SoftwareVersion: "1.21.4"}, "all")
	templatePush = func(context.Context, Deps, int, string, string, *models.Template) error {
		return context.DeadlineExceeded
	}
	rec := httptest.NewRecorder()
	h.SetupServer(rec, installRequest("setup", templateSetup(5), false))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %s", rec.Code, rec.Body.String())
	}
	if n := len(queuedInstaller(t, h.d.Redis)); n != 0 || fs.setupWrites != 0 {
		t.Fatalf("queued %d, wrote %d after a failed push", n, fs.setupWrites)
	}
}

// A proxy template needs the node that installs a proxy over an upload; an
// older one is refused before any byte is pushed.
func TestProxyTemplateRefusedOnAnOlderNode(t *testing.T) {
	h, fs, pushes := newTemplateInstallTest(t, &models.Template{ID: 5, OwnerID: "u1", Kind: "proxy", Visibility: "personal", Software: "velocity", SoftwareVersion: "3.4.0"}, "all")
	fs.server.ServerType = "proxy"
	h.d.Redis.Set(context.Background(), "dylaris:discovery:node-lib", `{"releaseVersion":"2026.10.31"}`, 0)
	rec := httptest.NewRecorder()
	h.SetupServer(rec, installRequest("setup", templateSetup(5), false))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if msg := decodeErrBody(t, rec); msg != "This node runs an older release that cannot install a proxy template. Update the node, then try again." {
		t.Fatalf("message %q", msg)
	}
	if len(*pushes) != 0 {
		t.Fatalf("pushed %d", len(*pushes))
	}

	// The same template on a game server goes through the proxy gate every
	// online proxy install takes.
	fs.server.ServerType = "game"
	h.d.Redis.Set(context.Background(), "dylaris:discovery:node-lib", `{"releaseVersion":"`+templateProxySince+`"}`, 0)
	h.d.Redis.Del(context.Background(), "dylaris:server:srv-uuid:install-start")
	rec = httptest.NewRecorder()
	h.SetupServer(rec, installRequest("setup", templateSetup(5), false))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("game server: status %d, want 400: %s", rec.Code, rec.Body.String())
	}

	fs.server.ServerType = "proxy"
	h.d.Redis.Del(context.Background(), "dylaris:server:srv-uuid:install-start")
	rec = httptest.NewRecorder()
	h.SetupServer(rec, installRequest("setup", templateSetup(5), false))
	if rec.Code != http.StatusOK {
		t.Fatalf("current node: status %d: %s", rec.Code, rec.Body.String())
	}
	if inst := queuedInstaller(t, h.d.Redis); len(inst) != 1 || inst[0]["software"] != "velocity" || inst[0]["version"] != "3.4.0" {
		t.Fatalf("queued %v", inst)
	}
}

// A push already running into the server refuses a second one, for admins too
// (they skip the install cooldown); the lock is held only for the push.
func TestTemplatePushLockRefusesASecondPush(t *testing.T) {
	const lockKey = "dylaris:server:srv-uuid:template-push"
	for _, admin := range []bool{false, true} {
		h, _, pushes := newTemplateInstallTest(t, &models.Template{ID: 5, OwnerID: "u1", Kind: "server", Visibility: "personal", Software: "paper", McVersion: "1.21.4", SoftwareVersion: "1.21.4"}, "all")
		ctx := context.Background()
		h.d.Redis.Set(ctx, lockKey, "someone-else", 0)
		rec := httptest.NewRecorder()
		h.SetupServer(rec, installRequest("setup", templateSetup(5), admin))
		if rec.Code != http.StatusConflict || decodeErrBody(t, rec) != templatePushBusy {
			t.Fatalf("admin=%v: status %d, want 409 busy: %s", admin, rec.Code, rec.Body.String())
		}
		if len(*pushes) != 0 {
			t.Fatalf("admin=%v: pushed %d while locked", admin, len(*pushes))
		}
		if v, _ := h.d.Redis.Get(ctx, lockKey).Result(); v != "someone-else" {
			t.Fatalf("admin=%v: foreign lock became %q", admin, v)
		}

		h.d.Redis.Del(ctx, lockKey, "dylaris:server:srv-uuid:install-start")
		var heldDuringPush bool
		templatePush = func(context.Context, Deps, int, string, string, *models.Template) error {
			heldDuringPush = h.d.Redis.Exists(ctx, lockKey).Val() == 1
			return nil
		}
		rec = httptest.NewRecorder()
		h.SetupServer(rec, installRequest("setup", templateSetup(5), admin))
		if rec.Code != http.StatusOK {
			t.Fatalf("admin=%v: status %d: %s", admin, rec.Code, rec.Body.String())
		}
		if !heldDuringPush {
			t.Errorf("admin=%v: lock not held during the push", admin)
		}
		if h.d.Redis.Exists(ctx, lockKey).Val() != 0 {
			t.Errorf("admin=%v: lock left behind after the push", admin)
		}
	}
}

func templateRef(path, project string) models.TemplateModrinthRef {
	return models.TemplateModrinthRef{Path: path, ProjectID: project, VersionID: project + "-v", FileName: "display " + project,
		URL: "https://cdn.modrinth.com/data/" + project + "/x.jar", Sha512: strings.Repeat("a", 128), SizeBytes: 42}
}

// A template's Modrinth references travel as modrinthFiles, and each jar laid
// directly in mods/ or plugins/ becomes a server_mods row.
func TestTemplateInstallSendsModrinthFiles(t *testing.T) {
	h, fs, _ := newTemplateInstallTest(t, &models.Template{ID: 5, OwnerID: "u1", Kind: "server", Visibility: "personal", Software: "paper", SoftwareVersion: "1.21.4"}, "all")
	fs.refs = []models.TemplateModrinthRef{templateRef("plugins/LuckPerms.jar", "lp"), templateRef("plugins/Geyser/ext/x.jar", "gx")}
	h.d.Redis.Set(context.Background(), "dylaris:discovery:node-lib", `{"releaseVersion":"`+templateModrinthSince+`"}`, 0)
	rec := httptest.NewRecorder()
	h.SetupServer(rec, installRequest("setup", templateSetup(5), false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	inst := queuedInstaller(t, h.d.Redis)
	if len(inst) != 1 {
		t.Fatalf("queued %v", inst)
	}
	files, _ := inst[0]["modrinthFiles"].([]any)
	if len(files) != 2 {
		t.Fatalf("modrinthFiles = %v", inst[0]["modrinthFiles"])
	}
	f0 := files[0].(map[string]any)
	if f0["path"] != "plugins/LuckPerms.jar" || f0["url"] != "https://cdn.modrinth.com/data/lp/x.jar" ||
		f0["sha512"] != strings.Repeat("a", 128) || f0["size"] != float64(42) {
		t.Fatalf("file = %v", f0)
	}
	if len(fs.modRows) != 1 {
		t.Fatalf("rows = %+v", fs.modRows)
	}
	r := fs.modRows[0]
	if r.ServerID != 1 || r.SubServerName != "main" || r.ModrinthProjectID != "lp" || r.ModrinthVersionID != "lp-v" ||
		r.FileName != "LuckPerms.jar" || r.TargetDir != "plugins" || r.SHA512 != strings.Repeat("a", 128) ||
		r.Status != models.ServerModInstalled || r.InstalledBy == nil || *r.InstalledBy != "u1" {
		t.Fatalf("row = %+v", r)
	}
}

// A template with references is refused on a node known to be older, and on a
// node that reports no version unless Core itself is an unstamped dev build
// (the local testbed, where the node is unstamped too). An old node would
// otherwise install the template without the jars and report success.
func TestTemplateWithModrinthRefusedOnAnOlderNode(t *testing.T) {
	cases := []struct {
		name      string
		refs      bool
		core      string // this Core's stamp; "" = dev build
		heartbeat string // "" = no heartbeat (offline node)
		want      int
	}{
		{"refs on an older node", true, "2026.11.02", `{"releaseVersion":"2026.11.01.3"}`, http.StatusConflict},
		{"refs on a node at the release", true, "2026.11.02", `{"releaseVersion":"` + templateModrinthSince + `"}`, http.StatusOK},
		{"refs on an unstamped node, stamped core", true, "2026.11.02", `{}`, http.StatusConflict},
		{"refs on an unparseable node version, stamped core", true, "2026.11.02", `{"releaseVersion":"dev"}`, http.StatusConflict},
		{"refs on an unstamped node, dev core", true, "", `{}`, http.StatusOK},
		{"refs on an older node, dev core", true, "", `{"releaseVersion":"2026.11.01.3"}`, http.StatusConflict},
		{"refs, no heartbeat", true, "2026.11.02", "", http.StatusOK},
		{"no refs on an unstamped node, stamped core", false, "2026.11.02", `{}`, http.StatusOK},
		{"no refs on an older node", false, "2026.11.02", `{"releaseVersion":"2026.11.01.3"}`, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, fs, pushes := newTemplateInstallTest(t, &models.Template{ID: 5, OwnerID: "u1", Kind: "server", Visibility: "personal", Software: "paper", SoftwareVersion: "1.21.4"}, "all")
			h.d.ReleaseVersion = c.core
			if c.refs {
				fs.refs = []models.TemplateModrinthRef{templateRef("plugins/LuckPerms.jar", "lp")}
			}
			if c.heartbeat != "" {
				h.d.Redis.Set(context.Background(), "dylaris:discovery:node-lib", c.heartbeat, 0)
			}
			rec := httptest.NewRecorder()
			h.SetupServer(rec, installRequest("setup", templateSetup(5), false))
			if rec.Code != c.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.want, rec.Body.String())
			}
			if c.want == http.StatusConflict {
				if msg := decodeErrBody(t, rec); msg != templateModrinthRefused {
					t.Fatalf("message %q", msg)
				}
				if len(*pushes) != 0 || fs.setupWrites != 0 || len(fs.modRows) != 0 {
					t.Fatalf("pushed %d, wrote %d, rows %d after a refusal", len(*pushes), fs.setupWrites, len(fs.modRows))
				}
				return
			}
			if _, has := queuedInstaller(t, h.d.Redis)[0]["modrinthFiles"]; has != c.refs {
				t.Fatalf("modrinthFiles present = %v", has)
			}
		})
	}
}

// A reference the node would refuse is refused before anything is pushed.
func TestTemplateWithAnUninstallableReferenceIsRefused(t *testing.T) {
	for _, ref := range []models.TemplateModrinthRef{
		templateRef("config/x.jar", "cx"),
		func() models.TemplateModrinthRef {
			r := templateRef("mods/a.jar", "a")
			r.URL = "https://evil.example/a.jar"
			return r
		}(),
		func() models.TemplateModrinthRef { r := templateRef("mods/a.jar", "a"); r.Sha512 = ""; return r }(),
	} {
		h, fs, pushes := newTemplateInstallTest(t, &models.Template{ID: 5, OwnerID: "u1", Kind: "server", Visibility: "personal", Software: "fabric", SoftwareVersion: "1.21.4"}, "all")
		fs.refs = []models.TemplateModrinthRef{ref}
		rec := httptest.NewRecorder()
		h.SetupServer(rec, installRequest("setup", templateSetup(5), false))
		if rec.Code != http.StatusBadRequest || !strings.Contains(decodeErrBody(t, rec), ref.Path) {
			t.Errorf("%+v: status %d: %s", ref, rec.Code, rec.Body.String())
		}
		if len(*pushes) != 0 {
			t.Errorf("pushed %d", len(*pushes))
		}
	}
}

// More references than one template may hold is refused before anything is
// pushed (only reachable for rows stored before the cap applied everywhere).
func TestTemplateOverTheReferenceCapIsRefused(t *testing.T) {
	h, fs, pushes := newTemplateInstallTest(t, &models.Template{ID: 5, OwnerID: "u1", Kind: "server", Visibility: "personal", Software: "paper", SoftwareVersion: "1.21.4"}, "all")
	for i := 0; i <= services.MaxTemplateModrinthRefs; i++ {
		fs.refs = append(fs.refs, templateRef(fmt.Sprintf("plugins/p%d.jar", i), fmt.Sprint("p", i)))
	}
	rec := httptest.NewRecorder()
	h.SetupServer(rec, installRequest("setup", templateSetup(5), false))
	if rec.Code != http.StatusBadRequest || !strings.Contains(decodeErrBody(t, rec), "more than the 500") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(*pushes) != 0 || fs.setupWrites != 0 || len(fs.modRows) != 0 {
		t.Fatalf("pushed %d, wrote %d, rows %d", len(*pushes), fs.setupWrites, len(fs.modRows))
	}

	// Exactly at the cap installs.
	fs.refs = fs.refs[:services.MaxTemplateModrinthRefs]
	h.d.Redis.Del(context.Background(), "dylaris:server:srv-uuid:install-start")
	rec = httptest.NewRecorder()
	h.SetupServer(rec, installRequest("setup", templateSetup(5), false))
	if rec.Code != http.StatusOK {
		t.Fatalf("at the cap: status %d: %s", rec.Code, rec.Body.String())
	}
}
