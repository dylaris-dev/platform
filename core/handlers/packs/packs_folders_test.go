package packs

import (
	"context"
	"dylaris-core/models"
	"dylaris-core/services"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type folderTestStore struct {
	*packScopeFakeStore
	changed bool
	content []models.BuildContentEntry
}

func (s *folderTestStore) ListPackFolders(context.Context, int) ([]string, error) {
	return []string{}, nil
}
func (s *folderTestStore) SetPackFolder(context.Context, int, string, bool) error {
	s.changed = true
	return nil
}
func (s *folderTestStore) ListBuildContent(int) ([]models.BuildContentEntry, error) {
	return s.content, nil
}

// Pack 1 is bound to "proj": alice manages it, "editor" may upload versions.
func (s *folderTestStore) GetModrinthMembership(user, project string) (*models.ModrinthMembership, error) {
	switch user {
	case "alice":
		return member(services.ModrinthPermAll), nil
	case "editor":
		return member(services.ModrinthPermUploadVersion), nil
	}
	return nil, nil
}
func (s *folderTestStore) UpsertModrinthMembership(*models.ModrinthMembership) error { return nil }
func TestPackFolderPermissionsAndPaths(t *testing.T) {
	for _, tc := range []struct {
		name, user, method, path, build string
		frozen, nonempty                bool
		status                          int
	}{
		{"read", "alice", http.MethodGet, "", "10", false, false, 200},
		{"create", "alice", http.MethodPost, "kubejs/server_scripts", "10", false, false, 200},
		{"edit grant", "editor", http.MethodPost, "config/new", "10", false, false, 200},
		{"no delete grant", "editor", http.MethodDelete, "config/new", "10", false, false, 403},
		{"frozen", "alice", http.MethodPost, "config/new", "10", true, false, 409},
		{"foreign build", "alice", http.MethodPost, "config/new", "20", false, false, 403},
		{"traversal", "alice", http.MethodPost, "../escape", "10", false, false, 400},
		{"absolute", "alice", http.MethodPost, "/tmp/x", "10", false, false, 400},
		{"nonempty", "alice", http.MethodDelete, "config", "10", false, true, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPackScopeHandler()
			base := h.state.Store.(*packScopeFakeStore)
			base.builds[10].Frozen = tc.frozen
			base.packs[1].ModrinthProjectID = "proj"
			s := &folderTestStore{packScopeFakeStore: base}
			if tc.nonempty {
				e := models.BuildContentEntry{}
				e.TargetPath = "config/file.toml"
				s.content = []models.BuildContentEntry{e}
			}
			h.state.Store = s
			r := packContentRequest(t, tc.user, "1", tc.build)
			r.Method = tc.method
			r.Body = io.NopCloser(strings.NewReader(`{"path":"` + tc.path + `"}`))
			w := httptest.NewRecorder()
			h.Folders(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.status, w.Body.String())
			}
			if tc.status != 200 && s.changed {
				t.Fatal("rejected operation mutated folders")
			}
		})
	}
}
