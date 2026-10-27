package models

import (
	"testing"
	"time"
)

func TestRecommendedPackBuildNeverPromotesDraftImplicitly(t *testing.T) {
	p := &Pack{ID: 1}
	builds := []PackBuild{{ID: 4, PackID: 1, Channel: ChannelDraft, CreatedAt: time.Unix(400, 0)}, {ID: 3, PackID: 1, Channel: ChannelRelease, CreatedAt: time.Unix(300, 0)}, {ID: 2, PackID: 1, Channel: ChannelRelease, CreatedAt: time.Unix(200, 0)}}
	if got := RecommendedPackBuild(p, builds); got == nil || got.ID != 3 {
		t.Fatalf("release fallback: %+v", got)
	}
	selected := 2
	p.RecommendedBuildID = &selected
	if got := RecommendedPackBuild(p, builds); got == nil || got.ID != 2 {
		t.Fatal("explicit recommendation was ignored")
	}
	selected = 4
	if got := RecommendedPackBuild(p, builds); got == nil || got.ID != 4 {
		t.Fatal("explicit draft choice was ignored")
	}
	p.RecommendedBuildID = nil
	if RecommendedPackBuild(p, builds[:1]) != nil {
		t.Fatal("draft was implicitly promoted")
	}
	selected = 9
	p.RecommendedBuildID = &selected
	builds = append(builds, PackBuild{ID: 9, PackID: 2, Channel: ChannelRelease})
	if got := RecommendedPackBuild(p, builds); got == nil || got.ID != 3 {
		t.Fatal("foreign recommendation selected")
	}
}
