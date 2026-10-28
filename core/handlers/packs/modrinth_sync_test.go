package packs

import (
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
)

func TestNewModrinthVersionsOnlyAfterTheNewestImported(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }
	at := func(d int) *time.Time { x := day(d); return &x }
	versions := []services.ModrinthProjectVersion{
		{ID: "v5", DatePublished: day(25)}, {ID: "v4", DatePublished: day(20)},
		{ID: "v3", DatePublished: day(15)}, {ID: "v2", DatePublished: day(10)}, {ID: "v1", DatePublished: day(5)},
	}
	// The owner imported only v3: v1 and v2 are older and stay missing.
	builds := []models.PackBuild{{ID: 1, ModrinthVersionID: "v3", PublishedAt: at(15)}, {ID: 2}}
	got := newModrinthVersions(versions, builds)
	if len(got) != 2 || got[0].ID != "v4" || got[1].ID != "v5" {
		t.Fatalf("got %v, want v4 then v5", got)
	}
	if got := newModrinthVersions(versions, []models.PackBuild{{ID: 2}}); len(got) != 0 {
		t.Fatalf("a pack without any imported version synced %v automatically", got)
	}
}
