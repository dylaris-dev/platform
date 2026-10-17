package services

import "testing"

// homeFirst reproduces orderedRegionLeaderCands's reorder step on an already
// capacity-sorted slice: if homeLeaderID is present, move it to front.
// (The worker/service call is integration-tested separately; this locks the rule.)
func homeFirstIDs(cands []leaderCandidate, homeLeaderID string) []string {
	sortLeaderCands(cands)
	moveHomeToFront(cands, homeLeaderID)
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.id)
	}
	return out
}

func TestHomeFirstOrdering(t *testing.T) {
	// L-a freest (alive), L-b less free (alive), L-c dead.
	base := func() []leaderCandidate {
		return []leaderCandidate{
			{endpoint: "a:1", id: "L-a", alive: true, freeBps: 900, known: true},
			{endpoint: "b:1", id: "L-b", alive: true, freeBps: 100, known: true},
			{endpoint: "c:1", id: "L-c", alive: false, freeBps: 0, known: false},
		}
	}
	tests := []struct {
		name string
		home string
		want []string
	}{
		{"no home = freest first", "", []string{"L-a", "L-b", "L-c"}},
		{"alive home wins even if not freest", "L-b", []string{"L-b", "L-a", "L-c"}},
		// The peer lives on its home only; steering the client off a home
		// whose alive key blinked strands it on a leader that does not know it.
		{"dead home still first", "L-c", []string{"L-c", "L-a", "L-b"}},
		{"unknown home ignored", "L-z", []string{"L-a", "L-b", "L-c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := homeFirstIDs(base(), tt.home)
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("%s: got %v, want %v", tt.name, got, tt.want)
				}
			}
		})
	}
}
