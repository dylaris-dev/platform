package validate_test

import (
	"strings"
	"testing"

	"dylaris-core/handlers/validate"
)

// TestSlugify pins slugify's real behavior read from handlers/utils.go:
// lowercase, TrimSpace first, then any run of characters outside [a-z0-9_-]
// collapses to a single dash, then leading/trailing '-' or '_' are trimmed,
// then the result is capped at 64 chars. Existing dashes/underscores in the
// input are NOT collapsed by the regex step (only disallowed-char runs are);
// truncation happens after trimming, so it can leave a trailing dash if it
// cuts mid-run. Expected values below were verified against the actual
// implementation, not assumed.
func TestSlugify(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"basic space to dash", "My Server", "my-server"},
		{"empty input", "", ""},
		{"whitespace only", "   ", ""},
		{"underscore preserved (allowed charset)", "hello_world", "hello_world"},
		{"run of spaces collapses to one dash", "hello   world", "hello-world"},
		{"surrounding whitespace and punctuation trimmed", "  Hello World!  ", "hello-world"},
		{"leading disallowed run trimmed after dash conversion", "!!!leading", "leading"},
		{"existing dash run NOT collapsed", "a---b", "a---b"},
		{"all-underscore input trims to empty", "___", ""},
		{"all-dash input trims to empty", "---", ""},
		{"mixed leading/trailing dash and underscore trimmed", "_-_leading-trailing-_", "leading-trailing"},
		{"unicode runs of disallowed runes collapse", "Café Müller", "caf-m-ller"},
		{
			"truncates to 64 chars",
			strings.Repeat("a", 100),
			strings.Repeat("a", 64),
		},
		{
			"truncation after trim can leave a trailing dash mid-run",
			strings.Repeat("a", 63) + "-" + "b",
			strings.Repeat("a", 63) + "-",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := validate.Slugify(c.input); got != c.want {
				t.Errorf("slugify(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}
