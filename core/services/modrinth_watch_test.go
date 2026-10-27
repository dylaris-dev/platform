package services

import (
	"dylaris-core/models"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestWatchTargetsAreOncePerMinecraftRelease(t *testing.T) {
	releases := []string{"26.4", "26.3", "1.21.11", "1.20.1"}
	w := models.ModrinthWatch{Current: "26.3", Targets: []string{"1.21.11", "1.20.1"}, Future: true, Delivered: []string{"1.20.1"}}
	if got := WatchTargets(w, releases); !reflect.DeepEqual(got, []string{"26.4", "1.21.11"}) {
		t.Fatalf("targets=%v", got)
	}
	w.Future = false
	if got := WatchTargets(w, releases); !reflect.DeepEqual(got, []string{"1.21.11"}) {
		t.Fatalf("specific targets=%v", got)
	}
	w.Delivered = append(w.Delivered, "1.21.11")
	if len(WatchTargets(w, releases)) != 0 {
		t.Fatal("delivered release would notify again")
	}
}

func TestWatchTargetsRotateBoundedWindow(t *testing.T) {
	releases := make([]string, 40)
	for i := range releases {
		releases[i] = fmt.Sprintf("26.%d", 40-i)
	}
	w := models.ModrinthWatch{Targets: releases, NextCheck: time.Unix(86400*20000, 0)}
	seen := map[string]bool{}
	for day := 0; day < 40; day++ {
		got := WatchTargets(w, releases)
		if len(got) != 32 {
			t.Fatalf("unbounded window: %d", len(got))
		}
		for _, target := range got {
			seen[target] = true
		}
		w.NextCheck = w.NextCheck.Add(24 * time.Hour)
	}
	if len(seen) != len(releases) {
		t.Fatalf("starved targets: %d/%d", len(seen), len(releases))
	}
}

func TestWatchPreservesProjectContentType(t *testing.T) {
	for _, kind := range []string{"", "mod", "resourcepack", "shaderpack", "shader"} {
		item := watchCompatItem(models.ModrinthWatchProject{ProjectID: "project", Title: "Project", ContentType: kind})
		expected := kind
		if kind == "shader" {
			expected = "shaderpack"
		}
		if item.ContentType != expected || item.ProjectID != "project" {
			t.Fatalf("watch lost project metadata: %+v", item)
		}
	}
}
