// Package release parses the hand-written release-notes files that drive both
// the in-panel update view and the Discord announcements.
//
// One parser, used by Core at runtime and by the CI tool that validates the
// file and builds the Discord payload. That is deliberate: a second
// implementation in a shell script would drift, and the first symptom of drift
// is an announcement that does not match what the panel shows.
//
// The format is FROZEN and validated rather than guessed at. See
// docs/superpowers/specs/2026-08-28-release-versioning-and-discord-updates-design.md.
package release

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Services is the closed set of component names a bullet may name. Closed on
// purpose: a typo like `nodes` would otherwise mean "this entry applies to a
// component nobody runs", which reads as an empty release rather than as an
// error.
//
// Closed also means INCOMPLETE is a defect rather than a policy. beam-relay was
// missing while being its own image, its own build job and its own thing to
// restart, so a security fix that only reached the relay could not be addressed
// to the people running it - the entry had to either lie by naming `edge` or
// name nothing. Appending is safe: the panel skips a component with no
// instances that no release names.
// spine is a gateway component like edge/link/hub/warp/beam-relay, and is here
// for the same reason they are: this list is what the gateway's notes validate
// against too. It is the one entry that is NOT an image - it ships as a systemd
// binary - which changes nothing about naming it, since the tag answers "you
// must update THIS to get the change" rather than "restart this container".
//
// "panel" is deliberately absent. It is compiled into Core's binary, so it has
// no version of its own to be behind - naming it in a release would produce a
// component the panel shows as outdated forever, because nothing builds a panel
// image any more. Say `core` instead; that is what the reader updates.
var Services = []string{"core", "node", "log-shipper", "edge", "link", "hub", "warp", "beam-relay", "spine", "voice"}

// Retired names a service that USED to be its own component. Releases in the
// history legitimately name these - they were true when written - so the parser
// must keep accepting them or the whole file stops validating the day a
// component is folded into another. They just produce no component row.
//
// "panel" was folded into Core: it is compiled into the binary now, so it has no
// version of its own and nothing builds an image for it.
var Retired = []string{"panel"}

// knownService accepts anything a release may legally name. Services drives what
// is REPORTED; this also lets the retired names through, which is the difference
// between "can be written" and "can be behind".
func knownService(s string) bool {
	return slices.Contains(Services, s) || slices.Contains(Retired, s)
}

// Version is a CalVer release version: YYYY.MM.DD with an optional same-day
// counter, e.g. 2026.08.28 and 2026.08.28.2.
//
// It is a struct rather than a string because the ordering question ("is this
// release newer than the build I am running") is asked on every panel load, and
// string comparison gets 2026.08.28.10 vs 2026.08.28.2 wrong.
type Version struct {
	parts [4]int
	raw   string
}

var versionRe = regexp.MustCompile(`^(\d{4})\.(\d{2})\.(\d{2})(?:\.(\d+))?$`)

// ParseVersion parses a CalVer version. The zero Version means "unknown", which
// is what an image built before this mechanism reports; callers must treat it
// as "cannot tell", never as "very old" - see IsZero.
func ParseVersion(s string) (Version, error) {
	s = strings.TrimSpace(s)
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("release: %q is not a version (want YYYY.MM.DD or YYYY.MM.DD.N)", s)
	}
	var v Version
	v.raw = s
	for i := 1; i <= 3; i++ {
		v.parts[i-1], _ = strconv.Atoi(m[i])
	}
	if m[4] != "" {
		n, err := strconv.Atoi(m[4])
		if err != nil || n < 1 {
			return Version{}, fmt.Errorf("release: %q has an invalid same-day counter", s)
		}
		v.parts[3] = n
	}
	if v.parts[1] < 1 || v.parts[1] > 12 || v.parts[2] < 1 || v.parts[2] > 31 {
		return Version{}, fmt.Errorf("release: %q is not a real date", s)
	}
	return v, nil
}

// IsZero reports whether this is the unknown version. Every image built before
// release stamping carries no version, and there are still such images running,
// so "unknown" must never be ordered against a real version.
func (v Version) IsZero() bool { return v.raw == "" }

