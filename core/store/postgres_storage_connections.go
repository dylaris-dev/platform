package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"

	"dylaris-core/models"
	"dylaris-core/pkg/crypto"
)

// storageConnSecretPurpose scopes the at-rest key derivation for storage
// connection secrets, so it is distinct from every other CLUSTER_SECRET-derived
// key (node secrets, settings secrets).
const storageConnSecretPurpose = "storage-connection-secret"

// errNoStorageConnKey is returned when a secret would have to be encrypted but
// no key is configured. It fails the write closed rather than persisting a
// plaintext secret.
var errNoStorageConnKey = errors.New("storage connections: no encryption key configured; refusing to store a plaintext secret")

// SetStorageConnEncryptionKey installs the at-rest key, derived from
// CLUSTER_SECRET. Called once at boot. Until it is set (tests, early boot) a
// write carrying a non-empty secret fails closed.
func (s *PostgresStore) SetStorageConnEncryptionKey(clusterSecret string) {
	if clusterSecret == "" {
		return
	}
	s.storageConnSecretKey = crypto.DeriveKey(clusterSecret, storageConnSecretPurpose)
}

// encodeStorageConnSecret encrypts a connection secret for storage. An empty
// secret stores an empty column (no secret). It fails CLOSED: with no key
// configured it refuses to persist a plaintext secret. This table is greenfield
// so there is no legacy plaintext to read through - every stored secret is
// always ciphertext.
func (s *PostgresStore) encodeStorageConnSecret(secret string) (string, error) {
	if secret == "" {
		return "", nil
	}
	if s.storageConnSecretKey == nil {
		return "", errNoStorageConnKey
	}
	return crypto.Encrypt(s.storageConnSecretKey, []byte(secret))
}

// decodeStorageConnSecret decrypts a stored secret on read. An empty column is
// no secret. An encrypted value with no key configured, or one that fails to
// decrypt (a rotated CLUSTER_SECRET), returns "" and logs - a provider build
// then fails cleanly instead of authenticating with ciphertext-as-secret. This
// mirrors the fail-to-empty read behaviour of the settings secrets.
func (s *PostgresStore) decodeStorageConnSecret(enc string) string {
	if enc == "" {
		return ""
	}
	if s.storageConnSecretKey == nil {
		log.Printf("storage connections: a secret is encrypted but no encryption key is configured")
		return ""
	}
	pt, err := crypto.Decrypt(s.storageConnSecretKey, enc)
	if err != nil {
		log.Printf("storage connections: could not decrypt a secret: %v", err)
		return ""
	}
	return string(pt)
}

// ListStorageConnections returns every connection, ordered by name. It does NOT
// decrypt the secret (a list view never needs it); SecretSet reports whether one
// is stored and SecretAccessKey stays empty.
func (s *PostgresStore) ListStorageConnections() ([]models.StorageConnection, error) {
	rows, err := s.db.Query(`SELECT id, name, provider, config, access_key, secret_enc, created_at, updated_at FROM storage_connections ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.StorageConnection
	for rows.Next() {
		var c models.StorageConnection
		var cfg []byte
		var secretEnc string
		if err := rows.Scan(&c.ID, &c.Name, &c.Provider, &cfg, &c.AccessKey, &secretEnc, &c.CreatedAt, &c.UpdatedAt); err != nil {
			continue
		}
		c.Config = json.RawMessage(cfg)
		c.SecretSet = secretEnc != ""
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetStorageConnection returns one connection with its secret DECRYPTED into
// SecretAccessKey (json:"-", so it never crosses the API). The resolver uses
// this to build a provider; the panel edit view ignores the decrypted field and
// reads only SecretSet. A decrypt failure yields an empty SecretAccessKey (see
// decodeStorageConnSecret), never an error, so the panel can still display and
// re-key a connection whose secret became unreadable.
func (s *PostgresStore) GetStorageConnection(id int) (*models.StorageConnection, error) {
	var c models.StorageConnection
	var cfg []byte
	var secretEnc string
	err := s.db.QueryRow(`SELECT id, name, provider, config, access_key, secret_enc, created_at, updated_at FROM storage_connections WHERE id = $1`, id).
		Scan(&c.ID, &c.Name, &c.Provider, &cfg, &c.AccessKey, &secretEnc, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	c.Config = json.RawMessage(cfg)
	c.SecretSet = secretEnc != ""
	c.SecretAccessKey = s.decodeStorageConnSecret(secretEnc)
	return &c, nil
}

// CreateStorageConnection inserts a connection, encrypting the secret from
// c.SecretAccessKey. A failed encryption (no key) aborts the insert.
func (s *PostgresStore) CreateStorageConnection(c *models.StorageConnection) (int, error) {
	cfg := []byte(c.Config)
	if len(cfg) == 0 {
		cfg = []byte("{}")
	}
	enc, err := s.encodeStorageConnSecret(c.SecretAccessKey)
	if err != nil {
		return 0, err
	}
	var id int
	err = s.db.QueryRow(
		`INSERT INTO storage_connections (name, provider, config, access_key, secret_enc) VALUES ($1, $2, $3::jsonb, $4, $5) RETURNING id`,
		c.Name, c.Provider, string(cfg), c.AccessKey, enc,
	).Scan(&id)
	if isUniqueViolation(err) {
		return 0, ErrNameTaken
	}
	return id, err
}

// UpdateStorageConnection updates only the non-secret fields and touches
// updated_at. It deliberately does NOT write secret_enc: the secret is a
// write-only field, so a metadata edit must never clear it, and a decrypt is
// never needed. Rotating the secret goes through SetStorageConnectionSecret.
func (s *PostgresStore) UpdateStorageConnection(c *models.StorageConnection) error {
	cfg := []byte(c.Config)
	if len(cfg) == 0 {
		cfg = []byte("{}")
	}
	res, err := s.db.Exec(
		`UPDATE storage_connections SET name = $1, provider = $2, config = $3::jsonb, access_key = $4, updated_at = CURRENT_TIMESTAMP WHERE id = $5`,
		c.Name, c.Provider, string(cfg), c.AccessKey, c.ID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrNameTaken
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetStorageConnectionSecret rotates just the secret for one connection,
// encrypting it before write. It is called only when a new secret was submitted,
// so an empty submission (the "leave blank to keep" case) never reaches here and
// the stored secret survives. An empty secret would clear the column, but the
// handler guards against that path.
func (s *PostgresStore) SetStorageConnectionSecret(id int, secret string) error {
	enc, err := s.encodeStorageConnSecret(secret)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`UPDATE storage_connections SET secret_enc = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`,
		enc, id,
	)
	return err
}

// DeleteStorageConnection removes a connection by id.
func (s *PostgresStore) DeleteStorageConnection(id int) error {
	_, err := s.db.Exec(`DELETE FROM storage_connections WHERE id = $1`, id)
	return err
}
