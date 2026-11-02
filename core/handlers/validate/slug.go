package validate

import (
	"regexp"
	"strings"
)

// Slugify turns a display name into a URL-safe slug (lowercase, dashes for
// runs of non-alphanumerics, trimmed, max 64 chars). Shared by the pack
// builder handlers.
func Slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = regexp.MustCompile(`[^a-z0-9_-]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-_")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}
