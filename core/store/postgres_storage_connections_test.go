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

// keyedConnStore returns a store with the storage-connection encryption key
// installed, over the given db (nil for the DB-less crypto tests).
func keyedConnStore(db *sql.DB) *PostgresStore {
	s := NewPostgresStore(db)
	s.SetStorageConnEncryptionKey("a-strong-cluster-secret")
	return s
}

// --- crypto (DB-less) ---

// TestStorageConnSecret_RoundTrips: a secret encrypts on the way in and decrypts
// on the way out, and the stored form never contains the plaintext.
func TestStorageConnSecret_RoundTrips(t *testing.T) {
	s := keyedConnStore(nil)
	const secret = "wJalrXUtnFEMI-super-secret"

	enc, err := s.encodeStorageConnSecret(secret)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if enc == "" || enc == secret {
		t.Fatalf("encoded value is empty or plaintext: %q", enc)
	}
	if strings.Contains(enc, secret) {
		t.Fatalf("encoded value contains the plaintext secret: %q", enc)
	}
	if got := s.decodeStorageConnSecret(enc); got != secret {
		t.Fatalf("decode = %q, want the original secret", got)
	}
}

// TestStorageConnSecret_EmptyStaysEmpty: no secret stores an empty column, not
// an encrypted blob, so SecretSet can key off emptiness.
func TestStorageConnSecret_EmptyStaysEmpty(t *testing.T) {
	s := keyedConnStore(nil)
	enc, err := s.encodeStorageConnSecret("")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if enc != "" {
		t.Fatalf("an empty secret encoded to %q, want empty", enc)
	}
	if got := s.decodeStorageConnSecret(""); got != "" {
		t.Fatalf("decode of an empty column = %q, want empty", got)
	}
}

// TestStorageConnSecret_NoKeyFailsClosed: unlike the settings secrets, this
// greenfield table must NEVER store a plaintext secret. With no key, an encode
// of a non-empty secret fails closed.
func TestStorageConnSecret_NoKeyFailsClosed(t *testing.T) {
	s := NewPostgresStore(nil) // no key installed
	if _, err := s.encodeStorageConnSecret("plaintext"); !errors.Is(err, errNoStorageConnKey) {
		t.Fatalf("encode without a key err = %v, want errNoStorageConnKey", err)
	}
	// An empty secret is still fine without a key (nothing to protect).
	if enc, err := s.encodeStorageConnSecret(""); err != nil || enc != "" {
		t.Fatalf("encode of empty without a key = (%q, %v), want (\"\", nil)", enc, err)
	}
}

// TestStorageConnSecret_UndecryptableReturnsEmpty: a missing or rotated key
// cannot read old ciphertext; decode fails to "" (never ciphertext-as-secret) so
// a provider build fails cleanly.
func TestStorageConnSecret_UndecryptableReturnsEmpty(t *testing.T) {
	enc, err := keyedConnStore(nil).encodeStorageConnSecret("secret")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	nokey := NewPostgresStore(nil)
	if got := nokey.decodeStorageConnSecret(enc); got != "" {
		t.Fatalf("decode with no key = %q, want empty", got)
	}
	wrong := NewPostgresStore(nil)
	wrong.SetStorageConnEncryptionKey("a-different-cluster-secret")
	if got := wrong.decodeStorageConnSecret(enc); got != "" {
		t.Fatalf("decode with the wrong key = %q, want empty", got)
	}
}

// --- CRUD (sqlmock) ---

