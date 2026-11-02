package handlers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"dylaris-core/handlers/handlertest"
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
	// All of core, at any depth: a check that moved into a subpackage or a
	// service is still a check. The setup wizard is exempt by its exact path,
	// so the exemption cannot follow a file of the same name elsewhere and
	// fails loudly when setup.go moves.
	root := handlertest.HandlersRoot(t)
	setup := filepath.Join(root, "setup.go")
	for _, f := range handlertest.SourceFiles(t, filepath.Join(root, "..")) {
		if f == setup {
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
	if _, err := os.Stat(setup); err != nil {
		t.Errorf("the exempt setup wizard is gone from %s: move the exemption with it", setup)
	}
}
