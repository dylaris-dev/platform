package health

import (
	"testing"
	"time"
)

// Two releases name `node` with overlapping deadlines. A node below only the
// newer one has the later deadline; a node below both is held to the earlier
// one, and Core's connect gate refuses it once that passes. The panel line must
// show the deadline the gate is enforcing, whichever node is listed first and
// whichever node row (cluster or external) it sits in.
func TestRequiredBlocksShowTheEarliestDeadlineAcrossInstances(t *testing.T) {
	notes := "## 2026.11.20\n" +
		"**Update required by 2026-12-01** - newer.\n" +
		"### Features\n- B. `node`\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n" +
		"\n## 2026.11.10\n" +
		"**Update required by 2026-11-12** - older.\n" +
		"### Features\n- A. `node`\n### Breaking\n- Nothing.\n### Security\n- Nothing.\n### Fixes\n- Nothing.\n"
	releases := mustParse(t, notes)
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	ahead := instance{Label: "n1", Version: "2026.11.15"}
	behind := instance{Label: "n2", Version: "2026.11.01"}

	cases := []struct {
		name  string
		comps []componentBlock
	}{
		{"lenient node listed first", []componentBlock{
			{Key: "node", Service: "node", Instances: []instance{ahead, behind}}}},
		{"lenient node listed last", []componentBlock{
			{Key: "node", Service: "node", Instances: []instance{behind, ahead}}}},
		{"overdue node on the external row", []componentBlock{
			{Key: "node", Service: "node", Instances: []instance{ahead}},
			{Key: "node-external", Service: "node", Instances: []instance{behind}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := requiredBlocks(c.comps, releases, now)
			if len(got) != 1 {
				t.Fatalf("got %d blocks, want one for `node`: %+v", len(got), got)
			}
			b := got[0]
			if b.MinVersion != "2026.11.20" {
				t.Errorf("minVersion = %q, want 2026.11.20", b.MinVersion)
			}
			if b.Deadline.Format("2006-01-02") != "2026-11-12" || !b.Passed {
				t.Errorf("deadline = %v passed = %v, want 2026-11-12 and passed", b.Deadline, b.Passed)
			}
		})
	}
}
