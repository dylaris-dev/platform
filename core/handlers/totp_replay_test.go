package handlers

import (
	"testing"
	"time"

	"dylaris-core/models"

	"github.com/alicebob/miniredis/v2"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
)

func totpReplayState(t *testing.T) *AppState {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return &AppState{Redis: rdb}
}

func totpSecretFor(t *testing.T) string {
	t.Helper()
	key, err := totp.Generate(totp.GenerateOpts{Issuer: totpIssuer, AccountName: "replay@test"})
	if err != nil {
		t.Fatalf("generate secret: %v", err)
	}
	return key.Secret()
}

// A time-based one-time password is one-time or it is not one. This check did
// not exist: measured on production, the same six digits logged the same
// account in three times in a row, and the codes from the neighbouring steps
// worked too - so a code seen once was good for roughly 90 seconds and for as
// many sessions as its holder wanted.
//
// The backup codes in the same function have always been destructive on use.
func TestTOTPCodeCannotBeUsedTwice(t *testing.T) {
	state := totpReplayState(t)
	secret := totpSecretFor(t)
	user := &models.User{ID: "u-1", Username: "victim", TOTPSecret: secret}
	code := codeAt(t, secret, time.Now())

	ok, err := verifyTOTPOrBackupFor(state, user, code)
	if err != nil || !ok {
		t.Fatalf("the first use was rejected (ok=%v err=%v)", ok, err)
	}
	for i := 2; i <= 3; i++ {
		ok, err = verifyTOTPOrBackupFor(state, user, code)
		if err != nil {
			t.Fatalf("use %d errored: %v", i, err)
		}
		if ok {
			t.Fatalf("use %d of the same code was accepted; the code is replayable", i)
		}
	}
}

// Each step is claimed on its own, so spending one does not lock a user out of
// the next: an authenticator shows a new code every 30 seconds and it has to
// work.
func TestTOTPNextStepStillWorksAfterOneIsSpent(t *testing.T) {
	state := totpReplayState(t)
	secret := totpSecretFor(t)
	user := &models.User{ID: "u-1", TOTPSecret: secret}

	now := time.Now()
	if ok, _ := verifyTOTPOrBackupFor(state, user, codeAt(t, secret, now)); !ok {
		t.Fatal("the current code was rejected")
	}
	next := codeAt(t, secret, now.Add(totpStepSeconds*time.Second))
	if next == codeAt(t, secret, now) {
		t.Skip("the two steps produced the same digits; nothing to tell apart")
	}
	if ok, _ := verifyTOTPOrBackupFor(state, user, next); !ok {
		t.Error("the NEXT step's code was refused after the current one was spent")
	}
}

// Spending a code for one account must not spend it for another. The claim is
// keyed by user, and two people can hold the same six digits by coincidence.
func TestTOTPClaimIsPerAccount(t *testing.T) {
	state := totpReplayState(t)
	secret := totpSecretFor(t)
	code := codeAt(t, secret, time.Now())

	a := &models.User{ID: "u-a", TOTPSecret: secret}
	b := &models.User{ID: "u-b", TOTPSecret: secret}

	if ok, _ := verifyTOTPOrBackupFor(state, a, code); !ok {
		t.Fatal("account A was rejected")
	}
	if ok, _ := verifyTOTPOrBackupFor(state, b, code); !ok {
		t.Error("account B was refused a code A had spent; the claim is not per account")
	}
}

// Redis is what remembers a spent step, and it is not the factor itself - the
// password is still required. Refusing every second factor while Redis is down
// would lock out every account including the operator's, so this accepts and
// logs instead.
func TestTOTPWithoutRedisStillAuthenticates(t *testing.T) {
	secret := totpSecretFor(t)
	user := &models.User{ID: "u-1", TOTPSecret: secret}
	code := codeAt(t, secret, time.Now())

	ok, err := verifyTOTPOrBackupFor(&AppState{}, user, code)
	if err != nil || !ok {
		t.Fatalf("a valid code was refused with no Redis to check against (ok=%v err=%v)", ok, err)
	}
}

// The step a code belongs to, which is what makes it claimable exactly once.
// The skew either side is kept - a clock a few seconds out must still be able
// to log in - so each neighbouring step has to resolve to its OWN number, or
// claiming one would silently claim all three.
func TestMatchTOTPStep(t *testing.T) {
	secret := totpSecretFor(t)
	now := time.Now()

	cur, ok := matchTOTPStep(codeAt(t, secret, now), secret, now)
	if !ok {
		t.Fatal("the current step's code matched nothing")
	}
	if want := now.Unix() / totpStepSeconds; cur != want {
		t.Errorf("step = %d, want %d", cur, want)
	}

	for _, off := range []int64{-1, 1} {
		at := now.Add(time.Duration(off) * totpStepSeconds * time.Second)
		got, ok := matchTOTPStep(codeAt(t, secret, at), secret, now)
		if !ok {
			t.Errorf("a code %d step(s) away was not accepted; clock drift would lock people out", off)
			continue
		}
		if got == cur {
			t.Errorf("the code %d step(s) away resolved to the current step; the three would share one claim", off)
		}
	}

	if _, ok := matchTOTPStep("000000", secret, now.Add(48*time.Hour)); ok {
		t.Error("a code from two days away matched")
	}
}

// Re-authentication verifies a code WITHOUT spending its step, and one save
// can make two writes: the panel's "role and permissions" button calls two
// endpoints from one prompt, and the second would be refused as a replay of
// the code the operator had just typed.
//
// The replay this protects against is at the LOGIN endpoint, where a code plus
// a phished password is the whole credential. Somebody able to replay one here
// holds a session and the password already, and could simply sign in.
func TestReauthDoesNotSpendTheStep(t *testing.T) {
	state := totpReplayState(t)
	secret := totpSecretFor(t)
	code := codeAt(t, secret, time.Now())
	user := &models.User{ID: "u-1", TOTPSecret: secret}

	for i := 1; i <= 3; i++ {
		ok, err := verifyTOTPOrBackupWith(state, user, code, false)
		if err != nil || !ok {
			t.Fatalf("re-authentication %d was refused (ok=%v err=%v); one prompt cannot cover a save that writes twice", i, ok, err)
		}
	}

	// And the login path is untouched: the same code is still spent there.
	if ok, _ := verifyTOTPOrBackupFor(state, user, code); !ok {
		t.Fatal("the first login use was refused")
	}
	if ok, _ := verifyTOTPOrBackupFor(state, user, code); ok {
		t.Error("the code was replayable at login after the re-authentication path had seen it")
	}
}