func (v Version) String() string { return v.raw }

// Compare returns -1, 0 or 1. Comparing against an unknown version is
// meaningless; callers check IsZero first.
func (v Version) Compare(o Version) int {
	for i := range v.parts {
		switch {
		case v.parts[i] < o.parts[i]:
			return -1
		case v.parts[i] > o.parts[i]:
			return 1
		}
	}
	return 0
}

func (v Version) MarshalJSON() ([]byte, error) { return json.Marshal(v.raw) }

func (v *Version) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*v = Version{}
		return nil
	}
	p, err := ParseVersion(s)
	if err != nil {
		return err
	}
	*v = p
	return nil
}

// Entry is one bullet: what changed, and which components have to be updated
// for it. Services may be empty - a route-only customer runs nothing, so an
// entry addressed to them names no component and is purely informational.
type Entry struct {
	Text     string   `json:"text"`
	Services []string `json:"services,omitempty"`
}

// Required is the mandatory-update declaration on a release. Deadline is the
// instant after which an out-of-date component is refused, so a date-only
// declaration resolves to the END of that day: "update by the 5th" means the
// 5th is still fine.
type Required struct {
	Deadline  time.Time `json:"deadline"`
	Immediate bool      `json:"immediate"`
	Note      string    `json:"note,omitempty"`
}

// Release is one `## <version>` block.
type Release struct {
	Version  Version   `json:"version"`
	Required *Required `json:"required,omitempty"`
	Features []Entry   `json:"features"`
	Breaking []Entry   `json:"breaking"`
	Security []Entry   `json:"security"`
	Fixes    []Entry   `json:"fixes"`
}

// sections is the frozen order. All four are required in every block, even when
// empty, so a reader can tell "no security fixes this time" from "nobody filled
// this in".
var sections = []string{"Features", "Breaking", "Security", "Fixes"}

// Entries returns the block's bullets for one section name, so callers can walk
// the four categories without repeating the field list.
func (r *Release) Entries(section string) []Entry {
	switch section {
	case "Features":
		return r.Features
	case "Breaking":
		return r.Breaking
	case "Security":
		return r.Security
	case "Fixes":
		return r.Fixes
	}
	return nil
}

func (r *Release) set(section string, entries []Entry) {
	switch section {
	case "Features":
		r.Features = entries
	case "Breaking":
		r.Breaking = entries
	case "Security":
		r.Security = entries
	case "Fixes":
		r.Fixes = entries
	}
}

// Names reports whether any bullet in this release names the given service.
func (r *Release) Names(service string) bool {
	for _, s := range sections {
		for _, e := range r.Entries(s) {
			if slices.Contains(e.Services, service) {
				return true
			}
		}
	}
	return false
}

// AllServices returns every service named anywhere in the release, in the order
// of Services so the result is stable. CI uses it to check that everything the
// release claims changed was actually rebuilt.
func (r *Release) AllServices() []string {
	var out []string
	for _, k := range Services {
		if r.Names(k) {
			out = append(out, k)
		}
	}
	return out
}

var (
	requiredRe = regexp.MustCompile(`^\*\*Update required (now|by ([^*]+?))\s*\*\*\s*(?:[-\x{2014}]\s*(.*))?$`)
	// A trailing run of inline-code tokens is the service list. Anchored to the
	// END so a backticked term inside the prose stays prose.
	trailingCodeRe = regexp.MustCompile("(?:\\s*`[^`]+`)+$")
	codeTokenRe    = regexp.MustCompile("`([^`]+)`")
)

