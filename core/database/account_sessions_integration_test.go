package database

import (
	"testing"
	"time"
)

// A backup code is spent by a write that only lands over the list the caller
// read: the read-modify-write before it let two logins spend one code, brought
// back a code spent concurrently, and wrote a just-reset secret back.
func TestIntegrationABackupCodeIsSpentOnce(t *testing.T) {
	db, st := integrationDB(t)
	var id string
	if err := db.QueryRow(`INSERT INTO users (username, password, role) VALUES ($1, 'x', 'user') RETURNING id`,
		"bc-"+time.Now().Format("150405.000000000")).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUserTOTP(id, "SECRET", `["a", "b"]`, true); err != nil {
		t.Fatal(err)
	}
	u, err := st.GetUserByID(id)
	if err != nil {
		t.Fatal(err)
	}
	read := u.TOTPBackupCodes

	if ok, err := st.ConsumeTOTPBackupCode(id, read, `["b"]`); err != nil || !ok {
		t.Fatalf("first spend: %v %v", ok, err)
	}
	// The second login read the same list before the first wrote.
	if ok, err := st.ConsumeTOTPBackupCode(id, read, `["b"]`); err != nil || ok {
		t.Fatalf("the same code was spent twice: %v %v", ok, err)
	}
	// 2FA switched off with the list untouched: nothing is spent any more.
	u, _ = st.GetUserByID(id)
	if _, err := db.Exec(`UPDATE users SET is_2fa_enabled = FALSE WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.ConsumeTOTPBackupCode(id, u.TOTPBackupCodes, `[]`); ok {
		t.Fatal("a code was spent with 2FA off")
	}
	if _, err := db.Exec(`UPDATE users SET is_2fa_enabled = TRUE WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	// A reset in between: the stale write must not land.
	u, _ = st.GetUserByID(id)
	if err := st.DisableUserTOTP(id); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.ConsumeTOTPBackupCode(id, u.TOTPBackupCodes, `[]`); ok {
		t.Fatal("a code was spent on an account whose 2FA was reset")
	}
	u, _ = st.GetUserByID(id)
	if u.Is2FAEnabled || u.TOTPSecret != "" {
		t.Fatalf("the reset was undone: %+v", u.Is2FAEnabled)
	}
}

func TestIntegrationTheSessionEpochIsReadAndBumped(t *testing.T) {
	db, st := integrationDB(t)
	var id string
	if err := db.QueryRow(`INSERT INTO users (username, password, role) VALUES ($1, 'x', 'user') RETURNING id`,
		"ep-"+time.Now().Format("150405.000000000")).Scan(&id); err != nil {
		t.Fatal(err)
	}
	u, err := st.GetUserByID(id)
	if err != nil || u.SessionEpoch != 0 {
		t.Fatalf("new user epoch %d (%v)", u.SessionEpoch, err)
	}
	for want := 1; want <= 2; want++ {
		got, err := st.BumpSessionEpoch(id)
		if err != nil || got != want {
			t.Fatalf("bump: %d %v, want %d", got, err, want)
		}
	}
	if u, _ = st.GetUserByID(id); u.SessionEpoch != 2 {
		t.Fatalf("read back %d", u.SessionEpoch)
	}
}
