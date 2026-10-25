package services

import (
	"strconv"
	"strings"
	"time"
)

// parseRetentionSpec parses a retention spec of the form "<n><unit>" where unit
// is d (days), w (weeks) or m (calendar months). Returns ok=false for an empty
// or malformed spec so callers fall back to a default.
func parseRetentionSpec(spec string) (n int, unit byte, ok bool) {
	spec = strings.TrimSpace(strings.ToLower(spec))
	if len(spec) < 2 {
		return 0, 0, false
	}
	unit = spec[len(spec)-1]
	if unit != 'd' && unit != 'w' && unit != 'm' {
		return 0, 0, false
	}
	n, err := strconv.Atoi(spec[:len(spec)-1])
	if err != nil || n < 0 {
		return 0, 0, false
	}
	return n, unit, true
}

// AddRetention advances t by the retention spec. Weeks expand to 7 days; months
// use calendar arithmetic (AddDate), so "3m" is three calendar months, not 90
// days. ok=false (t unchanged) when the spec is invalid.
//
// The calendar is UTC's: AddDate in a local zone with DST makes the day of the
// change 23 or 25 hours long, so a deadline would move with the TZ Core runs in.
func AddRetention(t time.Time, spec string) (time.Time, bool) {
	n, unit, ok := parseRetentionSpec(spec)
	if !ok {
		return t, false
	}
	t = t.UTC()
	switch unit {
	case 'd':
		return t.AddDate(0, 0, n), true
	case 'w':
		return t.AddDate(0, 0, n*7), true
	case 'm':
		return t.AddDate(0, n, 0), true
	}
	return t, false
}

// ValidRetentionSpec reports whether a spec is well-formed (for settings input).
func ValidRetentionSpec(spec string) bool {
	_, _, ok := parseRetentionSpec(spec)
	return ok
}
