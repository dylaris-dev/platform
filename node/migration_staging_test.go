package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-pkg/queue"
)

const stagingTestUUID = "11111111-2222-3333-4444-555555555555"

func readMigrationStatus(t *testing.T, rdb *redis.Client, progress string) migrationStatus {
	t.Helper()
	raw, err := rdb.Get(context.Background(), queue.MigrationStatusKey("tok", progress)).Result()
	if err != nil {
		t.Fatalf("no status written: %v", err)
	}
	var st migrationStatus
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatalf("status %q: %v", raw, err)
	}
	return st
}

// stageArchive puts a staged archive with the given content where migrate_out
// leaves it.
func stageArchive(t *testing.T, content string) (root string) {
	t.Helper()
	root = t.TempDir()
	for _, d := range []string{stagingTestUUID, migrationStagingDir} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(stagedArchivePath(root, stagingTestUUID), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// G52: a staged archive over the single-PUT limit could not be moved through
// transfer storage at all. Core now signs a URL per part, and the node must cut
// the archive into exactly those parts.
func TestMigratePushR2Multipart(t *testing.T) {
	const content = "0123456789" // 10 bytes, parts of 4: "0123" "4567" "89"
	tests := []struct {
		name      string
		parts     int
		partSize  int64
		wantPhase string
		wantBody  map[string]string
	}{
		{"every part uploaded as signed", 3, 4, "pushed", map[string]string{"/p1": "0123", "/p2": "4567", "/p3": "89"}},
		{"too few URLs for the archive", 2, 4, "error", nil},
		{"too many URLs for the archive", 4, 4, "error", nil},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			got := map[string]string{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				got[r.URL.Path] = string(b)
				mu.Unlock()
			}))
			defer srv.Close()
			var urls []string
			for p := 1; p <= tc.parts; p++ {
				urls = append(urls, srv.URL+"/p"+string(rune('0'+p)))
			}
			root := stageArchive(t, content)
			rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			progress := queue.MigrationProgressID(stagingTestUUID, string(rune('a'+i)))

			handleMigratePushR2(context.Background(), rdb, &StorageManager{paths: []string{root}}, "tok", stagingTestUUID, progress, srv.URL+"/single", urls, tc.partSize)

			if st := readMigrationStatus(t, rdb, progress); st.Phase != tc.wantPhase {
				t.Fatalf("phase = %q (%s), want %q", st.Phase, st.Error, tc.wantPhase)
			}
			if tc.wantBody == nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if len(got) != len(tc.wantBody) {
				t.Fatalf("uploads = %v, want %v", got, tc.wantBody)
			}
			for path, body := range tc.wantBody {
				if got[path] != body {
					t.Errorf("%s got %q, want %q", path, got[path], body)
				}
			}
		})
	}
}

// G52: from an old Core, which signs only one PUT, an archive over the limit
// failed with the same "upload failed" a network error gives, so the operator
// could not tell a permanent limit from a retryable one.
func TestMigratePushR2OverSinglePutLimitSaysWhy(t *testing.T) {
	prev := presignedPutMaxSize
	presignedPutMaxSize = 4
	t.Cleanup(func() { presignedPutMaxSize = prev })

	root := stageArchive(t, "0123456789")
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	progress := queue.MigrationProgressID(stagingTestUUID, "a")

	handleMigratePushR2(context.Background(), rdb, &StorageManager{paths: []string{root}}, "tok", stagingTestUUID, progress, "http://127.0.0.1:1/never", nil, 0)

	st := readMigrationStatus(t, rdb, progress)
	if st.Phase != "error" || !strings.Contains(st.Error, "update Core") {
		t.Fatalf("status = %+v, want an error naming the single-upload limit", st)
	}
}

// G53: migrate_out wrote a full copy of the server onto the storage path every
// other server on it writes to, with no look at the free space.
func TestMigrateOutRefusesWithoutFreeSpace(t *testing.T) {
	tests := []struct {
		name      string
		budget    int64
		wantPhase string
	}{
		{"room for the copy", 1 << 30, "staged"},
		{"no room for the copy", 10, "error"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prev := restoreDiskBudget
			restoreDiskBudget = func(string) int64 { return tc.budget }
			t.Cleanup(func() { restoreDiskBudget = prev })

			root := t.TempDir()
			src := filepath.Join(root, stagingTestUUID)
			if err := os.MkdirAll(src, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(src, "world.mca"), make([]byte, 4096), 0o644); err != nil {
				t.Fatal(err)
			}
			rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			progress := queue.MigrationProgressID(stagingTestUUID, string(rune('a'+i)))

			handleMigrateOut(context.Background(), rdb, &StorageManager{paths: []string{root}}, "tok", stagingTestUUID, progress)

			st := readMigrationStatus(t, rdb, progress)
			if st.Phase != tc.wantPhase {
				t.Fatalf("phase = %q (%s), want %q", st.Phase, st.Error, tc.wantPhase)
			}
			_, err := os.Stat(stagedArchivePath(root, stagingTestUUID))
			if tc.wantPhase == "error" {
				if !strings.Contains(st.Error, "free space") {
					t.Errorf("error %q does not say why", st.Error)
				}
				if !os.IsNotExist(err) {
					t.Errorf("a refused move left a staged archive: %v", err)
				}
			}
		})
	}
}

// G54: Core's stage wait was a fixed 5 minutes and the node said nothing until
// the archive was done, so a large server failed every move. The node now
// reports how far the archive has grown while it writes it.
func TestStagingProgressReportsGrowthAndStops(t *testing.T) {
	prev := stagingProgressInterval
	stagingProgressInterval = 5 * time.Millisecond
	t.Cleanup(func() { stagingProgressInterval = prev })

	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	progress := queue.MigrationProgressID(stagingTestUUID, "a")
	zip := filepath.Join(t.TempDir(), "x.zip")
	if err := os.WriteFile(zip, make([]byte, 123), 0o644); err != nil {
		t.Fatal(err)
	}

	stop := reportStagingProgress(ctx, rdb, "tok", progress, zip)
	deadline := time.Now().Add(2 * time.Second)
	for {
		st := readMigrationStatusMaybe(rdb, progress)
		if st.Phase == "staging" && st.Bytes == 123 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no staging progress of 123 bytes reported, last %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()

	// Nothing may overwrite the final phase once stop returned.
	setMigrationStatus(ctx, rdb, "tok", progress, "staged", "")
	time.Sleep(30 * time.Millisecond)
	if st := readMigrationStatus(t, rdb, progress); st.Phase != "staged" {
		t.Fatalf("phase after stop = %q, want staged", st.Phase)
	}
}

func readMigrationStatusMaybe(rdb *redis.Client, progress string) migrationStatus {
	var st migrationStatus
	raw, _ := rdb.Get(context.Background(), queue.MigrationStatusKey("tok", progress)).Result()
	_ = json.Unmarshal([]byte(raw), &st)
	return st
}
