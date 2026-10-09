package database

import (
	"strings"
	"testing"
)

// A profile address change waits for its confirmation: until the link is
// opened the account keeps its address and its verified mark, and opening it
// swaps the two in one statement. The guard is the SQL, so it is measured here.
func TestIntegrationPendingEmailWaitsForTheLink(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)
	if err := st.MarkEmailVerified(f.user.ID); err != nil {
		t.Fatal(err)
	}
	before, err := st.GetUserByID(f.user.ID)
	if err != nil {
		t.Fatal(err)
	}

	next := uniqueName("next_") + "@example.test"
	token := uniqueName("tok_") + "0123456789abcdef"
	if err := st.SetPendingEmail(f.user.ID, next, token); err != nil {
		t.Fatalf("SetPendingEmail: %v", err)
	}
	waiting, err := st.GetUserByEmailVerificationToken(token)
	if err != nil {
		t.Fatalf("the link does not find the account: %v", err)
	}
	if waiting.Email != before.Email || waiting.EmailVerifiedAt == nil || waiting.PendingEmail != next {
		t.Fatalf("while waiting: email %q verified %v pending %q", waiting.Email, waiting.EmailVerifiedAt, waiting.PendingEmail)
	}

	old, now, ok, err := st.ConfirmPendingEmail(f.user.ID)
	if err != nil || !ok || old != before.Email || now != next {
		t.Fatalf("confirm = %q, %q, %v, %v", old, now, ok, err)
	}
	after, _ := st.GetUserByID(f.user.ID)
	if after.Email != next || after.PendingEmail != "" || after.EmailVerifiedAt == nil || after.EmailVerificationToken != "" {
		t.Fatalf("after: email %q pending %q verified %v token %q", after.Email, after.PendingEmail, after.EmailVerifiedAt, after.EmailVerificationToken)
	}
	if _, _, ok, err := st.ConfirmPendingEmail(f.user.ID); ok || err != nil {
		t.Fatalf("a second confirm changed something: %v %v", ok, err)
	}

	// Withdrawn, nothing waits and the account is as it was.
	if err := st.SetPendingEmail(f.user.ID, uniqueName("x_")+"@example.test", token); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPendingEmail(f.user.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	var pending *string
	if err := db.QueryRow(`SELECT pending_email FROM users WHERE id = $1`, f.user.ID).Scan(&pending); err != nil || pending != nil {
		t.Fatalf("pending after withdrawal: %v, %v", pending, err)
	}

	// Any other verification link ends it: a resend to the CURRENT address
	// used to confirm the pending one, an address nobody had proved.
	if err := st.SetPendingEmail(f.user.ID, uniqueName("z_")+"@example.test", token); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEmailVerificationToken(f.user.ID, uniqueName("resend_")+"0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if u, _ := st.GetUserByID(f.user.ID); u.PendingEmail != "" {
		t.Fatalf("a resend left %q pending, to be confirmed by its link", u.PendingEmail)
	}

	// An admin setting the address drops whatever the user had pending.
	if err := st.SetPendingEmail(f.user.ID, uniqueName("y_")+"@example.test", token); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUserEmail(f.user.ID, uniqueName("admin_")+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if u, _ := st.GetUserByID(f.user.ID); u.PendingEmail != "" {
		t.Fatalf("an admin-set address left %q pending", u.PendingEmail)
	}
}

// A sign-in typed with other capitals reached no account, although the unique
// index already makes the names case-insensitive.
func TestIntegrationUsernameLookupIgnoresCase(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	u, err := st.GetUserByUsername(strings.ToUpper(f.user.Username))
	if err != nil || u == nil || u.ID != f.user.ID {
		t.Fatalf("lookup by %q = %+v, %v", strings.ToUpper(f.user.Username), u, err)
	}
}
