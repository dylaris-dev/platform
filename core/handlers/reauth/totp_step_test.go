package reauth

import (
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

// pquerna validates a code against an empty secret: the HMAC key is empty, so
// the code is public. A sealed secret the store cannot open reads as "", which
// made it the code for that account.
func TestAnEmptySecretMatchesNoCode(t *testing.T) {
	now := time.Now()
	code, err := totp.GenerateCode("", now)
	if err != nil {
		t.Fatal(err)
	}
	if !totp.Validate(code, "") {
		t.Fatal("premise: the library no longer validates against an empty secret; this test can go")
	}
	if _, ok := MatchTOTPStep(code, "", now); ok {
		t.Error("MatchTOTPStep accepted the public code of an empty secret")
	}
}
