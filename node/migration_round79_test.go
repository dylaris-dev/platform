package main

import (
	"context"
	"os"
	"path/filepath"

	"dylaris-pkg/queue"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The announced size is the source node's own report; the free space is not.
func TestPullCapIsBoundedByFreeSpace(t *testing.T) {
	prev := restoreDiskBudget
	t.Cleanup(func() { restoreDiskBudget = prev })
	restoreDiskBudget = func(string) int64 { return 1000 }

	for _, c := range []struct{ announced, want int64 }{
		{0, 1000},    // older Core, no size: still bounded
		{500, 500},   // honest small archive
		{5000, 1000}, // a size the disk cannot hold
	} {
		if got := pullCap(c.announced, "/x"); got != c.want {
			t.Errorf("pullCap(%d) = %d, want %d", c.announced, got, c.want)
		}
	}
	restoreDiskBudget = func(string) int64 { return 0 }
	if got := pullCap(500, "/x"); got != 1 {
		t.Errorf("a full disk gave cap %d; 0 would mean unbounded", got)
	}
}

// The endpoint key is written by the source node.
func TestPullableEndpoint(t *testing.T) {
	for ep, want := range map[string]bool{
		"10.8.0.5:25522":          true,
		"203.0.113.9:25522":       true,
		"127.0.0.1:25522":         false,
		"169.254.169.254:80":      false,
		"0.0.0.0:25522":           false,
		"[::1]:25522":             false,
		"metadata.internal:80":    false,
		"10.8.0.5":                false,
		"10.8.0.5:25522/../admin": false,
		"10.8.0.5:+80":            false,
		"10.8.0.5:025522":         false,
		"10.8.0.5:0":              false,
	} {
		if got := pullableEndpoint(ep); got != want {
			t.Errorf("pullableEndpoint(%q) = %v, want %v", ep, got, want)
		}
	}
}

// A LAN candidate answering with a redirect is not a reachable source, and the
// target must not follow it anywhere.
func TestMigrationProbeFollowsNoRedirect(t *testing.T) {
	var reached atomic.Int32
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached.Add(1) }))
	defer inner.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inner.URL, http.StatusFound)
	}))
	defer src.Close()

	if got := chooseMigrationHost(context.Background(), []string{strings.TrimPrefix(src.URL, "http://")}, "tok"); got != "" || reached.Load() != 0 {
		t.Fatalf("chose %q, redirect target reached %d times", got, reached.Load())
	}
}

// handleMigrateIn must ask before it touches storage or the network.
func TestMigrateInRefusesAnEndpointItMustNotFetch(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mr.Set("dylaris:migration:endpoint:src", "127.0.0.1:25522")
	root := t.TempDir()
	progress := queue.MigrationProgressID("srv-ep", "a1")
	handleMigrateIn(context.Background(), rdb, &StorageManager{paths: []string{root}}, "tok", "srv-ep", progress, "src", "token", "sha", 10, nil)
	got, _ := rdb.Get(context.Background(), queue.MigrationStatusKey("tok", progress)).Result()
	if !strings.Contains(got, "not a usable address") {
		t.Fatalf("status %q, want the loopback endpoint refused", got)
	}
	if _, err := os.Stat(filepath.Join(root, "srv-ep")); !os.IsNotExist(err) {
		t.Error("storage was allocated for a refused endpoint")
	}
}
