package packs

import (
	"context"
	"dylaris-core/models"
	"dylaris-core/store"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type forkAccessStore struct {
	*packScopeFakeStore
	called      bool
	targetOwner string
}

func (s *forkAccessStore) PackRightsFor(owner, user string, pack int) (store.PackRights, error) {
	return store.PackRights{Found: owner == "alice" && user == "reader" && pack == 1}, nil
}
func (s *forkAccessStore) ListPacksByOwner(string) ([]models.Pack, error) {
	return []models.Pack{}, nil
}
func (s *forkAccessStore) ListPackBuilds(id int) ([]models.PackBuild, error) {
	if id == 3 {
		return []models.PackBuild{{ID: 30, PackID: 3}}, nil
	}
	return []models.PackBuild{}, nil
}
func (s *forkAccessStore) ListBuildContent(int) ([]models.BuildContentEntry, error) {
	return []models.BuildContentEntry{}, nil
}
func (s *forkAccessStore) ListPackFolders(context.Context, int) ([]string, error) {
	return []string{}, nil
}
func (s *forkAccessStore) ForkPackBuild(_ context.Context, f *models.PackFork) (int, int, error) {
	s.called = true
	s.targetOwner = f.Target.OwnerID
	return 0, 0, fmt.Errorf("fixture rollback")
}

func TestForkBuildValidatesSourceAndEmptyTargetBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name, user, build, body string
		status                  int
		reachesStore            bool
	}{
		{"foreign source build", "alice", "20", `{"name":"Fork"}`, 403, false},
		{"source outsider", "outsider", "10", `{"name":"Fork"}`, 403, false},
		{"foreign target", "alice", "10", `{"targetPackId":2}`, 403, false},
		{"nonempty target", "alice", "10", `{"targetPackId":3}`, 409, false},
		{"missing name", "alice", "10", `{}`, 400, false},
		{"unsafe slug", "alice", "10", `{"name":"Fork","slug":"../escape"}`, 400, false},
		{"unsafe version", "alice", "10", `{"name":"Fork","versionString":"../escape"}`, 400, false},
		{"readonly source can export into own writable account", "reader", "10", `{"name":"Private fork","versionString":"1.0"}`, 409, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPackScopeHandler()
			base := h.state.Store.(*packScopeFakeStore)
			base.packs[3] = &models.Pack{ID: 3, OwnerID: "alice"}
			s := &forkAccessStore{packScopeFakeStore: base}
			h.state.Store = s
			request := packContentRequest(t, tc.user, "1", tc.build)
			request.Method = http.MethodPost
			request.Body = io.NopCloser(strings.NewReader(tc.body))
			response := httptest.NewRecorder()
			h.ForkBuild(response, request)
			if response.Code != tc.status || s.called != tc.reachesStore {
				t.Fatalf("status=%d, called=%v; expected %d,%v", response.Code, s.called, tc.status, tc.reachesStore)
			}
			if tc.reachesStore && s.targetOwner != tc.user {
				t.Fatal("new fork inherited source owner")
			}
		})
	}
}
