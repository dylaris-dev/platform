package store

import (
	"database/sql"
	"log"
	"strings"

	"dylaris-core/models"
	"dylaris-core/pkg/crypto"
)

// totpSecretPurpose derives the key that seals users.totp_secret. The column
// was plaintext, so a database dump, a read-only DB login or one SQL injection
// read was enough to mint the second factor of every account that has one.
const totpSecretPurpose = "totp-secret"

// totpEncMarker prefixes a sealed secret, the same marker the settings use. A
// value without it is a plaintext secret from before encryption and reads
// through unchanged.
const totpEncMarker = "enc:v1:"

// SetTOTPEncryptionKey installs the key, derived from CLUSTER_SECRET. Called
// once at boot; until then a sealed secret reads as "" (fails closed).
func (s *PostgresStore) SetTOTPEncryptionKey(clusterSecret string) {
	if clusterSecret == "" {
		return
	}
	s.totpSecretKey = crypto.DeriveKey(clusterSecret, totpSecretPurpose)
}

// decodeTOTPSecret opens a sealed secret. A value it cannot open reads as "".
// pquerna DOES validate a code against "" (an empty HMAC key, so a public
// code): every check refuses an empty secret before validating
// (handlers.matchTOTPStep). A key mismatch then locks the authenticator out;
// backup codes still work.
func (s *PostgresStore) decodeTOTPSecret(userID, value string) string {
	if !strings.HasPrefix(value, totpEncMarker) {
		return value
	}
	if s.totpSecretKey == nil {
		log.Printf("totp: the secret of user %s is sealed but no key is configured", userID)
		return ""
	}
	pt, err := crypto.Decrypt(s.totpSecretKey, strings.TrimPrefix(value, totpEncMarker))
	if err != nil {
		log.Printf("totp: could not open the secret of user %s: %v", userID, err)
		return ""
	}
	return string(pt)
}

// encodeTOTPSecret seals a secret for the column. With no key (tests, early
// boot) it passes through, as the settings do; Core refuses to boot without
// CLUSTER_SECRET, so a running Core always seals.
func (s *PostgresStore) encodeTOTPSecret(value string) (string, error) {
	if value == "" || s.totpSecretKey == nil {
		return value, nil
	}
	enc, err := crypto.Encrypt(s.totpSecretKey, []byte(value))
	if err != nil {
		return "", err
	}
	return totpEncMarker + enc, nil
}

// SealPlaintextTOTPSecrets seals every secret still stored in the clear and
// reports how many it sealed. A secret is only rewritten on enrolment, so
// without this the existing ones would stay plaintext for good. Each row is
// replaced only if it still holds the value read, so a second replica running
// the same pass, or an enrolment in between, makes it a no-op.
func (s *PostgresStore) SealPlaintextTOTPSecrets() (int, error) {
	if s.totpSecretKey == nil {
		return 0, nil
	}
	rows, err := s.db.Query(`SELECT id, totp_secret FROM users WHERE totp_secret <> '' AND totp_secret NOT LIKE $1`, totpEncMarker+"%")
	if err != nil {
		return 0, err
	}
	type pending struct{ id, val string }
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.val); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	sealed := 0
	for _, p := range todo {
		enc, err := s.encodeTOTPSecret(p.val)
		if err != nil {
			return sealed, err
		}
		res, err := s.db.Exec(`UPDATE users SET totp_secret = $1 WHERE id = $2 AND totp_secret = $3`, enc, p.id, p.val)
		if err != nil {
			return sealed, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			sealed++
		}
	}
	return sealed, nil
}

// scanUser reads a user row and opens its TOTP secret.
func (s *PostgresStore) scanUser(scan func(dest ...interface{}) error) (*models.User, error) {
	u, err := scanUserRow(scan)
	if err != nil {
		return nil, err
	}
	u.TOTPSecret = s.decodeTOTPSecret(u.ID, u.TOTPSecret)
	return u, nil
}

// SetUserTOTPBackupCodes replaces the hashed backup codes and leaves the secret
// alone. Regenerating the codes used to write back the secret it had read, so a
// secret that read as "" (a key mismatch) was erased while 2FA stayed on.
func (s *PostgresStore) SetUserTOTPBackupCodes(id, backupCodesJSON string) error {
	res, err := s.db.Exec(
		`UPDATE users SET totp_backup_codes = $1::jsonb WHERE id = $2 AND is_2fa_enabled = TRUE`,
		backupCodesJSON, id,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// resealTOTPSecrets moves the sealed TOTP secrets. Plaintext ones (no marker)
// are not sealed under either key and stay as they are.
func resealTOTPSecrets(tx *sql.Tx, fromKey, toKey []byte) (ResealBucket, error) {
	var b ResealBucket
	rows, err := tx.Query(`SELECT id, totp_secret FROM users WHERE totp_secret LIKE $1`, totpEncMarker+"%")
	if err != nil {
		return b, err
	}
	type pending struct{ id, val string }
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.val); err != nil {
			rows.Close()
			return b, err
		}
		todo = append(todo, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return b, err
	}
	rows.Close()

	for _, p := range todo {
		moved, err := crypto.Reseal(fromKey, toKey, strings.TrimPrefix(p.val, totpEncMarker))
		if err != nil {
			b.Unreadable++
			continue
		}
		if _, err := tx.Exec(`UPDATE users SET totp_secret=$1 WHERE id=$2`, totpEncMarker+moved, p.id); err != nil {
			return b, err
		}
		b.Moved++
	}
	return b, nil
}
