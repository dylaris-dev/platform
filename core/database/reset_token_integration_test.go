package database

import (
	"testing"
	"time"
)

// The reset read the user by token and then wrote the password by id, so two
// requests with one link both succeeded, and a request already past the read
// still worked after a newer link replaced it. The password now changes only
// in the same UPDATE that spends the token.
func TestIntegrationResetLinkIsSpentOnce(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	const hash = "$2a$10$resetresetresetresetresetresetresetresetresetresetrese"

	issue := func(tok string) {
		t.Helper()
		exp := time.Now().Add(time.Hour)
		if _, err := st.SetPasswordResetToken(f.user.ID, tok, exp, exp.Add(time.Hour)); err != nil {
			t.Fatalf("SetPasswordResetToken: %v", err)
		}
	}

	tok := uniqueName("tok_")
	issue(tok)
	if ok, err := st.ResetPasswordWithToken(f.user.ID, tok, hash); err != nil || !ok {
		t.Fatalf("first reset: ok=%v err=%v, want it to go through", ok, err)
	}
	if ok, _ := st.ResetPasswordWithToken(f.user.ID, tok, hash); ok {
		t.Fatal("the same link reset the password a second time")
	}

	old := uniqueName("old_")
	issue(old)
	issue(uniqueName("new_"))
	if ok, _ := st.ResetPasswordWithToken(f.user.ID, old, hash); ok {
		t.Fatal("a link replaced by a newer one still reset the password")
	}
}

// A verification link had no expiry: one in an old or leaked mailbox archive
// verified the address for as long as no newer link was asked for.
func TestIntegrationVerificationLinkExpires(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)
	tok := uniqueName("vtok_")
	if err := st.SetEmailVerificationToken(f.user.ID, tok); err != nil {
		t.Fatalf("SetEmailVerificationToken: %v", err)
	}
	if u, err := st.GetUserByEmailVerificationToken(tok); err != nil || u == nil {
		t.Fatalf("a fresh link was not accepted: %v", err)
	}
	if _, err := db.Exec(`UPDATE users SET email_verification_sent_at = NOW() - INTERVAL '8 days' WHERE id = $1`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	if u, _ := st.GetUserByEmailVerificationToken(tok); u != nil {
		t.Fatal("an eight-day-old verification link was still accepted")
	}
}