// Parse reads a release-notes file. Releases come back newest first, in file
// order, and the parser enforces that this is also strictly-descending version
// order - a block inserted in the wrong place is a mistake worth failing on,
// because the top block is what gets announced.
func Parse(src []byte) ([]Release, error) {
	lines := strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")

	var (
		out     []Release
		cur     *Release
		section string
		entries []Entry
		// Which of the four sections this block has seen, to catch a missing or
		// duplicated one.
		seen int
		// The bullet being read, joined across indented continuation lines, and
		// the line it started on so an error points at the bullet rather than at
		// its last fragment.
		pending     string
		pendingOpen bool
		pendingLine int
	)

	flushBullet := func() error {
		if !pendingOpen {
			return nil
		}
		text := pending
		pending, pendingOpen = "", false
		e, empty, err := parseEntry(text)
		if err != nil {
			return fmt.Errorf("line %d: %w", pendingLine, err)
		}
		if !empty {
			entries = append(entries, e)
		}
		return nil
	}
	flushSection := func() error {
		if err := flushBullet(); err != nil {
			return err
		}
		if cur != nil && section != "" {
			cur.set(section, entries)
		}
		section, entries = "", nil
		return nil
	}
	flushRelease := func(atLine int) error {
		if cur == nil {
			return nil
		}
		if err := flushSection(); err != nil {
			return err
		}
		if seen != len(sections) {
			return fmt.Errorf("line %d: release %s has %d of the %d required sections (%s)",
				atLine, cur.Version, seen, len(sections), strings.Join(sections, ", "))
		}
		out = append(out, *cur)
		cur, seen = nil, 0
		return nil
	}

	for i, raw := range lines {
		ln := i + 1
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "<!--") {
			continue
		}

		// An INDENTED line continues the bullet above it, which is how markdown
		// spells a wrapped list item. Requiring the indent is what keeps the
		// "prose where a bullet was meant" check below working: an unindented
		// sentence is still an error, so a line that was supposed to be its own
		// entry cannot silently attach itself to the previous one.
		if pendingOpen && raw != line && strings.HasPrefix(raw, " ") {
			pending += " " + line
			continue
		}

		switch {
		case strings.HasPrefix(line, "## "):
			if err := flushRelease(ln); err != nil {
				return nil, err
			}
			v, err := ParseVersion(strings.TrimPrefix(line, "## "))
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", ln, err)
			}
			if n := len(out); n > 0 && out[n-1].Version.Compare(v) <= 0 {
				return nil, fmt.Errorf("line %d: release %s is not older than %s above it; newest goes first",
					ln, v, out[n-1].Version)
			}
			cur = &Release{Version: v}

		case strings.HasPrefix(line, "### "):
			if cur == nil {
				return nil, fmt.Errorf("line %d: section %q before any ## release heading", ln, line)
			}
			if err := flushSection(); err != nil {
				return nil, err
			}
			name := strings.TrimPrefix(line, "### ")
			if seen >= len(sections) || name != sections[seen] {
				want := "nothing more"
				if seen < len(sections) {
					want = sections[seen]
				}
				return nil, fmt.Errorf("line %d: release %s has section %q where %q was expected; the four sections are fixed and ordered (%s)",
					ln, cur.Version, name, want, strings.Join(sections, ", "))
			}
			section = name
			seen++

		case strings.HasPrefix(line, "**Update required"):
			if cur == nil {
				return nil, fmt.Errorf("line %d: required-update line before any ## release heading", ln)
			}
			if section != "" {
				return nil, fmt.Errorf("line %d: the required-update line belongs directly under the version heading, not inside a section", ln)
			}
			req, err := parseRequired(line, cur.Version)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", ln, err)
			}
			cur.Required = req

		case strings.HasPrefix(line, "- "):
			if section == "" {
				if cur == nil {
					continue // preamble prose before the first release
				}
				return nil, fmt.Errorf("line %d: bullet outside any ### section", ln)
			}
			if err := flushBullet(); err != nil {
				return nil, err
			}
			pending, pendingOpen, pendingLine = strings.TrimPrefix(line, "- "), true, ln

		case strings.HasPrefix(line, "# "):
			// File title. Only legal before the first release block.
			if cur != nil {
				return nil, fmt.Errorf("line %d: a top-level heading inside release %s", ln, cur.Version)
			}

		default:
			// Free prose is allowed in the preamble and nowhere else, so a line
			// that was meant to be a bullet cannot vanish silently.
			if cur != nil {
				return nil, fmt.Errorf("line %d: unexpected line %q inside release %s; entries are bullets starting with \"- \"",
					ln, line, cur.Version)
			}
		}
	}
	if err := flushRelease(len(lines)); err != nil {
		return nil, err
	}
	return out, nil
}

