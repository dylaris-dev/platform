package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Default MC host-port range, kept here so the fallback and the notice shown in
// the panel always quote the same numbers.
const (
	defaultPortRangeStart = 25600
	defaultPortRangeEnd   = 25699
)

// resolvePortRange reads PORT_RANGE ("start-end") and returns the effective
// range plus a notice describing why it is not what the environment said.
//
// The range stays env-only because the host firewall rules have to match it,
// and that is exactly why a typo must not pass silently: anything unusable
// falls back to the default and the notice rides along in the heartbeat, so an
// admin sees in the panel WHY the effective range differs from the config.
func resolvePortRange() (start, end int, notice string) {
	raw := strings.TrimSpace(os.Getenv("PORT_RANGE"))
	if raw == "" {
		// PORT_RANGE_START / PORT_RANGE_END were folded into PORT_RANGE. Name
		// them explicitly rather than quietly defaulting, or an upgrade would
		// move the range out from under the operator's firewall rules.
		if os.Getenv("PORT_RANGE_START") != "" || os.Getenv("PORT_RANGE_END") != "" {
			return defaultPortRangeStart, defaultPortRangeEnd, fmt.Sprintf(
				"PORT_RANGE_START/PORT_RANGE_END are no longer read - set PORT_RANGE=%d-%d instead; using the default range",
				defaultPortRangeStart, defaultPortRangeEnd)
		}
		// Relying on the default is a choice, not a fault: a notice here showed
		// as a warning on every such node, including gateway-only ones that
		// bind no host ports at all.
		return defaultPortRangeStart, defaultPortRangeEnd, ""
	}
	s, e, err := parsePortRange(raw)
	if err != nil {
		return defaultPortRangeStart, defaultPortRangeEnd, fmt.Sprintf(
			"PORT_RANGE %q is invalid (%v), using the default range %d-%d",
			raw, err, defaultPortRangeStart, defaultPortRangeEnd)
	}
	return s, e, ""
}

// parsePortRange validates a "start-end" range. An inverted range is rejected
// rather than clamped because the port manager sizes its allocation table from
// end-start+1, which panics at boot on a negative length.
func parsePortRange(raw string) (start, end int, err error) {
	first, last, ok := strings.Cut(raw, "-")
	if !ok {
		return 0, 0, fmt.Errorf("expected \"start-end\"")
	}
	start, err = strconv.Atoi(strings.TrimSpace(first))
	if err != nil {
		return 0, 0, fmt.Errorf("start is not a number")
	}
	end, err = strconv.Atoi(strings.TrimSpace(last))
	if err != nil {
		return 0, 0, fmt.Errorf("end is not a number")
	}
	if start < 1 || start > 65535 || end < 1 || end > 65535 {
		return 0, 0, fmt.Errorf("ports must be within 1-65535")
	}
	if end < start {
		return 0, 0, fmt.Errorf("end %d is below start %d", end, start)
	}
	return start, end, nil
}
