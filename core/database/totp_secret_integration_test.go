package database

import (
	"strings"
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

// The secrets stored before sealing existed are sealed at boot, once: a second
// replica running the same pass seals nothing, and every one still opens.
func TestIntegrationPlaintextTOTPSecretsAreSealedOnce(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)
	st.SetTOTPEncryptionKey(secretBefore)
	f := newFixture(t, st)

	const plain = "JBSWY3DPEHPK3PXP"
	if _, err := db.Exec(`UPDATE users SET totp_secret = $1, is_2fa_enabled = TRUE WHERE id = $2`, plain, f.user.ID); err != nil {
		t.Fatal(err)
	}
	n, err := st.SealPlaintextTOTPSecrets()
	if err != nil || n != 1 {
		t.Fatalf("first pass sealed %d (%v), want 1", n, err)
	}
	if n, err := st.SealPlaintextTOTPSecrets(); err != nil || n != 0 {
		t.Fatalf("second pass sealed %d (%v), want 0", n, err)
	}
	var raw string
	db.QueryRow(`SELECT totp_secret FROM users WHERE id = $1`, f.user.ID).Scan(&raw)
	if !strings.HasPrefix(raw, "enc:v1:") || strings.Contains(raw, plain) {
		t.Fatalf("column holds %q after sealing", raw)
	}
	if u, err := st.GetUserByID(f.user.ID); err != nil || u.TOTPSecret != plain {
		t.Fatalf("sealed secret reads as %q (%v)", u.TOTPSecret, err)
	}

	// Enrolment writes it sealed too.
	if err := st.SetUserTOTP(f.user.ID, "KRSXG5CTMVRXEZLU", "[]", true); err != nil {
		t.Fatal(err)
	}
	db.QueryRow(`SELECT totp_secret FROM users WHERE id = $1`, f.user.ID).Scan(&raw)
	if !strings.HasPrefix(raw, "enc:v1:") {
		t.Fatalf("SetUserTOTP stored %q", raw)
	}
}

// The setup wizard verifies a code against the secret it stores, and stored it
// with 2FA off: the first admin was never asked for a code. Against a real
// Postgres because the parameter types of this INSERT are Postgres's to infer.
func TestIntegrationTheWizardsSecretTurns2FAOn(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)
	st.SetTOTPEncryptionKey(secretBefore)

	const plain = "JBSWY3DPEHPK3PXP"
	u, err := st.CreateFirstAdmin("wizard-admin", "hash", plain)
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	if !u.Is2FAEnabled || u.TOTPSecret != plain {
		t.Fatalf("returned admin: 2FA %v, secret %q", u.Is2FAEnabled, u.TOTPSecret)
	}
	var raw string
	var on bool
	db.QueryRow(`SELECT totp_secret, is_2fa_enabled FROM users WHERE id = $1`, u.ID).Scan(&raw, &on)
	if !on || !strings.HasPrefix(raw, "enc:v1:") {
		t.Fatalf("stored: 2FA %v, secret %q", on, raw)
	}

	other, err := st.CreateAdditionalAdmin("break-glass", "hash", "")
	if err != nil {
		t.Fatalf("CreateAdditionalAdmin: %v", err)
	}
	if other.Is2FAEnabled {
		t.Error("an admin created without a secret has 2FA on")
	}
}