func TestCreateStorageConnection_EncryptsSecretAndReturnsID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := keyedConnStore(db)

	cfg := []byte(`{"bucket":"b"}`)
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO storage_connections (name, provider, config, access_key, secret_enc) VALUES ($1, $2, $3::jsonb, $4, $5) RETURNING id`)).
		WithArgs("nas", "s3", string(cfg), "AKIA", sqlmock.AnyArg()). // secret_enc is non-deterministic ciphertext
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(7))

	id, err := s.CreateStorageConnection(&models.StorageConnection{Name: "nas", Provider: "s3", Config: json.RawMessage(cfg), AccessKey: "AKIA", SecretAccessKey: "the-secret"})
	if err != nil {
		t.Fatalf("CreateStorageConnection: %v", err)
	}
	if id != 7 {
		t.Fatalf("id = %d, want 7", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestCreateStorageConnection_NoKeyFailsClosedBeforeDB(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db) // no key

	// No ExpectQuery is set: if the insert ran, sqlmock would flag an
	// unexpected query. Proving the secret never reaches the DB in the clear.
	_, err = s.CreateStorageConnection(&models.StorageConnection{Name: "nas", Provider: "s3", Config: json.RawMessage(`{}`), AccessKey: "AKIA", SecretAccessKey: "plaintext"})
	if !errors.Is(err, errNoStorageConnKey) {
		t.Fatalf("CreateStorageConnection err = %v, want errNoStorageConnKey", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a query ran despite the fail-closed guard: %v", err)
	}
}

func TestGetStorageConnection_DecryptsSecret(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := keyedConnStore(db)

	enc, err := s.encodeStorageConnSecret("the-secret")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	now := time.Now()
	rows := sqlmock.NewRows([]string{"id", "name", "provider", "config", "access_key", "secret_enc", "created_at", "updated_at"}).
		AddRow(3, "nas", "s3", []byte(`{"bucket":"b"}`), "AKIA", enc, now, now)
	mock.ExpectQuery(regexp.QuoteMeta("FROM storage_connections WHERE id = $1")).
		WithArgs(3).
		WillReturnRows(rows)

	got, err := s.GetStorageConnection(3)
	if err != nil {
		t.Fatalf("GetStorageConnection: %v", err)
	}
	if !got.SecretSet {
		t.Error("SecretSet = false, want true (a secret is stored)")
	}
	if got.SecretAccessKey != "the-secret" {
		t.Errorf("SecretAccessKey = %q, want the decrypted secret", got.SecretAccessKey)
	}
	if got.AccessKey != "AKIA" || string(got.Config) != `{"bucket":"b"}` {
		t.Errorf("metadata round-trip wrong: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestGetStorageConnection_NoSecretStored(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := keyedConnStore(db)

	now := time.Now()
	rows := sqlmock.NewRows([]string{"id", "name", "provider", "config", "access_key", "secret_enc", "created_at", "updated_at"}).
		AddRow(4, "local-ish", "s3", []byte(`{}`), "AKIA", "", now, now)
	mock.ExpectQuery(regexp.QuoteMeta("FROM storage_connections WHERE id = $1")).
		WithArgs(4).
		WillReturnRows(rows)

	got, err := s.GetStorageConnection(4)
	if err != nil {
		t.Fatalf("GetStorageConnection: %v", err)
	}
	if got.SecretSet {
		t.Error("SecretSet = true, want false (no secret stored)")
	}
	if got.SecretAccessKey != "" {
		t.Errorf("SecretAccessKey = %q, want empty", got.SecretAccessKey)
	}
}

// TestListStorageConnections_NeverDecrypts proves the list path reports
// SecretSet but never loads a plaintext secret into the result, so a list view
// cannot leak credentials even in memory.
func TestListStorageConnections_NeverDecrypts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := keyedConnStore(db)

	enc, err := s.encodeStorageConnSecret("the-secret")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	now := time.Now()
	rows := sqlmock.NewRows([]string{"id", "name", "provider", "config", "access_key", "secret_enc", "created_at", "updated_at"}).
		AddRow(1, "with-secret", "s3", []byte(`{}`), "AK1", enc, now, now).
		AddRow(2, "without", "s3", []byte(`{}`), "AK2", "", now, now)
	mock.ExpectQuery(regexp.QuoteMeta("FROM storage_connections ORDER BY name")).
		WillReturnRows(rows)

	got, err := s.ListStorageConnections()
	if err != nil {
		t.Fatalf("ListStorageConnections: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if !got[0].SecretSet || got[1].SecretSet {
		t.Errorf("SecretSet flags wrong: [%v %v], want [true false]", got[0].SecretSet, got[1].SecretSet)
	}
	for i, c := range got {
		if c.SecretAccessKey != "" {
			t.Errorf("row %d leaked a decrypted secret in the list path: %q", i, c.SecretAccessKey)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestUpdateStorageConnection_LeavesSecretUntouched pins the exact metadata-only
// UPDATE. secret_enc is deliberately absent: a metadata edit must never clear or
// rewrite the write-only secret.
func TestUpdateStorageConnection_LeavesSecretUntouched(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := keyedConnStore(db)

	cfg := []byte(`{"bucket":"b"}`)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE storage_connections SET name = $1, provider = $2, config = $3::jsonb, access_key = $4, updated_at = CURRENT_TIMESTAMP WHERE id = $5`)).
		WithArgs("nas", "s3", string(cfg), "AKIA", 5).
		WillReturnResult(sqlmock.NewResult(0, 1))

	c := models.StorageConnection{ID: 5, Name: "nas", Provider: "s3", Config: json.RawMessage(cfg), AccessKey: "AKIA", SecretAccessKey: "ignored-on-update"}
	if err := s.UpdateStorageConnection(&c); err != nil {
		t.Fatalf("UpdateStorageConnection: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSetStorageConnectionSecret_EncryptsAndWritesOnlySecret(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := keyedConnStore(db)

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE storage_connections SET secret_enc = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`)).
		WithArgs(sqlmock.AnyArg(), 9).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.SetStorageConnectionSecret(9, "rotated-secret"); err != nil {
		t.Fatalf("SetStorageConnectionSecret: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSetStorageConnectionSecret_NoKeyFailsClosedBeforeDB(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db) // no key

	if err := s.SetStorageConnectionSecret(9, "plaintext"); !errors.Is(err, errNoStorageConnKey) {
		t.Fatalf("SetStorageConnectionSecret err = %v, want errNoStorageConnKey", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a query ran despite the fail-closed guard: %v", err)
	}
}

func TestDeleteStorageConnection(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM storage_connections WHERE id = $1`)).
		WithArgs(2).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.DeleteStorageConnection(2); err != nil {
		t.Fatalf("DeleteStorageConnection: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
