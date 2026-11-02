package health

import "testing"

// TestParsePaperMCVersions covers the v3 Fill shape migration: the `versions`
// object is keyed by major line -> build list, pre-release/RC/snapshot builds
// (anything with a "-") are dropped, and each surviving build gets its derived
// major.
func TestParsePaperMCVersions(t *testing.T) {
	resp := map[string]interface{}{
		"versions": map[string]interface{}{
			"1.21":  []interface{}{"1.21.4", "1.21.4-rc1", "1.21"},
			"1.20":  []interface{}{"1.20.6", ""},
			"4.0.0": []interface{}{"3.5.1", "3.6.0-SNAPSHOT"},
		},
	}
	entries, err := parsePaperMCVersions(resp, "paper")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := map[string]string{}
	for _, e := range entries {
		got[e.Build] = e.Major
	}

	// Stable builds survive; the -rc1, -SNAPSHOT and empty entries are dropped.
	want := map[string]string{
		"1.21.4": "1.21",
		"1.21":   "1.21",
		"1.20.6": "1.20",
		"3.5.1":  "3.5",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d builds %v, want %d %v", len(got), got, len(want), want)
	}
	for build, major := range want {
		if got[build] != major {
			t.Errorf("build %q major = %q, want %q", build, got[build], major)
		}
	}
	for _, dropped := range []string{"1.21.4-rc1", "3.6.0-SNAPSHOT", ""} {
		if _, ok := got[dropped]; ok {
			t.Errorf("non-stable build %q should have been filtered out", dropped)
		}
	}
}

// TestParsePaperMCVersions_SunsetBodyErrors asserts the retired v2 endpoint's
// {"ok":false,"error":"sunset"} body (which fetchJSON still parses as JSON)
// surfaces as an error rather than an empty list, so the handler reports a
// failure instead of silently returning zero versions.
func TestParsePaperMCVersions_SunsetBodyErrors(t *testing.T) {
	if _, err := parsePaperMCVersions(map[string]interface{}{"ok": false, "error": "sunset"}, "paper"); err == nil {
		t.Fatal("expected an error when the versions object is absent")
	}
}

func TestSplitDot(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"1.21.4", []string{"1", "21", "4"}},
		{"1.21", []string{"1", "21"}},
		{"single", []string{"single"}},
		{"", []string{""}},
	}
	for _, c := range cases {
		got := splitDot(c.input)
		if len(got) != len(c.want) {
			t.Errorf("splitDot(%q) len=%d, want %d", c.input, len(got), len(c.want))
			continue
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Errorf("splitDot(%q)[%d] = %q, want %q", c.input, i, got[i], c.want[i])
			}
		}
	}
}

func TestGetMajorVersion(t *testing.T) {
	cases := []struct{ input, want string }{
		{"1.21.4", "1.21"},
		{"1.21", "1.21"},
		{"1", "1"},
		{"", ""},
	}
	for _, c := range cases {
		if got := getMajorVersion(c.input); got != c.want {
			t.Errorf("getMajorVersion(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}
