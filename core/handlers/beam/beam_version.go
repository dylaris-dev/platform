package beam

import (
	"strconv"
	"strings"
)

// parseBeamSemver parses an ordered MAJOR.MINOR.PATCH version, mirroring the
// Beam app's parseSemver (beam/app/updater.go): an optional leading "v" is
// stripped and any "-prerelease"/"+build" suffix is dropped before parsing.
// ok=false for anything that does not reduce to exactly three non-negative
// numeric parts (incl. "", "dev", "1.2"). Kept byte-for-byte equivalent to the
// app rule so the client and the server agree on "below min".
func parseBeamSemver(s string) ([3]int, bool) {
	var out [3]int
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// beamClientBelowMin reports whether a Beam client at clientVer must be blocked
// given the admin-set floor minVer. Gating is OFF when minVer is empty or does
// not parse (never below) - a malformed stored floor fails safe to "allow"
// rather than locking everyone out (SaveBeamSettings already rejects malformed
// input on the way in). With gating ON, an absent/unparseable clientVer
// (including "dev" and pre-Phase-2 clients that send no X-Beam-Version header)
// is treated as below-min: FAIL-CLOSED, so exactly the old clients the floor
// exists to lock out cannot bypass it by omitting the header. Otherwise it is
// an ordered MAJOR.MINOR.PATCH compare.
func beamClientBelowMin(clientVer, minVer string) bool {
	min, ok := parseBeamSemver(minVer)
	if !ok {
		return false // no valid floor -> gating off
	}
	cur, ok := parseBeamSemver(clientVer)
	if !ok {
		return true // fail-closed: absent/unparseable client version
	}
	for i := 0; i < 3; i++ {
		if cur[i] != min[i] {
			return cur[i] < min[i]
		}
	}
	return false // equal core -> allowed
}
