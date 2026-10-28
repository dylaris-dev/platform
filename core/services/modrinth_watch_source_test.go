package services

import (
	"context"
	"dylaris-core/models"
	"dylaris-core/store"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type liveWatchStore struct {
	store.Store
	pack        *models.Pack
	builds      []models.PackBuild
	rows        map[int][]models.BuildContentEntry
	allowed     bool
	deleted     bool
	completions int
	failure     string
	checked     models.ModrinthWatch
}

func (s *liveWatchStore) GetPack(int) (*models.Pack, error)              { return s.pack, nil }
func (s *liveWatchStore) ListPackBuilds(int) ([]models.PackBuild, error) { return s.builds, nil }
func (s *liveWatchStore) ListBuildContent(id int) ([]models.BuildContentEntry, error) {
	return s.rows[id], nil
}
func (s *liveWatchStore) GetUserByID(string) (*models.User, error) { return &models.User{}, nil }

// The watcher reaches the pack through its Modrinth team while allowed; the
// answers are fresh, so the checker never has to ask Modrinth.
func (s *liveWatchStore) GetModrinthMembership(string, string) (*models.ModrinthMembership, error) {
	if s.allowed {
		return &models.ModrinthMembership{State: "member", CheckedAt: time.Now()}, nil
	}
	return &models.ModrinthMembership{State: "none", CheckedAt: time.Now()}, nil
}
func (s *liveWatchStore) ListModrinthWatches(context.Context, string, bool) ([]models.ModrinthWatch, error) {
	return []models.ModrinthWatch{{ID: 1, UserID: "viewer", PackID: 1, FollowPack: true, Loader: "forge", Current: "1.20.1", Targets: []string{"1.21.1"}}}, nil
}
func (s *liveWatchStore) DeleteModrinthWatch(context.Context, int64, string) error {
	s.deleted = true
	return nil
}
func (s *liveWatchStore) CompleteModrinthWatch(_ context.Context, w models.ModrinthWatch, _ []models.ModrinthWatchVersion, failure string) error {
	s.completions++
	s.checked = w
	s.failure = failure
	return nil
}

func TestPackWatchRechecksLiveSourceAndRevocationBeforeDelivery(t *testing.T) {
	for _, mode := range []string{"stable", "recommendation changes", "mod list changes", "access revoked", "draft only"} {
		t.Run(mode, func(t *testing.T) {
			selected := 1
			entry := models.BuildContentEntry{PrettyName: "Fixture mod", ContentType: models.ContentTypeMod}
			entry.ModrinthProjectID = "project"
			s := &liveWatchStore{pack: &models.Pack{ID: 1, OwnerID: "owner", InternalName: "Live pack", ModrinthProjectID: "packproj", RecommendedBuildID: &selected}, builds: []models.PackBuild{{ID: 1, PackID: 1, VersionString: "1.0", Channel: "release"}, {ID: 2, PackID: 1, VersionString: "1.1", Channel: "draft"}}, rows: map[int][]models.BuildContentEntry{1: {entry}, 2: {entry}}, allowed: true}
			if mode == "draft only" {
				s.pack.RecommendedBuildID = nil
				s.builds = s.builds[1:]
			}
			before := compatHTTP
			defer func() { compatHTTP = before }()
			compatHTTP = &http.Client{Transport: publishingContractTransport(func(r *http.Request) (*http.Response, error) {
				body := `{"hits":[{"project_id":"project"}]}`
				if strings.HasSuffix(r.URL.Path, "/tag/game_version") {
					body = `[{"version":"1.21.1","version_type":"release"},{"version":"1.20.1","version_type":"release"}]`
				} else {
					switch mode {
					case "recommendation changes":
						selected = 2
					case "access revoked":
						s.allowed = false
					case "mod list changes":
						newMod := entry
						newMod.ModrinthProjectID = "new-project"
						s.rows[1] = []models.BuildContentEntry{newMod}
					}
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			c := &ModrinthWatchChecker{Store: s}
			if err := c.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "stable":
				if s.completions != 1 || s.failure != "" || s.checked.CheckedBuildID != 1 || s.checked.Title != "Live pack / 1.0" {
					t.Fatalf("wrong live source result: %+v", s)
				}
			case "draft only":
				if s.completions != 1 || s.failure == "" || s.checked.CheckedBuildID != 0 {
					t.Fatal("draft was silently promoted")
				}
			case "access revoked":
				if !s.deleted || s.completions != 0 {
					t.Fatal("revoked source produced an alert")
				}
			default:
				if s.completions != 0 || s.deleted {
					t.Fatal("obsolete source was delivered instead of left due")
				}
			}
		})
	}
}
