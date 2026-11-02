package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"

	"github.com/DATA-DOG/go-sqlmock"
)

// keyedBackupStore returns a store with the backup-storage encryption key
// installed, over the given db (nil for the DB-less crypto/helper tests).
func keyedBackupStore(db *sql.DB) *PostgresStore {
	s := NewPostgresStore(db)
	s.SetBackupStorageEncryptionKey("a-strong-cluster-secret")
	return s
}

// --- crypto (DB-less) ---

func TestBackupStorageSecret_RoundTrips(t *testing.T) {
	s := keyedBackupStore(nil)
	const secret = "wJalrXUtnFEMI-backup-secret"

	enc, err := s.encodeBackupStorageSecret(secret)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if enc == "" || enc == secret || strings.Contains(enc, secret) {
		t.Fatalf("encoded value is empty or leaks the plaintext: %q", enc)
	}
	if got := s.decodeBackupStorageSecret(enc); got != secret {
		t.Fatalf("decode = %q, want the original secret", got)
	}
}

func TestBackupStorageSecret_EmptyStaysEmpty(t *testing.T) {
	s := keyedBackupStore(nil)
	enc, err := s.encodeBackupStorageSecret("")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if enc != "" {
		t.Fatalf("an empty secret encoded to %q, want empty", enc)
	}
	if got := s.decodeBackupStorageSecret(""); got != "" {
		t.Fatalf("decode of an empty column = %q, want empty", got)
	}
}

func TestBackupStorageSecret_NoKeyFailsClosed(t *testing.T) {
	s := NewPostgresStore(nil) // no key installed
	if _, err := s.encodeBackupStorageSecret("plaintext"); !errors.Is(err, errNoBackupStorageKey) {
		t.Fatalf("encode without a key err = %v, want errNoBackupStorageKey", err)
	}
	// An empty secret is still fine without a key (non-s3 providers).
	if enc, err := s.encodeBackupStorageSecret(""); err != nil || enc != "" {
		t.Fatalf("encode of empty without a key = (%q, %v), want (\"\", nil)", enc, err)
	}
}

func TestBackupStorageSecret_UndecryptableReturnsEmpty(t *testing.T) {
	enc, err := keyedBackupStore(nil).encodeBackupStorageSecret("secret")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := NewPostgresStore(nil).decodeBackupStorageSecret(enc); got != "" {
		t.Fatalf("decode with no key = %q, want empty", got)
	}
	wrong := NewPostgresStore(nil)
	wrong.SetBackupStorageEncryptionKey("a-different-cluster-secret")
	if got := wrong.decodeBackupStorageSecret(enc); got != "" {
		t.Fatalf("decode with the wrong key = %q, want empty", got)
	}
}

// --- config helpers (pure) ---

func TestSplitBackupStorageSecret(t *testing.T) {
	cfg, secret := splitBackupStorageSecret("s3", json.RawMessage(`{"bucket":"b","secretAccessKey":"sk"}`))
	if secret != "sk" {
		t.Fatalf("secret = %q, want sk", secret)
	}
	if strings.Contains(string(cfg), "secretAccessKey") || strings.Contains(string(cfg), "sk") {
		t.Fatalf("clean config still carries the secret: %s", cfg)
	}
	// Non-s3 is left untouched (no secret to hoist).
	orig := json.RawMessage(`{"basePath":"/data"}`)
	cfg, secret = splitBackupStorageSecret("local", orig)
	if secret != "" || string(cfg) != string(orig) {
		t.Fatalf("non-s3 split = (%s, %q), want unchanged config and empty secret", cfg, secret)
	}
	// s3 without a secret field is unchanged.
	cfg, secret = splitBackupStorageSecret("s3", json.RawMessage(`{"bucket":"b"}`))
	if secret != "" || string(cfg) != `{"bucket":"b"}` {
		t.Fatalf("s3-no-secret split = (%s, %q), want unchanged", cfg, secret)
	}
}

