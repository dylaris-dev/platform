package validate_test

import (
	"strings"
	"testing"

	"dylaris-pkg/validate"
)

// Setup NAMES a sub-server directory and switch PICKS one, so both have to mean
// the same thing by a name. They did not: setup sanitized (strip what you do not
// like, keep the rest) and had no length bound, so it could mint a sub-server
// that switch would refuse forever. This pins the rule they now share, from the
// inputs that actually reached the running system.
func TestSubServerNameRule(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
		why   string
	}{
		{"a plain name", "survival", true, ""},
		{"digits and separators", "CAPS_123-x+y", true, ""},
		{"traversal", "../escape", false, "it used to sanitize to \"escape\" and create that instead, with a 200 and no mention"},
		{"a nested path", "a/b", false, "it used to sanitize to \"ab\""},
		{"a space", "has space", false, "it used to sanitize to \"has_space\""},
		{"a shell metacharacter", "name;rm", false, "it used to sanitize to \"namerm\""},
		{"non-ascii", "ä-umlaut", false, "it used to sanitize to \"-umlaut\", which is a real sub-server nobody named"},
		{"a leading dot", ".hidden", false, "dot-directories are node bookkeeping, not sub-servers"},
		{"empty", "", false, ""},
		{"50 characters", strings.Repeat("a", 50), true, "the documented bound is inclusive"},
		{"51 characters", strings.Repeat("a", 51), false, "sanitizing had no length bound at all, so this was accepted and then unswitchable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validate.IsSubServerName(tt.input); got != tt.want {
				t.Errorf("IsSubServerName(%q) = %v, want %v: %s", tt.input, got, tt.want, tt.why)
			}
		})
	}
}
