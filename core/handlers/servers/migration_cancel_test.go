package servers

import "testing"

// TestIsCancellableMigrationPhase pins which orchestration phases still allow a
// cancel. Pre-cutover in-flight phases ("" just-started, "starting", "migrating")
// are cancellable; the post-cutover "finalizing" and every terminal phase are
// not - after cutover a cancel is a no-op and the endpoint must refuse it.
func TestIsCancellableMigrationPhase(t *testing.T) {
	tests := []struct {
		phase string
		want  bool
	}{
		{"", true},
		{"starting", true},
		{"migrating", true},
		{"finalizing", false},
		{"done", false},
		{"failed", false},
		{"failed_post_cutover", false},
		{"aborted_players", false},
		{"cancelled", false},
		{"none", false},
	}
	for _, tt := range tests {
		if got := isCancellableMigrationPhase(tt.phase); got != tt.want {
			t.Errorf("isCancellableMigrationPhase(%q) = %v, want %v", tt.phase, got, tt.want)
		}
	}
}
