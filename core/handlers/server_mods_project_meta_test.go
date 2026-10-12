package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

type projectMetaFakeStore struct {
	store.Store
	mods   []models.ServerMod
	stored map[int]string
}

func (f *projectMetaFakeStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, ActiveSubServer: "survival"}, nil
}

func (f *projectMetaFakeStore) ListServerMods(serverID int, sub string) ([]models.ServerMod, error) {
	return append([]models.ServerMod(nil), f.mods...), nil
}

func (f *projectMetaFakeStore) SetServerModProjectMeta(id int, slug, title string) error {
	f.stored[id] = slug + "|" + title
	return nil
}

// A jar identified by its hash was stored under its version name with no slug,
// so Spark identified that way was never recognised as Spark. Listing heals it
// from the project, once, and leaves rows that already have a slug alone.
func TestListGivesIdentifiedModsTheirProjectTitle(t *testing.T) {
	rows := []models.ServerMod{
		{ID: 1, ModrinthProjectID: "l6YH9Als", Title: "spark 1.10.53 for fabric"},
		{ID: 2, ModrinthProjectID: "AANobbMI", ModrinthProjectSlug: "sodium", Title: "Sodium"},
		{ID: 3, ModrinthProjectID: "gone0000", Title: "removed 1.0"},
	}
	tests := []struct {
		name       string
		fetchErr   error
		wantTitles []string
		wantStored map[int]string
	}{
		{
			name:       "project found",
			wantTitles: []string{"spark", "Sodium", "removed 1.0"},
			wantStored: map[int]string{1: "spark|spark"},
		},
		{
			name:       "Modrinth unreachable",
			fetchErr:   errors.New("timeout"),
			wantTitles: []string{"spark 1.10.53 for fabric", "Sodium", "removed 1.0"},
			wantStored: map[int]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var asked []string
			prev := fetchModProjects
			fetchModProjects = func(_ context.Context, ids []string) (map[string]services.ModrinthProjectMeta, error) {
				asked = ids
				if tt.fetchErr != nil {
					return nil, tt.fetchErr
				}
				return map[string]services.ModrinthProjectMeta{
					"l6YH9Als": {ID: "l6YH9Als", Slug: "spark", Title: "spark"},
				}, nil
			}
			t.Cleanup(func() { fetchModProjects = prev })

			fs := &projectMetaFakeStore{mods: rows, stored: map[int]string{}}
			h := &ServerModsHandler{state: &AppState{Store: fs}}
			req := mux.SetURLVars(httptest.NewRequest("GET", "/api/servers/7/mods", nil), map[string]string{"id": "7"})
			rec := httptest.NewRecorder()
			h.List(rec, req)

			var body struct {
				Mods []models.ServerMod `json:"mods"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			var titles []string
			for _, m := range body.Mods {
				titles = append(titles, m.Title)
			}
			if len(titles) != len(tt.wantTitles) {
				t.Fatalf("titles = %v, want %v", titles, tt.wantTitles)
			}
			for i := range titles {
				if titles[i] != tt.wantTitles[i] {
					t.Fatalf("titles = %v, want %v", titles, tt.wantTitles)
				}
			}
			if len(asked) != 2 || asked[0] != "l6YH9Als" || asked[1] != "gone0000" {
				t.Fatalf("asked Modrinth for %v, want only the rows without a slug", asked)
			}
			if len(fs.stored) != len(tt.wantStored) || fs.stored[1] != tt.wantStored[1] {
				t.Fatalf("stored %v, want %v", fs.stored, tt.wantStored)
			}
			if tt.fetchErr == nil && body.Mods[0].ModrinthProjectSlug != "spark" {
				t.Fatalf("slug = %q, want spark", body.Mods[0].ModrinthProjectSlug)
			}
		})
	}
}

// The mod list used to be a database read. A Modrinth that does not answer must
// not hold it open: the rows come back unhealed once the wait runs out.
func TestListDoesNotWaitOnASlowModrinth(t *testing.T) {
	prevFetch, prevWait := fetchModProjects, modMetaWait
	fetchModProjects = func(ctx context.Context, _ []string) (map[string]services.ModrinthProjectMeta, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	modMetaWait = 50 * time.Millisecond
	t.Cleanup(func() { fetchModProjects, modMetaWait = prevFetch, prevWait })

	fs := &projectMetaFakeStore{mods: []models.ServerMod{{ID: 1, ModrinthProjectID: "p1", Title: "v1"}}, stored: map[int]string{}}
	h := &ServerModsHandler{state: &AppState{Store: fs}}
	req := mux.SetURLVars(httptest.NewRequest("GET", "/api/servers/7/mods", nil), map[string]string{"id": "7"})
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.List(rec, req); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the mod list waited on Modrinth past its bound")
	}
	if rec.Code != 200 || len(fs.stored) != 0 {
		t.Fatalf("status %d, stored %v", rec.Code, fs.stored)
	}
}
