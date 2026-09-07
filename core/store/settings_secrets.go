package store

import (
	"log"
	"strings"

	"dylaris-core/pkg/crypto"
)

// settingsSecretKeys are the settings whose value is a credential that must be
// encrypted at rest. GetSetting decrypts them transparently and SetSetting
// encrypts them, so every caller keeps seeing plaintext while the DB never
// holds the secret in the clear. The backup s3 secret is NOT here - it lives in
// backup_storages.config JSONB and is handled separately.
var settingsSecretKeys = map[string]bool{
	"core_storage_s3_secret_key":    true,
	"modpack_storage_s3_secret_key": true,
	// DNS provider credential, set in Settings -> DNS. It can rewrite the
	// operator's zone, so it never sits in the clear next to the zone name.
	"dns.api_token": true,
	// Resend API key. Same argument as the SMTP password below it: whoever reads
	// a DB dump can send mail as the operator's domain, and that domain is where
	// the password-reset links come from.
	"resend.api_key": true,
	// The platform backup passphrase. It has to be STORED rather than only
	// verified, because a scheduled bundle is written at 04:00 with nobody
	// there to type it - a verifier is enough for a restore, which a human
	// starts, and not for a write. Storing it here rather than under its own
	// purpose is what puts it inside ResealAtRest for free, so a CLUSTER_SECRET
	// rotation does not silently stop automatic bundles.
	//
	// This costs nothing the passphrase was buying. It exists so a DOWNLOADED
	// bundle is independent of the secret that wrote it; it was never meant to
	// defend against somebody who already holds this database AND this cluster
	// secret, who owns the platform either way.
	"platform_backup.passphrase": true,
	// Password for a dedicated metadata-cache Redis. It is only a cache, but the
	// credential is still a credential: whoever reads a DB dump would otherwise
	// hold a working login to an endpoint on the operator's network.
	"mod_cache_redis_password": true,
}

// smtpPasswordSuffix identifies the SMTP credential settings, whose key is
// built per purpose ("smtp." + purpose + ".password", handlers/auth_settings.go)
// and so cannot be a fixed map entry.
const smtpPasswordSuffix = ".password"
const smtpSettingPrefix = "smtp."

// isSecretSettingKey reports whether a settings key holds a credential that
// must be encrypted at rest.
//
// The SMTP password was the one credential in this table stored in the clear.
// It is written through the same SetSettingBy that encodes everything else, so
// only its absence from the list decided that - and it belongs by the same
// argument the DNS token above carries: a mail credential lets whoever reads a
// DB dump send as the operator's domain, which is the reset-email domain. The
// UI already treats it write-only (GetSMTPConfig returns only passwordSet), so
// the clear-text copy in the settings table was the only place it was readable.
//
// Matched by shape rather than by exact key because the key is parameterised by
// purpose; the prefix keeps that from also catching an unrelated ".password".
func isSecretSettingKey(key string) bool {
	if settingsSecretKeys[key] {
		return true
	}
	return strings.HasPrefix(key, smtpSettingPrefix) && strings.HasSuffix(key, smtpPasswordSuffix)
}

// settingsEncMarker prefixes an encrypted value so a read can tell it apart from
// a legacy plaintext value written before encryption existed. Migration is
// therefore lazy and lossless: an old plaintext secret reads through unchanged
// and is re-encrypted the next time it is saved.
const settingsEncMarker = "enc:v1:"

const settingsSecretPurpose = "storage-settings-secret"

// SetSettingsEncryptionKey installs the at-rest key, derived from CLUSTER_SECRET.
// Called once at boot. Until it is set - tests, early boot - the secret
// settings pass through in the clear, which is the pre-encryption behaviour, so
// nothing breaks if it is never called.
func (s *PostgresStore) SetSettingsEncryptionKey(clusterSecret string) {
	if clusterSecret == "" {
		return
	}
	s.settingsSecretKey = crypto.DeriveKey(clusterSecret, settingsSecretPurpose)
}

// encodeSettingValue encrypts a secret setting for storage. A non-secret key, an
// empty value, or a store with no key configured passes through unchanged. It
// fails closed: an encryption error is returned rather than persisting the
// plaintext, which would be the exact leak this exists to prevent.
func (s *PostgresStore) encodeSettingValue(key, value string) (string, error) {
	if value == "" || s.settingsSecretKey == nil || !isSecretSettingKey(key) {
		return value, nil
	}
	enc, err := crypto.Encrypt(s.settingsSecretKey, []byte(value))
	if err != nil {
		return "", err
	}
	return settingsEncMarker + enc, nil
}

// decodeSettingValue decrypts a secret setting on read. A non-secret key, or a
// value without the marker (legacy plaintext), passes through. An encrypted
// value with no key configured, or one that fails to decrypt, returns "" - a
// provider build then fails cleanly instead of using ciphertext as a secret.
func (s *PostgresStore) decodeSettingValue(key, value string) string {
	if !isSecretSettingKey(key) || !strings.HasPrefix(value, settingsEncMarker) {
		return value
	}
	if s.settingsSecretKey == nil {
		log.Printf("settings: %s is encrypted but no encryption key is configured", key)
		return ""
	}
	pt, err := crypto.Decrypt(s.settingsSecretKey, strings.TrimPrefix(value, settingsEncMarker))
	if err != nil {
		log.Printf("settings: could not decrypt %s: %v", key, err)
		return ""
	}
	return string(pt)
}
