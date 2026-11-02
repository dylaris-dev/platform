package templates

import (
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

// modrinthServer serves /version/{id} from versions (raw JSON) and points
// fetchModrinthVersion at it; an unknown id is a 404 like Modrinth's.
func modrinthServer(t *testing.T, versions map[string]string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := versions[strings.TrimPrefix(r.URL.Path, "/version/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	fetchModrinthVersion = func(ctx context.Context, id string) (*services.ModrinthVersion, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/version/"+id, nil)
		res, err := srv.Client().Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return nil, &services.ModrinthHTTPError{Path: "/version/" + id, Status: res.StatusCode}
		}
		var v services.ModrinthVersion
		return &v, json.NewDecoder(res.Body).Decode(&v)
	}
}

func versionJSON(id, file, url string, hashes bool, loaders, games string) string {
	h := `{}`
	o1, o512 := sums("other")
	if hashes {
		s1, s512 := sums(id)
		h = `{"sha1":"` + s1 + `","sha512":"` + s512 + `"}`
	}
	return fmt.Sprintf(`{"id":%q,"project_id":"proj-%s","loaders":[%s],"game_versions":[%s],"files":[
		{"filename":"other.jar","url":"https://cdn.modrinth.com/data/x/other.jar","primary":false,"size":1,"hashes":{"sha1":"%s","sha512":"%s"}},
		{"filename":%q,"url":%q,"primary":true,"size":42,"hashes":%s}]}`, id, id, loaders, games, o1, o512, file, url, h)
}

// sums are stand-in checksums of the right shape, distinct per id.
func sums(id string) (string, string) {
	a, b := sha1.Sum([]byte(id)), sha512.Sum512([]byte(id))
	return hex.EncodeToString(a[:]), hex.EncodeToString(b[:])
}

func seedSoftware(t *testing.T, st *fakeStore, software, mc string) *models.Template {
	t.Helper()
	tpl := seedTemplate(t, st, ownerID, "personal")
	st.templates[tpl.ID].Software = software
	st.templates[tpl.ID].McVersion = mc
	tpl.Software, tpl.McVersion = software, mc
	return tpl
}

func addModrinth(h *TemplatesHandler, tplID int, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.AddModrinth(rec, withID(asUser(httptest.NewRequest("POST", "/x", strings.NewReader(body)), ownerID, false), tplID))
	return rec
}

