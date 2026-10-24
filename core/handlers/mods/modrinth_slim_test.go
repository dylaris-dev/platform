package mods

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The fixtures are real captured Modrinth responses (Sodium on fabric for
// 1.21.1, and one of its versions), not hand-written shapes. A hand-written
// fixture would only prove the trim works on the document I imagined.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

func TestStripVersionChangelogsOnAList(t *testing.T) {
	raw := readFixture(t, "modrinth_versions.json")
	out := stripVersionChangelogs(raw)

	if len(out) >= len(raw) {
		t.Errorf("stripped payload is %d B, was %d B: nothing was removed", len(out), len(raw))
	}

	var before, after []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}
	if err := json.Unmarshal(out, &after); err != nil {
		t.Fatalf("stripped payload does not parse: %v", err)
	}

	// Every build survives. Dropping older builds is the thing this deliberately
	// does NOT do: the version pickers exist to install something other than the
	// newest.
	if len(after) != len(before) {
		t.Fatalf("got %d versions, want all %d", len(after), len(before))
	}
	if len(before) < 2 {
		t.Fatalf("fixture has %d versions; it cannot show that builds are kept", len(before))
	}

	for i := range before {
		if _, ok := after[i]["changelog"]; ok {
			t.Errorf("version %d still carries a changelog", i)
		}
		// Every other field byte for byte: the trim works on RawMessage
		// precisely so nothing is re-encoded on the way through.
		for k, v := range before[i] {
			if k == "changelog" {
				continue
			}
			got, ok := after[i][k]
			if !ok {
				t.Errorf("version %d lost field %q", i, k)
				continue
			}
			if string(got) != string(v) {
				t.Errorf("version %d field %q changed:\n before %s\n after  %s", i, k, v, got)
			}
		}
	}
}

func TestStripVersionChangelogsOnASingleVersion(t *testing.T) {
	raw := readFixture(t, "modrinth_version.json")
	out := stripVersionChangelogs(raw)

	var after map[string]json.RawMessage
	if err := json.Unmarshal(out, &after); err != nil {
		t.Fatalf("stripped payload does not parse: %v", err)
	}
	if _, ok := after["changelog"]; ok {
		t.Error("the single version still carries a changelog")
	}
	// The fields the install path reads must be there, or a mod install would
	// have nothing to download.
	for _, k := range []string{"id", "project_id", "version_number", "game_versions", "loaders", "files", "date_published"} {
		if _, ok := after[k]; !ok {
			t.Errorf("lost field %q, which the install path reads", k)
		}
	}
}

func TestStripVersionChangelogsLeavesUnparseableInputAlone(t *testing.T) {
	// It is a size optimisation. Failing it must never become failing the
	// request, so anything it cannot read passes through untouched.
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"not json", "upstream exploded"},
		{"a json string", `"just a string"`},
		{"truncated array", `[{"id":"a",`},
		{"an object with no changelog", `{"id":"a","files":[]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(stripVersionChangelogs([]byte(tc.in))); got != tc.in {
				t.Errorf("got %q, want it unchanged", got)
			}
		})
	}
}

// The whole point is the size, so pin that it is a large win rather than a
// rounding error: if a future Modrinth response stops carrying changelogs, this
// trim has no reason to exist any more and should be reconsidered.
func TestStripVersionChangelogsSavesMostOfThePayload(t *testing.T) {
	raw := readFixture(t, "modrinth_versions.json")
	out := stripVersionChangelogs(raw)
	saved := 100 * (len(raw) - len(out)) / len(raw)
	t.Logf("%d B -> %d B (%d%% saved)", len(raw), len(out), saved)
	if saved < 25 {
		t.Errorf("only %d%% saved; the changelog was two thirds of the measured payload", saved)
	}
}
