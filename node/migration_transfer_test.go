package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dylaris-pkg/migration"
	"dylaris-pkg/queue"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// waitForProgress polls until the node reports phase with some bytes behind it.
func waitForProgress(rdb *redis.Client, progress, phase string) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st := readMigrationStatusMaybe(rdb, progress); st.Phase == phase && st.Bytes > 0 {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

func fastMigrationProgress(t *testing.T) {
	prev := stagingProgressInterval
	stagingProgressInterval = 2 * time.Millisecond
	t.Cleanup(func() { stagingProgressInterval = prev })
}

// movableZip is an archive of a small server with its hash and size.
func movableZip(t *testing.T) (body []byte, sum string) {
	t.Helper()
	body, err := os.ReadFile(writeMoveZip(t, map[string]string{"survival/a": strings.Repeat("a", 4000), "survival/b": "bb"}))
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(body)
	return body, hex.EncodeToString(h[:])
}

// H57/H60: the R2 legs and the LAN pull said nothing between start and the
// final phase, so Core's fixed wait failed every transfer longer than it. Each
// leg now reports its bytes while it moves them; the server here holds the
// transfer open until that report is seen.
func TestMigrationTransfersReportProgressWhileTheyRun(t *testing.T) {
	fastMigrationProgress(t)
	zipBody, sum := movableZip(t)
	tests := []struct {
		name      string
		phase     string
		wantFinal string
		run       func(ctx context.Context, rdb *redis.Client, url, progress string)
	}{
		{"push in one PUT", "pushing", "pushed", func(ctx context.Context, rdb *redis.Client, url, progress string) {
			root := stageArchive(t, string(zipBody))
			handleMigratePushR2(ctx, rdb, &StorageManager{paths: []string{root}}, "tok", stagingTestUUID, progress, url, nil, 0)
		}},
		{"push in parts", "pushing", "pushed", func(ctx context.Context, rdb *redis.Client, url, progress string) {
			root := stageArchive(t, string(zipBody))
			size := int64(len(zipBody))
			handleMigratePushR2(ctx, rdb, &StorageManager{paths: []string{root}}, "tok", stagingTestUUID, progress, "", []string{url + "/p1", url + "/p2"}, (size+1)/2)
		}},
		{"pull from transfer storage", "transferring", "transferred", func(ctx context.Context, rdb *redis.Client, url, progress string) {
			handleMigratePullR2(ctx, rdb, &StorageManager{paths: []string{t.TempDir()}}, "tok", stagingTestUUID, progress, url, sum, int64(len(zipBody)))
		}},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			progress := queue.MigrationProgressID(stagingTestUUID, string(rune('a'+i)))
			seen := make(chan bool, 4)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					io.ReadFull(r.Body, make([]byte, 1))
					seen <- waitForProgress(rdb, progress, tc.phase)
					io.Copy(io.Discard, r.Body)
					return
				}
				half := len(zipBody) / 2
				w.Write(zipBody[:half])
				w.(http.Flusher).Flush()
				seen <- waitForProgress(rdb, progress, tc.phase)
				w.Write(zipBody[half:])
			}))
			defer srv.Close()

			tc.run(context.Background(), rdb, srv.URL, progress)

			if st := readMigrationStatus(t, rdb, progress); st.Phase != tc.wantFinal {
				t.Fatalf("final phase = %q (%s), want %q", st.Phase, st.Error, tc.wantFinal)
			}
			if !<-seen {
				t.Fatalf("no %q progress was reported while the transfer ran", tc.phase)
			}
		})
	}
}

