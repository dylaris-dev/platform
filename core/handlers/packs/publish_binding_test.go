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

	"dylaris-core/models"
	"dylaris-core/store"
)

type publishBindingStore struct {
	*packCleanupStore
	packUpdates int
}

func (s *publishBindingStore) ListBuildContent(int) ([]models.BuildContentEntry, error) {
	return []models.BuildContentEntry{modrinthEntry("mods/fixture.jar")}, nil
}

func (s *publishBindingStore) PackRightsFor(owner, user string, packID int) (store.PackRights, error) {
	return store.PackRights{Found: owner == cleanupOwner && user == "editor" && packID == 1, CanEdit: user == "editor"}, nil
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

func TestPublishUsesStoredOrNewProjectAndOwnerPAT(t *testing.T) {
	for _, tc := range []struct{ name, actor, bound, want string }{{"owner existing", cleanupOwner, "bound-project", "bound-project"}, {"editor existing", "editor", "bound-project", "bound-project"}, {"editor creates first project", "editor", "", "new-server-assigned-project"}} {
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
			http.DefaultTransport = modrinthAddTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.modrinth.com" || r.Method != http.MethodPost {
					return nil, fmt.Errorf("unexpected request outside mocked Modrinth publish")
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
						ProjectID string `json:"project_id"`
					}
					if err := json.Unmarshal([]byte(r.FormValue("data")), &data); err != nil {
						return nil, err
					}
					if data.ProjectID != tc.want {
						t.Errorf("publication target=%q want stored/server-assigned %q", data.ProjectID, tc.want)
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
			if w.Code != 200 || uploads != 1 || pat.loadedFor != cleanupOwner {
				t.Fatalf("status=%d uploads=%d credential owner=%q body=%s", w.Code, uploads, pat.loadedFor, w.Body.String())
			}
			if tc.bound == "" && creates != 1 || tc.bound != "" && creates != 0 {
				t.Fatalf("project creates=%d for binding %q", creates, tc.bound)
			}
		})
	}
}