func TestAddModrinth(t *testing.T) {
	cdn := "https://cdn.modrinth.com/data/x/"
	versions := map[string]string{
		"lp":      versionJSON("lp", "LuckPerms.jar", cdn+"LuckPerms.jar", true, `"paper","spigot"`, `"1.21.1"`),
		"sodium":  versionJSON("sodium", "sodium.jar", cdn+"sodium.jar", true, `"fabric"`, `"1.21.1"`),
		"offcdn":  versionJSON("offcdn", "x.jar", "https://evil.example/x.jar", true, `"paper"`, `"1.21.1"`),
		"nohash":  versionJSON("nohash", "x.jar", cdn+"x.jar", false, `"paper"`, `"1.21.1"`),
		"slash":   versionJSON("slash", "../x.jar", cdn+"x.jar", true, `"paper"`, `"1.21.1"`),
		"forgeV":  versionJSON("forgeV", "f.jar", cdn+"f.jar", true, `"forge"`, `"1.20.1"`),
		"lpNewer": versionJSON("lpNewer", "LuckPerms.jar", cdn+"LuckPerms.jar", true, `"paper"`, `"1.21.1"`),
		// Same project as lp, another file name: replaces lp at its own path.
		"lpRenamed": strings.Replace(versionJSON("lpRenamed", "LuckPerms-5.jar", cdn+"LuckPerms-5.jar", true, `"paper"`, `"1.21.1"`),
			`"project_id":"proj-lpRenamed"`, `"project_id":"proj-lp"`, 1),
		"badhex": strings.Replace(versionJSON("badhex", "x.jar", cdn+"x.jar", true, `"paper"`, `"1.21.1"`),
			first(sums("badhex")), strings.Repeat("z", 40), 1),
	}

	cases := []struct {
		name, software, mc, version string
		policy                      string
		code                        int
		path, warning               string
	}{
		{name: "paper goes to plugins", software: "paper", mc: "1.21.1", version: "lp", code: 200, path: "plugins/LuckPerms.jar"},
		{name: "fabric goes to mods", software: "Fabric", mc: "1.21.1", version: "sodium", code: 200, path: "mods/sodium.jar"},
		{name: "works under uploads policy", software: "paper", version: "lp", policy: services.TemplatePolicyUploads, code: 200, path: "plugins/LuckPerms.jar"},
		{name: "mismatch is a warning", software: "fabric", mc: "1.21.1", version: "forgeV", code: 200, path: "mods/f.jar", warning: "not listed for fabric and is not listed for Minecraft 1.21.1"},
		{name: "off-cdn url refused", software: "paper", version: "offcdn", code: 400},
		{name: "missing hashes refused", software: "paper", version: "nohash", code: 400},
		{name: "malformed sha1 refused", software: "paper", version: "badhex", code: 400},
		{name: "path in file name refused", software: "paper", version: "slash", code: 400},
		{name: "vanilla refused", software: "vanilla", version: "lp", code: 400},
		{name: "no software refused", software: "", version: "lp", code: 400},
		{name: "unknown version", software: "paper", version: "missing", code: 404},
		{name: "bad id", software: "paper", version: "a/b", code: 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var settings map[string]string
			if c.policy != "" {
				settings = map[string]string{services.TemplatesContentPolicyKey: c.policy}
			}
			h, st := newTestHandler(t, settings)
			modrinthServer(t, versions)
			tpl := seedSoftware(t, st, c.software, c.mc)
			rec := addModrinth(h, tpl.ID, `{"versionId":"`+c.version+`"}`)
			if rec.Code != c.code {
				t.Fatalf("code %d, want %d: %s", rec.Code, c.code, rec.Body)
			}
			refs, _ := st.ListTemplateModrinth(tpl.ID)
			if c.code != 200 {
				if len(refs) != 0 {
					t.Errorf("refs %+v after a refusal", refs)
				}
				return
			}
			var body struct {
				Reference models.TemplateModrinthRef `json:"reference"`
				Warning   string                     `json:"warning"`
			}
			json.Unmarshal(rec.Body.Bytes(), &body)
			if len(refs) != 1 || refs[0].Path != c.path || body.Reference.Path != c.path {
				t.Fatalf("refs %+v body %+v, want %s", refs, body.Reference, c.path)
			}
			s1, s512 := sums(c.version)
			if got := refs[0]; got.VersionID != c.version || got.ProjectID != "proj-"+c.version || got.Sha1 != s1 ||
				got.Sha512 != s512 || got.SizeBytes != 42 || !strings.HasPrefix(got.URL, "https://cdn.modrinth.com/") {
				t.Errorf("ref %+v: not the primary file", got)
			}
			if c.warning == "" && body.Warning != "" || !strings.Contains(body.Warning, c.warning) {
				t.Errorf("warning %q, want %q", body.Warning, c.warning)
			}
		})
	}

	t.Run("same path replaces and the stored file goes", func(t *testing.T) {
		h, st := newTestHandler(t, map[string]string{services.TemplatesContentPolicyKey: services.TemplatePolicyUploads})
		modrinthServer(t, versions)
		tpl := seedSoftware(t, st, "paper", "1.21.1")
		if err := h.state.CoreStorage.TemplateWrite(context.Background(), tpl, "plugins/LuckPerms.jar", strings.NewReader("own"), 3); err != nil {
			t.Fatal(err)
		}
		if err := h.state.CoreStorage.TemplateWrite(context.Background(), tpl, "plugins/keep.yml", strings.NewReader("k"), 1); err != nil {
			t.Fatal(err)
		}
		for _, v := range []string{"lp", "lpNewer"} {
			if rec := addModrinth(h, tpl.ID, `{"versionId":"`+v+`"}`); rec.Code != 200 {
				t.Fatalf("%s: %d %s", v, rec.Code, rec.Body)
			}
		}
		refs, _ := st.ListTemplateModrinth(tpl.ID)
		if len(refs) != 1 || refs[0].VersionID != "lpNewer" {
			t.Errorf("refs %+v, want one ref of lpNewer", refs)
		}
		if got := storedFiles(t, h, tpl); len(got) != 1 || got[0] != "plugins/keep.yml" {
			t.Errorf("stored %v, want only plugins/keep.yml", got)
		}
	})

	t.Run("another version of the same project replaces it", func(t *testing.T) {
		h, st := newTestHandler(t, nil)
		modrinthServer(t, versions)
		tpl := seedSoftware(t, st, "paper", "1.21.1")
		st.UpsertTemplateModrinth(&models.TemplateModrinthRef{TemplateID: tpl.ID, Path: "plugins/keep.jar", ProjectID: "proj-other"})
		for _, v := range []string{"lp", "lpRenamed"} {
			if rec := addModrinth(h, tpl.ID, `{"versionId":"`+v+`"}`); rec.Code != 200 {
				t.Fatalf("%s: %d %s", v, rec.Code, rec.Body)
			}
		}
		refs, _ := st.ListTemplateModrinth(tpl.ID)
		if len(refs) != 2 || refs[0].Path != "plugins/LuckPerms-5.jar" || refs[1].Path != "plugins/keep.jar" {
			t.Errorf("refs %+v, want LuckPerms-5.jar and keep.jar", refs)
		}
	})

	t.Run("references are capped per template", func(t *testing.T) {
		h, st := newTestHandler(t, nil)
		modrinthServer(t, versions)
		tpl := seedSoftware(t, st, "paper", "1.21.1")
		for i := 0; i < maxTemplateRefs-1; i++ {
			st.UpsertTemplateModrinth(&models.TemplateModrinthRef{TemplateID: tpl.ID, Path: fmt.Sprintf("plugins/p%d.jar", i), ProjectID: fmt.Sprint("p", i)})
		}
		if rec := addModrinth(h, tpl.ID, `{"versionId":"lp"}`); rec.Code != 200 {
			t.Fatalf("the 500th: %d %s", rec.Code, rec.Body)
		}
		// At the cap a replacement of the same project still works...
		if rec := addModrinth(h, tpl.ID, `{"versionId":"lpRenamed"}`); rec.Code != 200 {
			t.Fatalf("replace at the cap: %d %s", rec.Code, rec.Body)
		}
		// ...a new one does not.
		if rec := addModrinth(h, tpl.ID, `{"versionId":"sodium"}`); rec.Code != http.StatusConflict {
			t.Fatalf("the 501st: %d %s, want 409", rec.Code, rec.Body)
		}
		if refs, _ := st.ListTemplateModrinth(tpl.ID); len(refs) != maxTemplateRefs {
			t.Errorf("%d refs, want %d", len(refs), maxTemplateRefs)
		}
	})

	t.Run("a reference does not open as text", func(t *testing.T) {
		h, st := newTestHandler(t, nil)
		modrinthServer(t, versions)
		tpl := seedSoftware(t, st, "paper", "")
		addModrinth(h, tpl.ID, `{"versionId":"lp"}`)
		rec := httptest.NewRecorder()
		h.GetText(rec, withID(asUser(httptest.NewRequest("GET", "/x?path=plugins/LuckPerms.jar", nil), ownerID, false), tpl.ID))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Modrinth reference") {
			t.Errorf("%d %s, want 400 naming the reference", rec.Code, rec.Body)
		}
		rec = httptest.NewRecorder()
		h.GetText(rec, withID(asUser(httptest.NewRequest("GET", "/x?path=plugins/none.yml", nil), ownerID, false), tpl.ID))
		if rec.Code != http.StatusNotFound {
			t.Errorf("missing file: %d, want 404", rec.Code)
		}
	})

	t.Run("a stranger may not add", func(t *testing.T) {
		h, st := newTestHandler(t, nil)
		modrinthServer(t, versions)
		tpl := seedSoftware(t, st, "paper", "")
		rec := httptest.NewRecorder()
		h.AddModrinth(rec, withID(asUser(httptest.NewRequest("POST", "/x", strings.NewReader(`{"versionId":"lp"}`)), strangerID, false), tpl.ID))
		if rec.Code != http.StatusNotFound {
			t.Errorf("code %d, want 404", rec.Code)
		}
	})
}

