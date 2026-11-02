package auth

import "testing"

// A session is a stateless JWT - nothing on the server can retract one. The
// fingerprint is what lets a password change end the sessions that predate it,
// so the properties that matter are: it changes when the hash does, it is not
// the hash, and an empty hash produces an empty marker (an unbound session,
// which AuthMiddleware lets through on purpose).
func TestPasswordFingerprint(t *testing.T) {
	// Two real bcrypt hashes of the same password differ by salt, which is
	// exactly why a re-hash of the SAME password still ends old sessions.
	const oldHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
	const newHash = "$2a$10$abcdefghijklmnopqrstuvOJ0Zk9xVvGZ0uZ5Yc9K0Pq1rS2tU3vW"

	a := passwordFingerprint(oldHash)
	b := passwordFingerprint(newHash)

	if a == "" || b == "" {
		t.Fatal("a real hash produced an empty fingerprint - the session would be unbound")
	}
	if a == b {
		t.Error("two different hashes share a fingerprint - a password change would not end old sessions")
	}
	if a != passwordFingerprint(oldHash) {
		t.Error("the fingerprint is not stable for one hash - every request would 401")
	}
	for _, fp := range []string{a, b} {
		if len(fp) != 16 {
			t.Errorf("fingerprint %q is %d chars, want 16", fp, len(fp))
		}
	}

	t.Run("it does not carry the hash", func(t *testing.T) {
		// The token goes to the client; a bcrypt hash inside it would be an
		// offline cracking target.
		if a == oldHash || len(a) >= len(oldHash) {
			t.Errorf("fingerprint %q is not a reduction of the hash", a)
		}
	})

	t.Run("an empty hash is an unbound session", func(t *testing.T) {
		// AuthMiddleware treats an empty claim as "not password-bound" and lets
		// it through - that is what keeps tokens minted before this shipped
		// working until they expire, instead of logging the platform out on
		// deploy.
		if got := passwordFingerprint(""); got != "" {
			t.Errorf("passwordFingerprint(\"\") = %q, want empty", got)
		}
	})
}
