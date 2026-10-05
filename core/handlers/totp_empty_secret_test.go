package handlers

import (
	"os"
	"path/filepath"
	"strings"
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
	if _, ok := matchTOTPStep(code, "", now); ok {
		t.Error("matchTOTPStep accepted the public code of an empty secret")
	}
}

// Every check of a stored or submitted secret goes through matchTOTPStep. The
// setup wizard refuses an empty secret itself before it validates.
func TestNoHandlerValidatesAroundTheEmptySecretGuard(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "setup.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "totp.Validate(") {
			t.Errorf("%s calls totp.Validate directly; use matchTOTPStep", f)
		}
	}
}