// parseEntry splits a bullet into its text and its trailing service list. The
// second return is true for a placeholder ("Nothing."), which records an
// intentionally empty category and produces no entry.
func parseEntry(s string) (Entry, bool, error) {
	s = strings.TrimSpace(s)
	if t := strings.TrimRight(strings.ToLower(s), "."); t == "nothing" || t == "none" {
		return Entry{}, true, nil
	}
	var services []string
	if loc := trailingCodeRe.FindStringIndex(s); loc != nil {
		for _, m := range codeTokenRe.FindAllStringSubmatch(s[loc[0]:], -1) {
			name := strings.TrimSpace(m[1])
			if !knownService(name) {
				return Entry{}, false, fmt.Errorf("unknown service %q (known: %s)", name, strings.Join(append(append([]string{}, Services...), Retired...), ", "))
			}
			services = append(services, name)
		}
		s = strings.TrimSpace(s[:loc[0]])
	}
	if s == "" {
		return Entry{}, false, fmt.Errorf("entry has services but no text")
	}
	return Entry{Text: s, Services: services}, false, nil
}

// parseRequired reads the mandatory-update line. A date without a time resolves
// to the END of that day in UTC, because "update by the 5th" means the 5th is
// still fine.
func parseRequired(line string, v Version) (*Required, error) {
	m := requiredRe.FindStringSubmatch(line)
	if m == nil {
		return nil, fmt.Errorf("cannot read %q; write **Update required now** or **Update required by YYYY-MM-DD [HH:MM UTC]**, optionally followed by \"- reason\"", line)
	}
	req := &Required{Note: strings.TrimSpace(m[3])}
	if m[1] == "now" {
		req.Immediate = true
		// A release version is a date, so "now" is anchored to the release day
		// rather than to whenever this file happens to be parsed. Parsing must
		// give the same answer in CI and in Core a week later.
		req.Deadline = time.Date(v.parts[0], time.Month(v.parts[1]), v.parts[2], 0, 0, 0, 0, time.UTC)
		return req, nil
	}
	when := strings.TrimSpace(m[2])
	if t, err := time.Parse("2006-01-02 15:04 MST", when); err == nil {
		req.Deadline = t.UTC()
		return req, nil
	}
	if t, err := time.Parse("2006-01-02", when); err == nil {
		req.Deadline = t.Add(24*time.Hour - time.Second)
		return req, nil
	}
	return nil, fmt.Errorf("cannot read the deadline %q; write YYYY-MM-DD or YYYY-MM-DD HH:MM UTC", when)
}

// Latest returns the newest release, or false for an empty file.
func Latest(rs []Release) (Release, bool) {
	if len(rs) == 0 {
		return Release{}, false
	}
	return rs[0], true
}

// Outdated answers the question the panel asks per component: is there a
// release NEWER than what this component runs that names it.
//
// This is the whole reason one version per repo does not produce false
// "update available". A release naming only `node` leaves Core green even
// though Core's stamped version is older than that release.
//
// An unknown version is never outdated: it cannot be ordered, and reporting
// "behind" for every pre-stamping image would be noise, not information.
func Outdated(rs []Release, service string, cur Version) bool {
	if cur.IsZero() {
		return false
	}
	for _, r := range rs {
		if r.Version.Compare(cur) > 0 && r.Names(service) {
			return true
		}
	}
	return false
}

// Requirement returns the mandatory-update requirement a component is subject
// to, or nil. Only the NEWEST applicable one matters: it carries the latest
// deadline and the highest floor, so an older requirement adds nothing.
//
// MinVersion is the version the component must reach to satisfy it, which is
// the release that declared it.
func Requirement(rs []Release, service string, cur Version) (req *Required, minVersion Version, ok bool) {
	if cur.IsZero() {
		return nil, Version{}, false
	}
	for _, r := range rs {
		if r.Required == nil || !r.Names(service) {
			continue
		}
		if r.Version.Compare(cur) <= 0 {
			// This component already satisfies the newest requirement, and
			// anything below it is older still.
			return nil, Version{}, false
		}
		return r.Required, r.Version, true
	}
	return nil, Version{}, false
}