// H58/H61: the target's pull and unpack measured only the free space, not what
// the restores, unpacks and installs running here had already claimed, and
// claimed nothing themselves - two large writes spent the same space and ran
// the shared storage path into ENOSPC.
func TestATargetMoveSharesTheDiskWithTheOtherWrites(t *testing.T) {
	zipBody, sum := movableZip(t)
	size := int64(len(zipBody))
	const budget = 1 << 20
	prev := restoreDiskBudget
	restoreDiskBudget = func(string) int64 { return budget }
	t.Cleanup(func() { restoreDiskBudget = prev; inflightWrites.Store(0) })

	tests := []struct {
		name      string
		other     int64 // claimed by the other writes on this node
		wantPhase string
	}{
		{"room beside the other writes", 1000, "transferred"},
		{"the other writes left too little", budget - size + 1, "error"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inflightWrites.Store(tc.other)
			heldDuringPull := make(chan int64, 4)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				heldDuringPull <- inflightWrites.Load()
				w.Write(zipBody)
			}))
			defer srv.Close()
			rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			progress := queue.MigrationProgressID(stagingTestUUID, string(rune('a'+i)))

			handleMigratePullR2(context.Background(), rdb, &StorageManager{paths: []string{t.TempDir()}}, "tok", stagingTestUUID, progress, srv.URL, sum, size)

			if st := readMigrationStatus(t, rdb, progress); st.Phase != tc.wantPhase {
				t.Fatalf("phase = %q (%s), want %q", st.Phase, st.Error, tc.wantPhase)
			}
			if got := inflightWrites.Load(); got != tc.other {
				t.Fatalf("inflightWrites = %d after the move, want the other writes' %d back", got, tc.other)
			}
			if tc.wantPhase == "transferred" {
				if held := <-heldDuringPull; held < tc.other+size {
					t.Fatalf("inflightWrites = %d during the pull, want at least %d", held, tc.other+size)
				}
			}
		})
	}

	// The unpack's budget leaves out what the other writes claimed.
	restoreDiskBudget = func(string) int64 { return 8 }
	inflightWrites.Store(4)
	storage := t.TempDir()
	zipPath := writeMoveZip(t, map[string]string{"survival/a": "1234", "survival/b": "5678"})
	if err := extractMovedServer(zipPath, storage, filepath.Join(storage, "srv"), nil); !errors.Is(err, migration.ErrExtractBudget) {
		t.Fatalf("err = %v, want the budget refusal", err)
	}
}

// H59: a migrate_out that ran after Core had aborted the attempt staged a
// copy nothing removed, and for hours the reconciler left the server Core had
// started again alone. An abort that lands while the archive is written now
// drops it, and one queued too long ago is not run at all.
func TestAMigrateOutAbortedWhileStagingLeavesNoArchive(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, stagingTestUUID), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, stagingTestUUID, "level.dat"), []byte("w"), 0o644)
	storage := &StorageManager{paths: []string{root}}
	prev := migrateOutArchive
	migrateOutArchive = func(src, dst string) (string, int64, error) {
		handleMigrateAbort(storage, stagingTestUUID) // finds nothing yet
		return migration.Archive(src, dst)
	}
	t.Cleanup(func() { migrateOutArchive = prev })
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	progress := queue.MigrationProgressID(stagingTestUUID, "a")

	handleMigrateOut(context.Background(), rdb, storage, "tok", stagingTestUUID, progress)

	if st := readMigrationStatusMaybe(rdb, progress); st.Phase == "staged" {
		t.Fatal("an aborted attempt reported staged")
	}
	for _, p := range []string{stagedArchivePath(root, stagingTestUUID), migrationOriginPath(root, stagingTestUUID)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind by an aborted attempt: %v", p, err)
		}
	}
}

// J52/J57: a cancelled attempt's migrate_out still archiving when the retry
// staged removed the archive and origin stamp by path once it finished - the
// retry's, since both were named by the server alone - and the retry's target
// pull found nothing. Each attempt now writes its own file and removes only
// that one.
func TestALateAbortedMigrateOutLeavesTheRetrysArchive(t *testing.T) {
	tests := []struct {
		name   string
		failsA bool
	}{
		{"the earlier attempt finishes after the retry", false},
		{"the earlier attempt fails after the retry", true},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, stagingTestUUID), 0o755); err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(root, stagingTestUUID, "level.dat"), []byte("w"), 0o644)
			storage := &StorageManager{paths: []string{root}}
			rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			attemptA := queue.MigrationProgressID(stagingTestUUID, string(rune('a'+2*i)))
			attemptB := queue.MigrationProgressID(stagingTestUUID, string(rune('b'+2*i)))
			prev := migrateOutArchive
			nested := false
			migrateOutArchive = func(src, dst string) (string, int64, error) {
				if nested {
					return migration.Archive(src, dst)
				}
				nested = true
				// A is still archiving: Core aborts it, and the retry runs
				// to "staged" before A is done.
				os.WriteFile(dst, []byte("half"), 0o644)
				handleMigrateAbort(storage, stagingTestUUID)
				handleMigrateOut(context.Background(), rdb, storage, "tok", stagingTestUUID, attemptB)
				if tc.failsA {
					return "", 0, errors.New("disk error")
				}
				return migration.Archive(src, dst)
			}
			t.Cleanup(func() { migrateOutArchive = prev })

			handleMigrateOut(context.Background(), rdb, storage, "tok", stagingTestUUID, attemptA)

			if st := readMigrationStatusMaybe(rdb, attemptB); st.Phase != "staged" {
				t.Fatalf("retry status = %+v, want staged", st)
			}
			for _, p := range []string{stagedArchivePath(root, stagingTestUUID), migrationOriginPath(root, stagingTestUUID)} {
				if _, err := os.Stat(p); err != nil {
					t.Errorf("the retry's %s was removed by the earlier attempt: %v", p, err)
				}
			}
			entries, _ := os.ReadDir(filepath.Join(root, migrationStagingDir))
			if len(entries) != 2 {
				names := []string{}
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("staging dir holds %v, want only the retry's archive and stamp", names)
			}
		})
	}
}

