package store

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestManifestEntryBatches(t *testing.T) {
	mk := func(n int) []models.StorageManifestEntry {
		out := make([]models.StorageManifestEntry, n)
		for i := range out {
			out[i] = models.StorageManifestEntry{Key: "k", Size: 1, Checksum: "c"}
		}
		return out
	}
	cases := []struct {
		name      string
		entries   int
		size      int
		wantCount int
		wantLast  int
	}{
		{"empty", 0, 500, 0, 0},
		{"one", 1, 500, 1, 1},
		{"exactly one batch", 500, 500, 1, 500},
		{"one over", 501, 500, 2, 1},
		{"two full", 1000, 500, 2, 500},
		{"many", 2501, 500, 6, 1},
		{"size smaller than entries", 5, 2, 3, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := manifestEntryBatches(mk(c.entries), c.size)
			if len(got) != c.wantCount {
				t.Fatalf("batches = %d, want %d", len(got), c.wantCount)
			}
			if c.wantCount == 0 {
				return
			}
			if len(got[len(got)-1]) != c.wantLast {
				t.Errorf("last batch size = %d, want %d", len(got[len(got)-1]), c.wantLast)
			}
			total := 0
			for _, b := range got {
				total += len(b)
			}
			if total != c.entries {
				t.Errorf("batched %d entries, want %d (nothing may be dropped)", total, c.entries)
			}
		})
	}
}

func TestBuildManifestEntriesInsert(t *testing.T) {
	batch := []models.StorageManifestEntry{
		{Key: "a.txt", Size: 3, Checksum: "aa"},
		{Key: "b/c.txt", Size: 7, Checksum: "bb"},
	}
	q, args := buildManifestEntriesInsert(9, batch)

	if !strings.HasPrefix(q, "INSERT INTO storage_manifest_entries (manifest_id, key, size, checksum) VALUES ") {
		t.Fatalf("query prefix = %q", q)
	}
	if !strings.Contains(q, "($1,$2,$3,$4)") || !strings.Contains(q, "($5,$6,$7,$8)") {
		t.Errorf("query placeholders wrong: %q", q)
	}
	if !strings.Contains(q, "ON CONFLICT (manifest_id, key) DO NOTHING") {
		t.Errorf("query must be re-runnable: %q", q)
	}
	want := []interface{}{9, "a.txt", int64(3), "aa", 9, "b/c.txt", int64(7), "bb"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %v, want %v", i, args[i], want[i])
		}
	}
}

func TestBuildManifestEntriesInsert_EmptyBatch(t *testing.T) {
	q, args := buildManifestEntriesInsert(1, nil)
	if q != "" || args != nil {
		t.Fatalf("empty batch produced q=%q args=%v, want empty", q, args)
	}
}

func TestCreateStorageManifest_InsertsHeaderThenEntriesInOneTx(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	captured := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	m := &models.StorageManifest{
		DataSet:      "library",
		BackendLabel: "path:/mnt/shared/library",
		Algo:         "sha256",
		CapturedAt:   captured,
		ObjectCount:  2,
		TotalBytes:   10,
		CreatedBy:    "admin-uuid",
	}
	entries := []models.StorageManifestEntry{
		{Key: "a.txt", Size: 3, Checksum: "aa"},
		{Key: "b.txt", Size: 7, Checksum: "bb"},
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO storage_manifests
		(data_set, backend_label, algo, captured_at, object_count, total_bytes, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`)).
		WithArgs("library", "path:/mnt/shared/library", "sha256", captured, int64(2), int64(10), "admin-uuid").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(4))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO storage_manifest_entries")).
		WithArgs(4, "a.txt", int64(3), "aa", 4, "b.txt", int64(7), "bb").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	id, err := s.CreateStorageManifest(m, entries)
	if err != nil {
		t.Fatalf("CreateStorageManifest: %v", err)
	}
	if id != 4 {
		t.Errorf("id = %d, want 4", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestCreateStorageManifest_RollsBackOnEntryFailure(t *testing.T) {
	// A half-written manifest is worse than none: verification would then
	// report every un-inserted key as "extra" on a perfectly good target.
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO storage_manifests")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(4))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO storage_manifest_entries")).
		WillReturnError(errBoomStore)
	mock.ExpectRollback()

	_, err = s.CreateStorageManifest(
		&models.StorageManifest{DataSet: "library", Algo: "sha256"},
		[]models.StorageManifestEntry{{Key: "a", Size: 1, Checksum: "c"}},
	)
	if err == nil {
		t.Fatal("CreateStorageManifest err = nil, want the entry-insert failure")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestListStorageManifests_FiltersByDataSetWhenGiven(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	rows := sqlmock.NewRows([]string{"id", "data_set", "backend_label", "algo", "captured_at", "object_count", "total_bytes", "created_by"}).
		AddRow(2, "library", "s3:bucket/library", "sha256", time.Now(), int64(5), int64(50), "admin")
	mock.ExpectQuery(regexp.QuoteMeta("WHERE data_set = $1")).
		WithArgs("library", 25).
		WillReturnRows(rows)

	got, err := s.ListStorageManifests("library", 25)
	if err != nil {
		t.Fatalf("ListStorageManifests: %v", err)
	}
	if len(got) != 1 || got[0].ID != 2 || got[0].DataSet != "library" {
		t.Fatalf("ListStorageManifests = %+v, want the one library manifest", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestListModpackStorageKeys_UnionsTheColumnsAndSkipsBlanks(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	rows := sqlmock.NewRows([]string{"storage_key"}).
		AddRow("modpacks/a.mrpack").
		AddRow("modpacks/b.jar").
		AddRow("modpacks/c-client.mrpack").
		AddRow("").
		AddRow("   ")
	mock.ExpectQuery(regexp.QuoteMeta("FROM modversions")).WillReturnRows(rows)

	got, err := s.ListModpackStorageKeys()
	if err != nil {
		t.Fatalf("ListModpackStorageKeys: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("keys = %v, want 3", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestListModpackStorageKeys_QueryCoversBothSources(t *testing.T) {
	q := modpackStorageKeysSQL()
	for _, want := range []string{
		"modversions",
		"pack_builds",
		"storage_key <> ''",
		"mrpack_storage_key <> ''",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("modpack key-space query is missing %q:\n%s", want, q)
		}
	}
	// The Solder loader table is dropped at boot. A query that still named it
	// would fail the whole storage migration of the modpacks data set.
	if strings.Contains(q, "loaders") {
		t.Errorf("modpack key-space query still reads the dropped loaders table:\n%s", q)
	}
}

func TestListModversionSHA512ByStorageKey_SkipsBlanks(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	rows := sqlmock.NewRows([]string{"storage_key", "sha512"}).
		AddRow("modpacks/a.jar", "deadbeef").
		AddRow("modpacks/b.jar", "")
	mock.ExpectQuery(regexp.QuoteMeta("FROM modversions")).WillReturnRows(rows)

	got, err := s.ListModversionSHA512ByStorageKey()
	if err != nil {
		t.Fatalf("ListModversionSHA512ByStorageKey: %v", err)
	}
	if len(got) != 1 || got["modpacks/a.jar"] != "deadbeef" {
		t.Fatalf("map = %v, want only the non-blank sha512", got)
	}
	if _, present := got["modpacks/b.jar"]; present {
		t.Error("a blank sha512 was included; it carries no integrity signal")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// errBoomStore is a local sentinel for the rollback test.
var errBoomStore = errBoomStoreType{}

type errBoomStoreType struct{}

func (errBoomStoreType) Error() string { return "boom" }