func TestInjectBackupStorageSecret(t *testing.T) {
	out := injectBackupStorageSecret("s3", json.RawMessage(`{"bucket":"b"}`), "sk")
	var m map[string]string
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["secretAccessKey"] != "sk" || m["bucket"] != "b" {
		t.Fatalf("inject result = %s, want bucket+secret", out)
	}
	// Non-s3 or empty secret: unchanged.
	if got := injectBackupStorageSecret("local", json.RawMessage(`{"basePath":"/x"}`), "sk"); string(got) != `{"basePath":"/x"}` {
		t.Fatalf("non-s3 inject changed config: %s", got)
	}
	if got := injectBackupStorageSecret("s3", json.RawMessage(`{"bucket":"b"}`), ""); string(got) != `{"bucket":"b"}` {
		t.Fatalf("empty-secret inject changed config: %s", got)
	}
}

func TestStripBackupStorageSecret(t *testing.T) {
	cfg, had := stripBackupStorageSecret("s3", json.RawMessage(`{"bucket":"b","secretAccessKey":"sk"}`))
	if !had {
		t.Fatal("had = false, want true (a secret was present)")
	}
	if strings.Contains(string(cfg), "secretAccessKey") || strings.Contains(string(cfg), "sk") {
		t.Fatalf("stripped config still carries the secret: %s", cfg)
	}
	if cfg, had := stripBackupStorageSecret("s3", json.RawMessage(`{"bucket":"b"}`)); had || string(cfg) != `{"bucket":"b"}` {
		t.Fatalf("s3-no-secret strip = (%s, %v), want unchanged/false", cfg, had)
	}
	if cfg, had := stripBackupStorageSecret("local", json.RawMessage(`{"basePath":"/x"}`)); had || string(cfg) != `{"basePath":"/x"}` {
		t.Fatalf("non-s3 strip = (%s, %v), want unchanged/false", cfg, had)
	}
}

// --- CRUD (sqlmock) ---

