package database

import (
	"testing"
	"time"
)

// The durable sign-out list: a revoked id is found until its expiry, a second
// revoke keeps the later expiry, and expired rows are cleaned on the next one.
func TestIntegrationRevokedSessions(t *testing.T) {
	db, st := integrationDB(t)
	id := uniqueName("sess_")
	if ok, err := st.IsSessionRevoked(id); err != nil || ok {
		t.Fatalf("fresh id revoked=%v err=%v", ok, err)
	}
	if err := st.RevokeSession(id, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.IsSessionRevoked(id); err != nil || !ok {
		t.Fatalf("revoked id revoked=%v err=%v", ok, err)
	}
	if err := st.RevokeSession(id, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.IsSessionRevoked(id); !ok {
		t.Fatal("a second, earlier expiry shortened the revocation")
	}
	old := uniqueName("sess_old_")
	if _, err := db.Exec(`INSERT INTO revoked_sessions (id, expires_at) VALUES ($1, NOW() - INTERVAL '1 day')`, old); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeSession(uniqueName("sess_new_"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM revoked_sessions WHERE id = $1`, old).Scan(&n); err != nil || n != 0 {
		t.Fatalf("expired row kept: n=%d err=%v", n, err)
	}
}
