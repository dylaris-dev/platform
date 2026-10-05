package database

import (
	"testing"

	"dylaris-core/store"
)

// A secret stored before encryption existed keeps working, and replacing the
// backup codes leaves the secret column byte for byte as it was.
func TestIntegrationTOTPSecretLegacyRowAndBackupCodes(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)
	st.SetTOTPEncryptionKey(secretBefore)
	f := newFixture(t, st)

	const plain = "JBSWY3DPEHPK3PXP"
	if _, err := db.Exec(`UPDATE users SET totp_secret = $1, is_2fa_enabled = TRUE WHERE id = $2`, plain, f.user.ID); err != nil {
		t.Fatal(err)
	}
	u, err := st.GetUserByID(f.user.ID)
	if err != nil || u.TOTPSecret != plain {
		t.Fatalf("legacy secret read as %q (%v)", u.TOTPSecret, err)
	}

	if err := st.SetUserTOTPBackupCodes(f.user.ID, `["h1","h2"]`); err != nil {
		t.Fatalf("SetUserTOTPBackupCodes: %v", err)
	}
	var raw, codes string
	if err := db.QueryRow(`SELECT totp_secret, totp_backup_codes::text FROM users WHERE id = $1`, f.user.ID).Scan(&raw, &codes); err != nil {
		t.Fatal(err)
	}
	if raw != plain {
		t.Errorf("the secret column changed to %q", raw)
	}
	if codes != `["h1", "h2"]` {
		t.Errorf("backup codes = %s", codes)
	}

	// With 2FA off there is nothing to regenerate codes for.
	db.Exec(`UPDATE users SET is_2fa_enabled = FALSE WHERE id = $1`, f.user.ID)
	if err := st.SetUserTOTPBackupCodes(f.user.ID, `[]`); err == nil {
		t.Error("codes were written for a user without 2FA")
	}
}
