package packs

import (
	"context"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
)

type cloneCaptureStore struct {
	*packCleanupStore
	copied *models.PackBuild
	source int
}

func (s *cloneCaptureStore) ClonePackBuild(_ context.Context, b *models.PackBuild, source int) (int, error) {
	copy := *b
	s.copied, s.source = &copy, source
	return 42, nil
}

func TestBuildCopyGuardsSourceAndEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name      string
		packID    int
		minecraft string
		loader    string
		want      int
	}{
		{"same environment", 1, "1.20.1", "forge", 200},
		{"another pack", 2, "1.20.1", "forge", 404},
		{"another Minecraft", 1, "1.21.1", "forge", 422},
		{"another loader", 1, "1.20.1", "fabric", 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, base, _ := newPackCleanup(t)
			base.builds[0].PackID = tc.packID
			base.builds[0].Minecraft = tc.minecraft
			base.builds[0].Loader = tc.loader
			base.builds[0].MinMemory = 4096
			base.builds[0].Frozen = true
			capture := &cloneCaptureStore{packCleanupStore: base}
			h.state.Store = capture
			w := httptest.NewRecorder()
			h.CreateBuild(w, packReq("POST", map[string]string{"id": "1"}, `{"versionString":"1.1.0","minecraft":"1.20.1","loader":"forge","loaderVersion":"47.4.28","sourceBuildId":10}`))
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d: %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want == 200 {
				if capture.copied == nil || capture.source != 10 || capture.copied.MinMemory != 4096 || capture.copied.Frozen || capture.copied.Channel != models.ChannelDraft {
					t.Fatalf("invalid copy: source=%d build=%+v", capture.source, capture.copied)
				}
			} else if capture.copied != nil {
				t.Fatal("rejected source reached the copy operation")
			}
		})
	}
}