// J55: a migrate_out redelivered after a node restart, queued too long ago to
// run, was dropped without a word, and Core waited out its stage deadline.
func TestADroppedStaleMigrateOutReportsAnError(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	progress := queue.MigrationProgressID(stagingTestUUID, "stale")
	if !dropStaleMigrateOut(context.Background(), rdb, "tok", progress, now.Add(-time.Hour).Unix(), now) {
		t.Fatal("a stale migrate_out was not dropped")
	}
	if st := readMigrationStatusMaybe(rdb, progress); st.Phase != "error" {
		t.Fatalf("status = %+v, want error", st)
	}
	fresh := queue.MigrationProgressID(stagingTestUUID, "fresh")
	if dropStaleMigrateOut(context.Background(), rdb, "tok", fresh, now.Unix(), now) {
		t.Fatal("a fresh migrate_out was dropped")
	}
	if st := readMigrationStatusMaybe(rdb, fresh); st.Phase != "" {
		t.Fatalf("a fresh migrate_out wrote status %+v", st)
	}
}

// J53: Core's waits grow with progress up to 6h (migrationProgressMaxWait), so
// a discard that gave up after 2h kept the copy of a pull that ran on.
func TestMigrateDiscardOutwaitsTheLongestPull(t *testing.T) {
	const coreProgressMaxWait = 6 * time.Hour
	if migrateDiscardWait <= coreProgressMaxWait {
		t.Fatalf("migrateDiscardWait = %s, a pull may run %s", migrateDiscardWait, coreProgressMaxWait)
	}
}

func TestStaleMigrateOut(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tests := []struct {
		name     string
		issuedAt int64
		want     bool
	}{
		{"Core that stamps none", 0, false},
		{"fresh", now.Add(-time.Minute).Unix(), false},
		{"at the stage wait", now.Add(-migrateOutMaxAge).Unix(), false},
		{"past the stage wait", now.Add(-migrateOutMaxAge - time.Second).Unix(), true},
	}
	for _, tc := range tests {
		if got := staleMigrateOut(tc.issuedAt, now); got != tc.want {
			t.Errorf("%s: staleMigrateOut = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// P22: the abort count lives in memory, so an abort that ran before a node
// restart was forgotten and the redelivered migrate_out of that attempt staged
// a full archive nothing removed. The abort is recorded on disk against the
// attempt that was staging; a newer attempt still stages.
func TestAnAbortBeforeARestartStillStopsTheRedeliveredMigrateOut(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, stagingTestUUID), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, stagingTestUUID, "level.dat"), []byte("w"), 0o644)
	storage := &StorageManager{paths: []string{root}}
	abortNow := true
	prev := migrateOutArchive
	migrateOutArchive = func(src, dst string) (string, int64, error) {
		if abortNow {
			abortNow = false
			handleMigrateAbort(storage, stagingTestUUID)
		}
		return migration.Archive(src, dst)
	}
	t.Cleanup(func() { migrateOutArchive = prev; migrateAborts.Delete(stagingTestUUID) })
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	attemptA := queue.MigrationProgressID(stagingTestUUID, "restart-a")
	attemptB := queue.MigrationProgressID(stagingTestUUID, "restart-b")

	handleMigrateOut(context.Background(), rdb, storage, "tok", stagingTestUUID, attemptA)
	migrateAborts.Delete(stagingTestUUID) // the node restarts
	handleMigrateOut(context.Background(), rdb, storage, "tok", stagingTestUUID, attemptA)

	if st := readMigrationStatusMaybe(rdb, attemptA); st.Phase == "staged" {
		t.Fatal("the redelivered migrate_out of an aborted attempt reported staged")
	}
	if _, err := os.Stat(stagedArchivePath(root, stagingTestUUID)); !os.IsNotExist(err) {
		t.Fatalf("the aborted attempt left an archive: %v", err)
	}

	handleMigrateOut(context.Background(), rdb, storage, "tok", stagingTestUUID, attemptB)
	if st := readMigrationStatusMaybe(rdb, attemptB); st.Phase != "staged" {
		t.Fatalf("the next attempt: %+v, want staged", st)
	}
}
