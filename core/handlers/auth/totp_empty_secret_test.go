package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dylaris-core/handlers/handlertest"
)

// Every check of a stored or submitted secret goes through
// reauth.MatchTOTPStep. The setup wizard refuses an empty secret itself before
// it validates.
func TestNoHandlerValidatesAroundTheEmptySecretGuard(t *testing.T) {
	// All of core, at any depth: a check that moved into a subpackage or a
	// service is still a check. The setup wizard is exempt by its exact path,
	// so the exemption cannot follow a file of the same name elsewhere and
	// fails loudly when setup.go moves.
	root := handlertest.HandlersRoot(t)
	setup := filepath.Join(root, "auth", "setup.go")
	for _, f := range handlertest.SourceFiles(t, filepath.Join(root, "..")) {
		if f == setup {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "totp.Validate(") {
			t.Errorf("%s calls totp.Validate directly; use reauth.MatchTOTPStep", f)
		}
	}
	if _, err := os.Stat(setup); err != nil {
		t.Errorf("the exempt setup wizard is gone from %s: move the exemption with it", setup)
	}
}
