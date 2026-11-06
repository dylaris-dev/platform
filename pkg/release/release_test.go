package release

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func mustVersion(t *testing.T, s string) Version {
	t.Helper()
	v, err := ParseVersion(s)
	if err != nil {
		t.Fatalf("ParseVersion(%q): %v", s, err)
	}
	return v
}

func TestParseVersion(t *testing.T) {
	for _, s := range []string{"2026.08.28", "2026.08.28.2", "2026.12.31.10", "2026.01.01"} {
		if _, err := ParseVersion(s); err != nil {
			t.Errorf("ParseVersion(%q) rejected a valid version: %v", s, err)
		}
	}
	for _, s := range []string{"", "2026.8.28", "v2026.08.28", "2026.08.28.0", "2026.13.01", "2026.08.32", "1.2.3"} {
		if _, err := ParseVersion(s); err == nil {
			t.Errorf("ParseVersion(%q) accepted an invalid version", s)
		}
	}
}

// The reason Version is not a string. Ten same-day releases sort after two, and
// a string comparison gets that backwards.
func TestVersionCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2026.08.28", "2026.08.28", 0},
		{"2026.08.28", "2026.08.27", 1},
		{"2026.08.28", "2026.09.01", -1},
		{"2025.12.31", "2026.01.01", -1},
		{"2026.08.28.2", "2026.08.28", 1},
		{"2026.08.28.10", "2026.08.28.2", 1},
	}
	for _, c := range cases {
		if got := mustVersion(t, c.a).Compare(mustVersion(t, c.b)); got != c.want {
			t.Errorf("%s vs %s = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// An image built before release stamping reports nothing. It must read as
// "cannot tell", never as "extremely old" - the whole installed base is in that
// state on the day this ships.
func TestUnknownVersionIsNeverOrdered(t *testing.T) {
	var unknown Version
	if !unknown.IsZero() {
		t.Fatal("the zero Version must report IsZero")
	}
	rs := []Release{{Version: mustVersion(t, "2026.08.28"), Fixes: []Entry{{Text: "x", Services: []string{"node"}}}}}
	if Outdated(rs, "node", unknown) {
		t.Error("an unstamped image was reported as outdated; it would flag every pre-stamping deployment")
	}
	if _, _, ok := Requirement(rs, "node", unknown); ok {
		t.Error("an unstamped image was made subject to a mandatory update")
	}
}

const goodFile = "# Platform updates\n" +
	"\n" +
	"Newest first.\n" +
	"\n" +
	"## 2026.08.28\n" +
	"\n" +
	"**Update required by 2026-09-05 14:00 UTC** - older nodes stop connecting.\n" +
	"\n" +
	"### Features\n" +
	"- Limits read the same way everywhere. `core` `panel`\n" +
	"\n" +
	"### Breaking\n" +
	"- Nothing.\n" +
	"\n" +
	"### Security\n" +
	"- STARTTLS is now required. `core`\n" +
	"\n" +
	"### Fixes\n" +
	"- A route allowance of zero now means none. `node`\n" +
	"- Something for route-only customers with nothing to update.\n" +
	"\n" +
	"## 2026.08.20\n" +
	"\n" +
	"### Features\n" +
	"- Nothing.\n" +
	"### Breaking\n" +
	"- Nothing.\n" +
	"### Security\n" +
	"- Nothing.\n" +
	"### Fixes\n" +
	"- An older fix. `panel`\n"

func TestParseGoodFile(t *testing.T) {
	rs, err := Parse([]byte(goodFile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(rs) != 2 {
		t.Fatalf("got %d releases, want 2", len(rs))
	}
	top := rs[0]
	if top.Version.String() != "2026.08.28" {
		t.Errorf("top version = %s", top.Version)
	}
	if len(top.Features) != 1 || top.Features[0].Text != "Limits read the same way everywhere." {
		t.Errorf("features = %+v", top.Features)
	}
	if got := top.Features[0].Services; len(got) != 2 || got[0] != "core" || got[1] != "panel" {
		t.Errorf("services = %v, want [core panel]", got)
	}
	// "Nothing." records an intentionally empty category and yields no entry.
	if len(top.Breaking) != 0 {
		t.Errorf("breaking = %+v, want empty", top.Breaking)
	}
	// A bullet with no services is legal: route-only customers run nothing.
	if len(top.Fixes) != 2 || len(top.Fixes[1].Services) != 0 {
		t.Errorf("fixes = %+v", top.Fixes)
	}
	if top.Required == nil {
		t.Fatal("required line was not parsed")
	}
	want := time.Date(2026, 9, 5, 14, 0, 0, 0, time.UTC)
	if !top.Required.Deadline.Equal(want) {
		t.Errorf("deadline = %v, want %v", top.Required.Deadline, want)
	}
	if top.Required.Note != "older nodes stop connecting." {
		t.Errorf("note = %q", top.Required.Note)
	}
	// "panel" is NOT in the result even though the fixture names it. CI uses
	// AllServices to demand that everything a release claims was rebuilt, and
	// nothing builds a panel image any more - it is compiled into Core. A
	// retired name stays PARSEABLE, so the history keeps validating, but it can
	// no longer put a build on the critical path.
	if got := top.AllServices(); strings.Join(got, ",") != "core,node" {
		t.Errorf("AllServices = %v, want core,node (panel is retired)", got)
	}
}

// A date with no time means the whole day is still fine.
func TestRequiredDateOnlyEndsTheDay(t *testing.T) {
	src := "## 2026.08.28\n**Update required by 2026-09-05**\n### Features\n- Nothing.\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n"
	rs, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	d := rs[0].Required.Deadline
	if d.Year() != 2026 || d.Month() != 9 || d.Day() != 5 || d.Hour() != 23 {
		t.Errorf("deadline = %v, want the end of 2026-09-05", d)
	}
}

// "now" is anchored to the release date, not to parse time, so CI and Core a
// week later agree on the same instant.
func TestRequiredNowAnchorsToTheReleaseDate(t *testing.T) {
	src := "## 2026.08.28\n**Update required now** - do it today.\n### Features\n- Nothing.\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- x `node`\n"
	rs, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	r := rs[0].Required
	if !r.Immediate {
		t.Error("Immediate not set")
	}
	if want := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC); !r.Deadline.Equal(want) {
		t.Errorf("deadline = %v, want %v", r.Deadline, want)
	}
}

// Every one of these is a mistake that would otherwise ship as a silently
// wrong announcement or an empty-looking release.
func TestParseRejects(t *testing.T) {
	cases := []struct {
		name, src, wantSubstr string
	}{
		{
			"a missing section",
			"## 2026.08.28\n### Features\n- Nothing.\n### Security\n- Nothing.\n",
			"where \"Breaking\" was expected",
		},
		{
			"sections out of order",
			"## 2026.08.28\n### Breaking\n- Nothing.\n### Features\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n",
			"where \"Features\" was expected",
		},
		{
			"a block that stops early",
			"## 2026.08.28\n### Features\n- Nothing.\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n",
			"3 of the 4 required sections",
		},
		{
			"an unknown service name",
			"## 2026.08.28\n### Features\n- x `nodes`\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n",
			"unknown service \"nodes\"",
		},
		{
			"releases in the wrong order",
			"## 2026.08.20\n### Features\n- Nothing.\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n" +
				"## 2026.08.28\n### Features\n- Nothing.\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n",
			"newest goes first",
		},
		{
			"the same version twice",
			"## 2026.08.28\n### Features\n- Nothing.\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n" +
				"## 2026.08.28\n### Features\n- Nothing.\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n",
			"newest goes first",
		},
		{
			"an unparseable version",
			"## 2026.8.28\n### Features\n- Nothing.\n",
			"is not a version",
		},
		{
			"prose where a bullet was meant",
			"## 2026.08.28\n### Features\nWe added a thing.\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n",
			"entries are bullets",
		},
		{
			"a bullet before any section",
			"## 2026.08.28\n- x\n### Features\n- Nothing.\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n",
			"bullet outside any ### section",
		},
		{
			"a malformed required line",
			"## 2026.08.28\n**Update required soon**\n### Features\n- Nothing.\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n",
			"Update required now",
		},
		{
			"a required line with an unreadable date",
			"## 2026.08.28\n**Update required by next tuesday**\n### Features\n- Nothing.\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n",
			"cannot read the deadline",
		},
		{
			"a required line buried in a section",
			"## 2026.08.28\n### Features\n**Update required now**\n- Nothing.\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n",
			"directly under the version heading",
		},
		{
			"two required lines in one release",
			"## 2026.08.28\n**Update required by 2026-09-01**\n**Update required by 2026-09-30**\n### Features\n- Nothing.\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n",
			"already has a required-update line",
		},
		{
			"a nested bullet",
			"## 2026.08.28\n### Features\n- Faster backups. `core`\n  - Node side streams now. `node`\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n",
			"nested bullets are not supported",
		},
		{
			"an indented section heading",
			"## 2026.08.28\n### Features\n- One thing. `core`\n  ### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n",
			"cannot continue the bullet above",
		},
		{
			"an indented required line",
			"## 2026.08.28\n### Features\n- One thing. `core`\n  **Update required now**\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n",
			"cannot continue the bullet above",
		},
		{
			"a nested star bullet",
			"## 2026.08.28\n### Features\n- Parent entry. `core`\n  * nested item `node`\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n",
			"nested bullets are not supported",
		},
		{
			"a nested numbered bullet",
			"## 2026.08.28\n### Features\n- Parent entry.\n  1. nested item\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n",
			"nested bullets are not supported",
		},
		{
			"prose continuing after the service tags",
			"## 2026.08.28\n### Features\n- Fix X `core`\n  and Y.\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n",
			"must be on the bullet's last line",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.src))
			if err == nil {
				t.Fatalf("accepted %s", c.name)
			}
			if !strings.Contains(err.Error(), c.wantSubstr) {
				t.Errorf("error %q does not mention %q", err, c.wantSubstr)
			}
		})
	}
}

// THE central property: one version per repo, and a release that does not name
// your component leaves you alone. Without this, every release would mark every
// component outdated and the whole display would be noise.
func TestOutdatedIsPerServiceNotPerVersion(t *testing.T) {
	rs, err := Parse([]byte(goodFile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	old := mustVersion(t, "2026.08.20")

	if !Outdated(rs, "core", old) {
		t.Error("core is on 2026.08.20 and 2026.08.28 names core, but reads as current")
	}
	if !Outdated(rs, "node", old) {
		t.Error("node is on 2026.08.20 and 2026.08.28 names node, but reads as current")
	}
	// log-shipper is named by NO release. Its version being older than the
	// newest release must not make it orange.
	if Outdated(rs, "log-shipper", old) {
		t.Error("log-shipper was flagged by a release that never names it - the false-positive this design exists to avoid")
	}
	// Up to date: no release newer than the component's own version.
	if Outdated(rs, "core", mustVersion(t, "2026.08.28")) {
		t.Error("a component on the newest release reads as outdated")
	}
	// Newer than everything published (a local build) is not outdated either.
	if Outdated(rs, "core", mustVersion(t, "2026.09.01")) {
		t.Error("a component ahead of the newest release reads as outdated")
	}
}

// The colour rule the panel renders, stated as the case that is easy to get
// wrong: a component named by an OLDER release, current at that release, with a
// NEWER release that names somebody else. It must stay green.
//
// The existing per-service test uses log-shipper, which no release ever names.
// That is the easy half. This is the half where the component does appear in
// the file, so a check written against "is there a newer release" rather than
// "is there a newer release NAMING me" would go orange here and nowhere else.
func TestOutdatedIgnoresAReleaseThatNamesSomebodyElse(t *testing.T) {
	const file = "# Platform updates\n" +
		"\n" +
		"## 2026.09.01\n" +
		"\n" +
		"### Features\n" +
		"- Only the node changed. `node`\n" +
		"### Breaking\n" +
		"- Nothing.\n" +
		"### Security\n" +
		"- Nothing.\n" +
		"### Fixes\n" +
		"- Nothing.\n" +
		"\n" +
		"## 2026.08.28\n" +
		"\n" +
		"### Features\n" +
		"- The panel changed here, and not since. `panel`\n" +
		"### Breaking\n" +
		"- Nothing.\n" +
		"### Security\n" +
		"- Nothing.\n" +
		"### Fixes\n" +
		"- Nothing.\n"

	rs, err := Parse([]byte(file))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	panelAt := mustVersion(t, "2026.08.28")

	if Outdated(rs, "panel", panelAt) {
		t.Error("the panel is on the newest release that names it and still reads as outdated")
	}
	if !Outdated(rs, "node", panelAt) {
		t.Error("the node is behind a release that does name it and reads as current")
	}
}

func TestRequirement(t *testing.T) {
	rs, err := Parse([]byte(goodFile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	req, min, ok := Requirement(rs, "node", mustVersion(t, "2026.08.20"))
	if !ok {
		t.Fatal("node on 2026.08.20 is not subject to the requirement declared in 2026.08.28")
	}
	if min.String() != "2026.08.28" {
		t.Errorf("min version = %s, want the release that declared it", min)
	}
	if req.Note == "" {
		t.Error("the reason was dropped")
	}
	// Satisfied: the component is already at or beyond the declaring release.
	if _, _, ok := Requirement(rs, "node", mustVersion(t, "2026.08.28")); ok {
		t.Error("a node already on the required release is still being told to update")
	}
	// A component the requirement does not name is not subject to it.
	if _, _, ok := Requirement(rs, "log-shipper", mustVersion(t, "2026.08.20")); ok {
		t.Error("a component the release never names was made subject to its deadline")
	}
}

// Backticked prose in the middle of a sentence is prose. Only a trailing run is
// the service list, or "use the `link` sidecar" would silently address a
// component.
func TestOnlyTrailingCodeIsAServiceList(t *testing.T) {
	src := "## 2026.08.28\n### Features\n- Set `REDIS_ADDR` before starting. `node`\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n"
	rs, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	e := rs[0].Features[0]
	if e.Text != "Set `REDIS_ADDR` before starting." {
		t.Errorf("text = %q, the inline term was eaten", e.Text)
	}
	if len(e.Services) != 1 || e.Services[0] != "node" {
		t.Errorf("services = %v, want [node]", e.Services)
	}
}

// The gateway ships four images an operator restarts independently, and the
// beam relay is one of them. It was absent from Services, so an entry about a
// relay-only fix could not address the people running a relay: naming it was a
// parse error, and the alternatives were to name `edge` (a different image) or
// to name nothing. The closed set has to be complete to be closed.
func TestServicesCoversEveryIndependentlyDeployedComponent(t *testing.T) {
	for _, svc := range Services {
		src := "## 2026.08.28\n### Features\n- x. `" + svc + "`\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n"
		rs, err := Parse([]byte(src))
		if err != nil {
			t.Errorf("a bullet naming %q must parse: %v", svc, err)
			continue
		}
		if got := rs[0].Features[0].Services; len(got) != 1 || got[0] != svc {
			t.Errorf("services for %q = %v", svc, got)
		}
	}
	if !slices.Contains(Services, "beam-relay") {
		t.Error("beam-relay left the set; a relay-only entry can then only name the wrong image or nothing")
	}
	// Still closed: an unknown name is an error, not a silently empty audience.
	bad := "## 2026.08.28\n### Features\n- x. `relay`\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n"
	if _, err := Parse([]byte(bad)); err == nil {
		t.Error("an unknown service name parsed; the set is no longer closed")
	}
}

func TestParseEmptyFile(t *testing.T) {
	rs, err := Parse([]byte("# Platform updates\n\nNothing released yet.\n"))
	if err != nil {
		t.Fatalf("a file with no releases must parse: %v", err)
	}
	if len(rs) != 0 {
		t.Errorf("got %d releases", len(rs))
	}
	if _, ok := Latest(rs); ok {
		t.Error("Latest reported a release in an empty file")
	}
}

// A long entry wraps. Requiring the continuation to be INDENTED is what keeps
// the "prose where a bullet was meant" guard alive: an unindented sentence is
// still an error rather than silently joining the entry above it.
func TestWrappedBullets(t *testing.T) {
	src := "## 2026.08.28\n" +
		"### Features\n" +
		"- Updates now have a version, and the panel shows which of your\n" +
		"  components are behind rather than a count of changelog lines.\n" +
		"  `core` `panel`\n" +
		"- A second entry. `node`\n" +
		"- Code that wraps, `.disabled/`\n  and back. `core`\n" +
		"- Tags that wrap. `core`\n  `node`\n" +
		"- HTTP answers 404 instead of\n  200. Nothing else. `core`\n" +
		"### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n"
	rs, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	f := rs[0].Features
	if len(f) != 5 {
		t.Fatalf("got %d features, want 5 - a continuation line was read as its own entry", len(f))
	}
	want := "Updates now have a version, and the panel shows which of your components are behind rather than a count of changelog lines."
	if f[0].Text != want {
		t.Errorf("text = %q\nwant %q", f[0].Text, want)
	}
	// Services on the continuation line still belong to the entry.
	if got := f[0].Services; len(got) != 2 || got[0] != "core" || got[1] != "panel" {
		t.Errorf("services = %v, want [core panel]", got)
	}
	if f[1].Text != "A second entry." {
		t.Errorf("second entry = %q", f[1].Text)
	}
	// Wraps the J01 guard must keep allowing: prose code mid-sentence, a tag
	// list split over two lines, and a wrapped line that starts with a number.
	for i, w := range []struct {
		text     string
		services string
	}{
		{"Code that wraps, `.disabled/` and back.", "core"},
		{"Tags that wrap.", "core node"},
		{"HTTP answers 404 instead of 200. Nothing else.", "core"},
	} {
		e := f[2+i]
		if e.Text != w.text || strings.Join(e.Services, " ") != w.services {
			t.Errorf("entry %d = %q %v, want %q [%s]", 2+i, e.Text, e.Services, w.text, w.services)
		}
	}
}

// The guard that makes the indent rule worth having.
func TestUnindentedProseAfterABulletIsStillAnError(t *testing.T) {
	src := "## 2026.08.28\n### Features\n- One thing. `core`\nThis line was meant to be a bullet.\n" +
		"### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n"
	if _, err := Parse([]byte(src)); err == nil {
		t.Fatal("unindented prose after a bullet was accepted; it would vanish from the release")
	}
}

// Stacked requirements: the newest sets the floor, but the EARLIEST deadline
// among those the component still fails is the one that binds. Otherwise a
// later, more lenient release would re-open a deadline that already passed.
func TestRequirementTakesTheEarliestDeadlineItStillFails(t *testing.T) {
	block := func(v, req string) string {
		return "## " + v + "\n" + req + "\n### Features\n- Nothing.\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- x `node`\n"
	}
	cases := []struct {
		name, src, cur, wantMin string
		wantDeadline            time.Time
		wantImmediate           bool
	}{
		{
			"an older overdue deadline is not masked by a newer later one",
			block("2026.11.20", "**Update required by 2026-12-01**") + block("2026.11.10", "**Update required by 2026-11-12 10:00 UTC** - old nodes break"),
			"2026.11.01", "2026.11.20",
			time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC), false,
		},
		{
			"an older 'now' binds too",
			block("2026.11.20", "**Update required by 2026-12-01**") + block("2026.11.10", "**Update required now**"),
			"2026.11.01", "2026.11.20",
			time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC), true,
		},
		{
			"a requirement already satisfied does not count",
			block("2026.11.20", "**Update required by 2026-12-01**") + block("2026.11.10", "**Update required by 2026-11-12**"),
			"2026.11.10", "2026.11.20",
			time.Date(2026, 12, 1, 23, 59, 59, 0, time.UTC), false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rs, err := Parse([]byte(c.src))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			req, min, ok := Requirement(rs, "node", mustVersion(t, c.cur))
			if !ok {
				t.Fatal("no requirement")
			}
			if min.String() != c.wantMin {
				t.Errorf("min = %s, want %s", min, c.wantMin)
			}
			if !req.Deadline.Equal(c.wantDeadline) {
				t.Errorf("deadline = %v, want %v", req.Deadline, c.wantDeadline)
			}
			if req.Immediate != c.wantImmediate {
				t.Errorf("immediate = %v, want %v", req.Immediate, c.wantImmediate)
			}
		})
	}
}

// The frozen format allows HH:MM UTC only. Any other abbreviation used to parse
// with a ZERO offset when the machine did not know it, so the enforced instant
// silently differed from the one the author wrote.
func TestRequiredDeadlineMustBeUTC(t *testing.T) {
	for _, zone := range []string{"PST", "CET", "CEST", "XYZ", "MST"} {
		t.Run(zone, func(t *testing.T) {
			src := "## 2026.08.28\n**Update required by 2026-09-05 14:00 " + zone + "**\n### Features\n- Nothing.\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n"
			if _, err := Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), "cannot read the deadline") {
				t.Errorf("err = %v, want a deadline error", err)
			}
		})
	}
	src := "## 2026.08.28\n**Update required by 2026-09-05 14:00 UTC**\n### Features\n- Nothing.\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n"
	rs, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := time.Date(2026, 9, 5, 14, 0, 0, 0, time.UTC); !rs[0].Required.Deadline.Equal(want) {
		t.Errorf("deadline = %v, want %v", rs[0].Required.Deadline, want)
	}
}

