package servers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"
)

// templateFakeStore is the install fake plus one template and the caller's
// share level on it.
type templateFakeStore struct {
	installFakeStore
	tmpl  *models.Template
	share string
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
	h.state.Store = fs
	h.state.FeatureFlags = services.NewFeatureFlags(fs)
	var pushes []pushedTemplate
	prev := templatePush
	templatePush = func(_ context.Context, _ *handlers.AppState, nodeID int, serverUUID, subServer string, t *models.Template) error {
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
			if n := len(queuedInstaller(t, h.state.Redis)); n != 0 {
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
		h.state.Redis.Del(context.Background(), "dylaris:server:srv-uuid:install-start")
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
	inst := queuedInstaller(t, h.state.Redis)
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
	templatePush = func(context.Context, *handlers.AppState, int, string, string, *models.Template) error {
		return context.DeadlineExceeded
	}
	rec := httptest.NewRecorder()
	h.SetupServer(rec, installRequest("setup", templateSetup(5), false))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %s", rec.Code, rec.Body.String())
	}
	if n := len(queuedInstaller(t, h.state.Redis)); n != 0 || fs.setupWrites != 0 {
		t.Fatalf("queued %d, wrote %d after a failed push", n, fs.setupWrites)
	}
}

// A proxy template needs the node that installs a proxy over an upload; an
// older one is refused before any byte is pushed.
func TestProxyTemplateRefusedOnAnOlderNode(t *testing.T) {
	h, fs, pushes := newTemplateInstallTest(t, &models.Template{ID: 5, OwnerID: "u1", Kind: "proxy", Visibility: "personal", Software: "velocity", SoftwareVersion: "3.4.0"}, "all")
	fs.server.ServerType = "proxy"
	h.state.Redis.Set(context.Background(), "dylaris:discovery:node-lib", `{"releaseVersion":"2026.10.31"}`, 0)
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
	h.state.Redis.Set(context.Background(), "dylaris:discovery:node-lib", `{"releaseVersion":"`+templateProxySince+`"}`, 0)
	h.state.Redis.Del(context.Background(), "dylaris:server:srv-uuid:install-start")
	rec = httptest.NewRecorder()
	h.SetupServer(rec, installRequest("setup", templateSetup(5), false))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("game server: status %d, want 400: %s", rec.Code, rec.Body.String())
	}

	fs.server.ServerType = "proxy"
	h.state.Redis.Del(context.Background(), "dylaris:server:srv-uuid:install-start")
	rec = httptest.NewRecorder()
	h.SetupServer(rec, installRequest("setup", templateSetup(5), false))
	if rec.Code != http.StatusOK {
		t.Fatalf("current node: status %d: %s", rec.Code, rec.Body.String())
	}
	if inst := queuedInstaller(t, h.state.Redis); len(inst) != 1 || inst[0]["software"] != "velocity" || inst[0]["version"] != "3.4.0" {
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
		h.state.Redis.Set(ctx, lockKey, "someone-else", 0)
		rec := httptest.NewRecorder()
		h.SetupServer(rec, installRequest("setup", templateSetup(5), admin))
		if rec.Code != http.StatusConflict || decodeErrBody(t, rec) != templatePushBusy {
			t.Fatalf("admin=%v: status %d, want 409 busy: %s", admin, rec.Code, rec.Body.String())
		}
		if len(*pushes) != 0 {
			t.Fatalf("admin=%v: pushed %d while locked", admin, len(*pushes))
		}
		if v, _ := h.state.Redis.Get(ctx, lockKey).Result(); v != "someone-else" {
			t.Fatalf("admin=%v: foreign lock became %q", admin, v)
		}

		h.state.Redis.Del(ctx, lockKey, "dylaris:server:srv-uuid:install-start")
		var heldDuringPush bool
		templatePush = func(context.Context, *handlers.AppState, int, string, string, *models.Template) error {
			heldDuringPush = h.state.Redis.Exists(ctx, lockKey).Val() == 1
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
		if h.state.Redis.Exists(ctx, lockKey).Val() != 0 {
			t.Errorf("admin=%v: lock left behind after the push", admin)
		}
	}
}
