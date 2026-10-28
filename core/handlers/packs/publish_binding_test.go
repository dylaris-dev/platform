package packs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
)

type publishBindingStore struct {
	*packCleanupStore
	packUpdates int
}

func (s *publishBindingStore) ListBuildContent(int) ([]models.BuildContentEntry, error) {
	return []models.BuildContentEntry{modrinthEntry("mods/fixture.jar")}, nil
}

// "editor" is a team member with Modrinth's upload right on the bound project.
func (s *publishBindingStore) GetModrinthMembership(user, project string) (*models.ModrinthMembership, error) {
	if user == "editor" && project == "bound-project" {
		return &models.ModrinthMembership{State: "member", Permissions: services.ModrinthPermUploadVersion, CheckedAt: time.Now()}, nil
	}
	return s.packCleanupStore.GetModrinthMembership(user, project)
}
func (s *publishBindingStore) UpdatePack(p *models.Pack) error {
	cp := *p
	s.pack = &cp
	s.packUpdates++
	return nil
}

type publishBindingPAT struct{ loadedFor string }

func (p *publishBindingPAT) LoadPAT(owner string) (string, string, error) {
	p.loadedFor = owner
	return "test-owner-pat", "owner", nil
}

func TestPackPatchCannotRebindModrinthProjects(t *testing.T) {
	for _, actor := range []string{cleanupOwner, "editor"} {
		t.Run(actor, func(t *testing.T) {
			h, base, _ := newPackCleanup(t)
			base.pack.ModrinthProjectID = "bound-project"
			base.pack.ModrinthVisibility = "unlisted"
			fs := &publishBindingStore{packCleanupStore: base}
			h.state.Store = fs
			r := packReq(http.MethodPatch, map[string]string{"id": "1"}, `{"name":"Allowed edit","ownerId":"attacker","modrinthProjectId":"another-owner-project","projectId":"another-owner-project","modrinthVisibility":"listed"}`)
			r = r.WithContext(context.WithValue(r.Context(), "userID", actor))
			w := httptest.NewRecorder()
			h.Update(w, r)
			if w.Code != 200 {
				t.Fatalf("allowed metadata update status=%d: %s", w.Code, w.Body.String())
			}
			if fs.pack.InternalName != "Allowed edit" || fs.pack.OwnerID != cleanupOwner || fs.pack.ModrinthProjectID != "bound-project" || fs.pack.ModrinthVisibility != "unlisted" {
				t.Fatalf("request changed publishing authority: %+v", fs.pack)
			}
		})
	}
}

// Publishing targets the pack's bound project (or creates the first one for an
// unbound pack) and uses the PUBLISHER's own token: Modrinth checks their team
// right again (owner decision 2026-10-28).
func TestPublishUsesStoredOrNewProjectAndCallerPAT(t *testing.T) {
	for _, tc := range []struct{ name, actor, bound, want string }{{"owner existing", cleanupOwner, "bound-project", "bound-project"}, {"teammate existing", "editor", "bound-project", "bound-project"}, {"creator of an unbound pack creates the project", cleanupOwner, "", "new-server-assigned-project"}} {
		t.Run(tc.name, func(t *testing.T) {
			h, base, dir := newPackCleanup(t)
			base.pack.ModrinthProjectID = tc.bound
			base.pack.ModrinthVisibility = "unlisted"
			base.builds[0].Frozen = true
			base.builds[0].MrpackStorageKey = "modpacks/frozen/pack.mrpack"
			base.builds[0].Minecraft = "1.20.1"
			base.builds[0].Loader = "forge"
			base.builds[0].Channel = models.ChannelBeta
			fs := &publishBindingStore{packCleanupStore: base}
			h.state.Store = fs
			pat := &publishBindingPAT{}
			h.SetPATLoader(pat)
			prov := modpackProviderAt(t, dir)
			if err := prov.Put(context.Background(), base.builds[0].MrpackStorageKey, zipBytes(t, map[string]string{"modrinth.index.json": "{}"})); err != nil {
				t.Fatal(err)
			}
			// A transport fake intercepts every request; no real Modrinth publish occurs.
			old := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = old })
			creates, uploads := 0, 0
			envSet := ""
			http.DefaultTransport = modrinthAddTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.modrinth.com" {
					return nil, fmt.Errorf("unexpected request outside mocked Modrinth publish")
				}
				// After the upload: read the project, then set the new version's environment.
				if r.Method == http.MethodGet && r.URL.Path == "/v2/project/"+tc.want {
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"` + tc.want + `","versions":["uploaded-version"]}`))}, nil
				}
				if r.Method == http.MethodPatch && r.URL.Path == "/v3/version/uploaded-version" {
					b, _ := io.ReadAll(r.Body)
					var env struct {
						Environment string `json:"environment"`
					}
					_ = json.Unmarshal(b, &env)
					envSet = env.Environment
					return &http.Response{StatusCode: 204, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				if r.Method != http.MethodPost {
					return nil, fmt.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "test-owner-pat" {
					t.Error("publish did not use the owner's credential")
				}
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					return nil, err
				}
				defer r.MultipartForm.RemoveAll()
				var response string
				switch r.URL.Path {
				case "/v2/project":
					creates++
					response = `{"id":"new-server-assigned-project","slug":"pack"}`
				case "/v2/version":
					uploads++
					var data struct {
						ProjectID   string `json:"project_id"`
						Status      string `json:"status"`
						VersionType string `json:"version_type"`
					}
					if err := json.Unmarshal([]byte(r.FormValue("data")), &data); err != nil {
						return nil, err
					}
					if data.ProjectID != tc.want {
						t.Errorf("publication target=%q want stored/server-assigned %q", data.ProjectID, tc.want)
					}
					// An unlisted version never shows in the project's version list,
					// not even to its owner; the draft project is what keeps it private.
					if data.Status != "listed" || data.VersionType != "beta" {
						t.Errorf("version status=%q type=%q, want listed beta", data.Status, data.VersionType)
					}
					response = `{"id":"uploaded-version"}`
				default:
					return nil, fmt.Errorf("unexpected mocked endpoint %s", r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
			})
			r := packReq(http.MethodPost, map[string]string{"id": "1", "buildId": "10"}, `{"channel":"beta","ackNonModrinth":true,"projectId":"another-owner-project","modrinthProjectId":"another-owner-project","ownerId":"editor"}`)
			r = r.WithContext(context.WithValue(r.Context(), "userID", tc.actor))
			w := httptest.NewRecorder()
			h.PublishModrinth(w, r)
			if w.Code != 200 || uploads != 1 || pat.loadedFor != tc.actor {
				t.Fatalf("status=%d uploads=%d credential owner=%q body=%s", w.Code, uploads, pat.loadedFor, w.Body.String())
			}
			if envSet != "client_and_server" {
				t.Fatalf("first version environment = %q, want client_and_server (Modrinth blocks review on unknown)", envSet)
			}
			if tc.bound == "" && creates != 1 || tc.bound != "" && creates != 0 {
				t.Fatalf("project creates=%d for binding %q", creates, tc.bound)
			}
		})
	}
}