func TestCreateBackupStorage_EncryptsSecretAndStripsConfig(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := keyedBackupStore(db)

	// The config the store persists must NOT contain the secret.
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO backup_storages (name, provider, config, secret_enc, is_default, owner_id) VALUES ($1, $2, $3::jsonb, $4, $5, $6) RETURNING id`)).
		WithArgs("nas", "s3", `{"bucket":"b"}`, sqlmock.AnyArg(), false, nil).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(7))
	mock.ExpectCommit()

	id, err := s.CreateBackupStorage(&models.BackupStorage{
		Name: "nas", Provider: "s3",
		Config: json.RawMessage(`{"bucket":"b","secretAccessKey":"the-secret"}`),
	})
	if err != nil {
		t.Fatalf("CreateBackupStorage: %v", err)
	}
	if id != 7 {
		t.Fatalf("id = %d, want 7", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestCreateBackupStorage_NoKeyFailsClosedBeforeDB(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db) // no key

	// No ExpectQuery: an s3 secret must never reach the DB in the clear.
	_, err = s.CreateBackupStorage(&models.BackupStorage{
		Name: "nas", Provider: "s3",
		Config: json.RawMessage(`{"bucket":"b","secretAccessKey":"plaintext"}`),
	})
	if !errors.Is(err, errNoBackupStorageKey) {
		t.Fatalf("CreateBackupStorage err = %v, want errNoBackupStorageKey", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a query ran despite the fail-closed guard: %v", err)
	}
}

func TestCreateBackupStorage_NonS3NeedsNoKey(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db) // no key, but a non-s3 provider has no secret

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO backup_storages (name, provider, config, secret_enc, is_default, owner_id) VALUES ($1, $2, $3::jsonb, $4, $5, $6) RETURNING id`)).
		WithArgs("disk", "local", `{"basePath":"/data"}`, "", false, nil).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3))
	mock.ExpectCommit()

	id, err := s.CreateBackupStorage(&models.BackupStorage{
		Name: "disk", Provider: "local", Config: json.RawMessage(`{"basePath":"/data"}`),
	})
	if err != nil || id != 3 {
		t.Fatalf("CreateBackupStorage = (%d, %v), want (3, nil)", id, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestGetBackupStorage_DecryptsAndInjectsSecret(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := keyedBackupStore(db)

	enc, err := s.encodeBackupStorageSecret("the-secret")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta("FROM backup_storages WHERE id = $1")).
		WithArgs(3).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "provider", "config", "secret_enc", "is_default", "created_at", "owner_id"}).
			AddRow(3, "nas", "s3", []byte(`{"bucket":"b"}`), enc, false, now, nil))

	got, err := s.GetBackupStorage(3)
	if err != nil {
		t.Fatalf("GetBackupStorage: %v", err)
	}
	if !got.SecretSet {
		t.Error("SecretSet = false, want true")
	}
	var m map[string]string
	if err := json.Unmarshal(got.Config, &m); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if m["secretAccessKey"] != "the-secret" {
		t.Errorf("provider-build config missing the decrypted secret: %s", got.Config)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// A legacy row (secret_enc empty, secret still plaintext in config) must still
// yield the secret to a provider build - the read-then-rewrite migration only
// completes on the next save, and backups must not break in the meantime.
func TestGetBackupStorage_LegacyPlaintextInConfig(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := keyedBackupStore(db)

	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta("FROM backup_storages WHERE id = $1")).
		WithArgs(4).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "provider", "config", "secret_enc", "is_default", "created_at", "owner_id"}).
			AddRow(4, "legacy", "s3", []byte(`{"bucket":"b","secretAccessKey":"legacy-plain"}`), "", false, now, nil))

	got, err := s.GetBackupStorage(4)
	if err != nil {
		t.Fatalf("GetBackupStorage: %v", err)
	}
	if !got.SecretSet {
		t.Error("SecretSet = false, want true (legacy plaintext secret present)")
	}
	var m map[string]string
	if err := json.Unmarshal(got.Config, &m); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if m["secretAccessKey"] != "legacy-plain" {
		t.Errorf("legacy secret not surfaced for build: %s", got.Config)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// The list path must strip the secret (encrypted OR legacy-plaintext) and only
// report SecretSet, so a settings.read holder can never harvest a credential.
func TestListBackupStorages_StripsSecretAndReportsSet(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := keyedBackupStore(db)

	enc, err := s.encodeBackupStorageSecret("enc-secret")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta("FROM backup_storages WHERE owner_id IS NULL ORDER BY id")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "provider", "config", "secret_enc", "is_default", "created_at", "owner_id"}).
			AddRow(1, "encrypted", "s3", []byte(`{"bucket":"b"}`), enc, false, now, nil).
			AddRow(2, "legacy", "s3", []byte(`{"bucket":"b","secretAccessKey":"legacy-plain"}`), "", false, now, nil).
			AddRow(3, "disk", "local", []byte(`{"basePath":"/x"}`), "", false, now, nil))

	got, err := s.ListBackupStorages()
	if err != nil {
		t.Fatalf("ListBackupStorages: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	wantSet := []bool{true, true, false}
	for i, bs := range got {
		if bs.SecretSet != wantSet[i] {
			t.Errorf("row %d SecretSet = %v, want %v", i, bs.SecretSet, wantSet[i])
		}
		if strings.Contains(string(bs.Config), "secretAccessKey") || strings.Contains(string(bs.Config), "legacy-plain") {
			t.Errorf("row %d list config leaked the secret: %s", i, bs.Config)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestUpdateBackupStorage_EncryptsSecretAndStripsConfig(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := keyedBackupStore(db)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE backup_storages SET name = $1, provider = $2, config = $3::jsonb, secret_enc = $4, is_default = $5 WHERE id = $6`)).
		WithArgs("nas", "s3", `{"bucket":"b"}`, sqlmock.AnyArg(), false, 5).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = s.UpdateBackupStorage(&models.BackupStorage{
		ID: 5, Name: "nas", Provider: "s3",
		Config: json.RawMessage(`{"bucket":"b","secretAccessKey":"rotated"}`),
	})
	if err != nil {
		t.Fatalf("UpdateBackupStorage: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
