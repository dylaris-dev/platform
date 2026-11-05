package main

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-pkg/queue"
)

// The R2 push and pull used to put the raw net/http error into the node log
// and the move status. Its text carries the full pre-signed URL, and the move
// status is served to the server's owner and sub-users.
func TestR2TransferFailureLeaksNoSignedURL(t *testing.T) {
	srv := httptest.NewServer(nil)
	signed := srv.URL + "/b/k.zip?X-Amz-Credential=AKIA&X-Amz-Signature=deadbeef"
	srv.Close() // nothing listens: the request fails at dial with a *url.Error

	const uuid = "11111111-2222-3333-4444-555555555555"
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, migrationStagingDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagedArchivePath(root, uuid), []byte("zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	storage := &StorageManager{paths: []string{root}}

	leaks := func(s string) bool { return strings.Contains(s, "X-Amz") || strings.Contains(s, "deadbeef") }

	t.Run("putFilePresigned", func(t *testing.T) {
		err := putFilePresigned(ctx, signed, stagedArchivePath(root, uuid), io.Discard)
		if err == nil {
			t.Fatal("put to a closed port succeeded")
		}
		if leaks(err.Error()) {
			t.Fatalf("error carries the signed query: %s", err)
		}
	})
	cases := []struct {
		name string
		run  func(progress string)
	}{
		{"push status", func(p string) { handleMigratePushR2(ctx, rdb, storage, "tok", uuid, p, signed, nil, 0) }},
		{"pull status", func(p string) {
			handleMigratePullR2(ctx, rdb, &StorageManager{paths: []string{t.TempDir()}}, "tok", uuid, p, signed, "00", 0)
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			progress := queue.MigrationProgressID(uuid, string(rune('a'+i)))
			c.run(progress)
			got, err := rdb.Get(ctx, queue.MigrationStatusKey("tok", progress)).Result()
			if err != nil {
				t.Fatalf("no status written: %v", err)
			}
			if !strings.Contains(got, `"error"`) || leaks(got) {
				t.Fatalf("status %s: want an error that carries no signed URL", got)
			}
		})
	}
}