// The connect gate and the panel's Required line read the two notes files as
// ONE set. A node requirement written into only one file must still bind, with
// the earliest deadline across both, exactly as within one file.
func TestRequirementAcrossMergedFiles(t *testing.T) {
	block := func(v, req, svc string) string {
		return "## " + v + "\n" + req + "\n### Features\n- x `" + svc + "`\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n"
	}
	parse := func(src string) []Release {
		rs, err := Parse([]byte(src))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		return rs
	}
	platform := parse(block("2026.11.20", "**Update required by 2026-12-01**", "node") + block("2026.11.05", "", "core"))
	hosted := parse(block("2026.11.10", "**Update required by 2026-11-12**", "node"))
	cases := []struct {
		name         string
		sets         [][]Release
		cur, wantMin string
		wantDeadline time.Time
	}{
		{"only in hosted", [][]Release{nil, hosted}, "2026.11.01", "2026.11.10", time.Date(2026, 11, 12, 23, 59, 59, 0, time.UTC)},
		{"only in platform", [][]Release{platform, nil}, "2026.11.01", "2026.11.20", time.Date(2026, 12, 1, 23, 59, 59, 0, time.UTC)},
		{"both: newest floor, earliest deadline", [][]Release{platform, hosted}, "2026.11.01", "2026.11.20", time.Date(2026, 11, 12, 23, 59, 59, 0, time.UTC)},
		{"both: hosted already satisfied", [][]Release{platform, hosted}, "2026.11.10", "2026.11.20", time.Date(2026, 12, 1, 23, 59, 59, 0, time.UTC)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			merged := Merge(c.sets...)
			for i := 1; i < len(merged); i++ {
				if merged[i-1].Version.Compare(merged[i].Version) < 0 {
					t.Fatalf("merged not newest first: %s before %s", merged[i-1].Version, merged[i].Version)
				}
			}
			req, min, ok := Requirement(merged, "node", mustVersion(t, c.cur))
			if !ok {
				t.Fatal("no requirement")
			}
			if min.String() != c.wantMin {
				t.Errorf("min = %s, want %s", min, c.wantMin)
			}
			if !req.Deadline.Equal(c.wantDeadline) {
				t.Errorf("deadline = %v, want %v", req.Deadline, c.wantDeadline)
			}
		})
	}
}
