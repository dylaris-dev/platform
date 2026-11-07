package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"dylaris-pkg/queue"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// R17: a migrate_in / migrate_pull_r2 redelivered after the target already
// reported "transferred" (a restart before the ack) pulled the archive again
// and reset the directory the cut-over server was already running on.
func TestARedeliveredLandedMoveDoesNotPullAgain(t *testing.T) {
	zipBody, sum := movableZip(t)
	tests := []struct {
		name string
		run  func(rdb *redis.Client, storage *StorageManager, url, progress string)
	}{
		{"pull from transfer storage", func(rdb *redis.Client, storage *StorageManager, url, progress string) {
			handleMigratePullR2(context.Background(), rdb, storage, "tok", stagingTestUUID, progress, url, sum, int64(len(zipBody)))
		}},
		// No endpoint published: the redelivery must answer before it looks.
		{"pull from the source node", func(rdb *redis.Client, storage *StorageManager, _, progress string) {
			handleMigrateIn(context.Background(), rdb, storage, "tok", stagingTestUUID, progress, "src", "token", sum, int64(len(zipBody)), nil)
		}},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.Write(zipBody)
			}))
			defer srv.Close()
			rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			root := t.TempDir()
			storage := &StorageManager{paths: []string{root}}
			progress := queue.MigrationProgressID(stagingTestUUID, string(rune('a'+i)))

			// The first delivery lands for real, over transfer storage.
			handleMigratePullR2(context.Background(), rdb, storage, "tok", stagingTestUUID, progress, srv.URL, sum, int64(len(zipBody)))
			if st := readMigrationStatus(t, rdb, progress); st.Phase != "transferred" {
				t.Fatalf("first delivery phase = %q (%s)", st.Phase, st.Error)
			}
			// The server runs here now and writes.
			live := filepath.Join(root, stagingTestUUID, "survival", "since-cutover")
			if err := os.WriteFile(live, []byte("new"), 0o644); err != nil {
				t.Fatal(err)
			}
			hits.Store(0)
			rdb.Del(context.Background(), queue.MigrationStatusKey("tok", progress))

			tc.run(rdb, storage, srv.URL, progress)

			if hits.Load() != 0 {
				t.Fatal("the redelivered attempt downloaded again")
			}
			if _, err := os.Stat(live); err != nil {
				t.Fatalf("data written since cutover is gone: %v", err)
			}
			if st := readMigrationStatus(t, rdb, progress); st.Phase != "transferred" {
				t.Fatalf("redelivery phase = %q (%s), want transferred", st.Phase, st.Error)
			}
		})
	}
}
