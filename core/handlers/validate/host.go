package validate

import (
	"regexp"
	"strings"
)

// NormHost is how a typed domain is stored: lowercase, and without the one
// trailing dot of a fully qualified name. The edge strips that dot from the
// player's handshake before it looks the route up, so a stored dot would be a
// route nobody can reach.
func NormHost(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

// IsHostname reports whether s is a lowercase DNS name of at least minLabels
// labels: each label 1-63 letters, digits or inner hyphens, 253 in all. No
// wildcard - the gateway routes exact Minecraft hostnames only.
func IsHostname(s string, minLabels int) bool {
	if len(s) > 253 {
		return false
	}
	labels := strings.Split(s, ".")
	if len(labels) < minLabels {
		return false
	}
	for _, l := range labels {
		if len(l) > 63 || !SubRegexDNS.MatchString(l) {
			return false
		}
	}
	return true
}

// Per-hoster subdomain validation regexes — kept narrow on purpose so the
// admin's choice in settings actually constrains what users can register.
var (
	subRegexLetters      = regexp.MustCompile(`^[a-z]+$`)
	subRegexAlphanumeric = regexp.MustCompile(`^[a-z0-9]+$`)
	SubRegexDNS          = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
)

// ValidateSubdomain matches a user-entered subdomain against the validation
// mode the admin picked for that hoster domain.
func ValidateSubdomain(sub, mode string) bool {
	if sub == "" || len(sub) > 63 {
		return false
	}
	switch mode {
	case "letters":
		return subRegexLetters.MatchString(sub)
	case "alphanumeric":
		return subRegexAlphanumeric.MatchString(sub)
	case "dns":
		return SubRegexDNS.MatchString(sub)
	default:
		return false
	}
}