func TestDeleteModrinth(t *testing.T) {
	h, st := newTestHandler(t, nil)
	mine := seedTemplate(t, st, ownerID, "personal")
	other := seedTemplate(t, st, ownerID, "personal")
	a := &models.TemplateModrinthRef{TemplateID: mine.ID, Path: "mods/a.jar"}
	b := &models.TemplateModrinthRef{TemplateID: other.ID, Path: "mods/b.jar"}
	st.UpsertTemplateModrinth(a)
	st.UpsertTemplateModrinth(b)
	del := func(tplID, refID int) int {
		rec := httptest.NewRecorder()
		req := asUser(httptest.NewRequest("DELETE", "/x", nil), ownerID, false)
		req = mux.SetURLVars(req, map[string]string{"id": fmt.Sprint(tplID), "refId": fmt.Sprint(refID)})
		h.DeleteModrinth(rec, req)
		return rec.Code
	}
	if code := del(mine.ID, b.ID); code != http.StatusNotFound {
		t.Errorf("another template's ref: %d, want 404", code)
	}
	if refs, _ := st.ListTemplateModrinth(other.ID); len(refs) != 1 {
		t.Errorf("other template's ref was removed")
	}
	if code := del(mine.ID, a.ID); code != http.StatusOK {
		t.Errorf("own ref: %d, want 200", code)
	}
	if refs, _ := st.ListTemplateModrinth(mine.ID); len(refs) != 0 {
		t.Errorf("refs %+v, want none", refs)
	}
}

func first(a, _ string) string { return a }
