package updates

import "testing"

// The connect gate and the panel's Required line both read this set, so a
// requirement in either file must be in it, in the newest-first order
// release.Requirement relies on.
func TestRequirementsCoversBothFiles(t *testing.T) {
	if err := ParseError(); err != nil {
		t.Fatalf("embedded notes: %v", err)
	}
	got := Requirements()
	if want := len(Platform()) + len(Hosted()); len(got) != want {
		t.Fatalf("got %d releases, want %d (platform.md + hosted.md)", len(got), want)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Version.Compare(got[i].Version) < 0 {
			t.Fatalf("not newest first: %s before %s", got[i-1].Version, got[i].Version)
		}
	}
}
