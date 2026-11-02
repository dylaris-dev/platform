package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// codeAt generates the code a correct authenticator would show at t.
func codeAt(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, at, totp.ValidateOpts{
		Period: 30, Skew: 0, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom: %v", err)
	}
	return code
}

func newSecret(t *testing.T) string {
	t.Helper()
	key, err := totp.Generate(totp.GenerateOpts{Issuer: totpIssuer, AccountName: "alice"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return key.Secret()
}

// TestDiagnoseRejectedTOTP_ClockSkew: a code from the right secret but the wrong
// time must be reported as a clock problem, with the drift named. This is the
// half that turns "2FA says invalid" into an actionable finding.
func TestDiagnoseRejectedTOTP_ClockSkew(t *testing.T) {
	secret := newSecret(t)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		offset   time.Duration
		wantSide string
	}{
		{"authenticator 5 minutes ahead", 5 * time.Minute, "behind"},
		{"authenticator 5 minutes behind", -5 * time.Minute, "ahead of"},
		{"just outside the library's own skew", 90 * time.Second, "behind"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code := codeAt(t, secret, now.Add(c.offset))
			got := diagnoseRejectedTOTP(secret, code, now)
			if !strings.Contains(got, "clock") {
				t.Fatalf("diagnosis = %q, want it to name the clock", got)
			}
			if !strings.Contains(got, c.wantSide) {
				t.Errorf("diagnosis = %q, want it to say the server is %q", got, c.wantSide)
			}
			if strings.Contains(got, secret) || strings.Contains(got, code) {
				t.Error("the diagnosis leaked the secret or the code into a log line")
			}
		})
	}
}

// TestDiagnoseRejectedTOTP_WrongSecret: a code from a DIFFERENT secret must not
// be blamed on the clock. This is the stale-authenticator-entry case, which is
// easy to hit because every wizard open mints a fresh secret.
func TestDiagnoseRejectedTOTP_WrongSecret(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	stale := newSecret(t)
	current := newSecret(t)

	got := diagnoseRejectedTOTP(current, codeAt(t, stale, now), now)
	if strings.Contains(got, "clock") {
		t.Fatalf("diagnosis = %q, want the secret blamed, not the clock", got)
	}
	if !strings.Contains(got, "DIFFERENT secret") {
		t.Errorf("diagnosis = %q, want it to name a different secret", got)
	}
}

// TestDiagnoseRejectedTOTP_GarbageCode: a mistyped code is not a clock problem
// either, and must not crash the diagnosis.
func TestDiagnoseRejectedTOTP_GarbageCode(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	for _, code := range []string{"", "000000", "abcdef", "12345678901234567890"} {
		got := diagnoseRejectedTOTP(newSecret(t), code, now)
		if strings.Contains(got, "clock") {
			t.Errorf("code %q: diagnosis = %q, want no clock claim", code, got)
		}
		if got == "" {
			t.Errorf("code %q: empty diagnosis", code)
		}
	}
}

// TestDiagnoseRejectedTOTP_DriftBeyondWindow: drift larger than the search window
// falls back to the wrong-secret wording. Worth pinning so the message is never
// silently wrong - it says "no time step within N", which stays true.
func TestDiagnoseRejectedTOTP_DriftBeyondWindow(t *testing.T) {
	secret := newSecret(t)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	got := diagnoseRejectedTOTP(secret, codeAt(t, secret, now.Add(2*time.Hour)), now)
	if strings.Contains(got, "clock") {
		t.Errorf("diagnosis = %q; beyond the window it must not claim a specific drift", got)
	}
	if !strings.Contains(got, "no time step within") {
		t.Errorf("diagnosis = %q, want the bounded-search wording", got)
	}
}
