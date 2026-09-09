package services

import (
	"testing"
	"time"
)

// A backup now publishes its archived byte count every few seconds, so the same
// channel carries progress and outcomes. These pin the two ways that goes wrong
// quietly.
func TestApplyBackupReport(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		report       string
		row          string
		wantApply    bool
		wantFinished bool
		why          string
	}{
		{
			name: "progress while the run is still going", report: "running", row: "running",
			wantApply: true, wantFinished: false,
			why: "the size is written, completed_at stays NULL",
		},
		{
			// The race the whole helper exists for. Pub/Sub orders nothing, so a
			// progress message can land after the terminal one; writing it would
			// put a finished run back into progress and blank its error message.
			name: "progress arriving after the run already succeeded", report: "running", row: "success",
			wantApply: false,
			why:       "a finished run must not go back into progress",
		},
		{
			name: "progress arriving after the run already failed", report: "running", row: "failed",
			wantApply: false,
			why:       "the error message must survive a late progress report",
		},
		{
			name: "the run succeeded", report: "success", row: "running",
			wantApply: true, wantFinished: true,
		},
		{
			name: "the run failed", report: "failed", row: "running",
			wantApply: true, wantFinished: true,
		},
		{
			// The reaper may have marked it failed first; the node's own verdict
			// still gets written, with its error text.
			name: "a terminal report for a run the reaper already closed", report: "failed", row: "failed",
			wantApply: true, wantFinished: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apply, completed := applyBackupReport(tt.report, tt.row, now)
			if apply != tt.wantApply {
				t.Fatalf("apply = %v, want %v (%s)", apply, tt.wantApply, tt.why)
			}
			if !apply {
				return
			}
			finished := !completed.IsZero()
			if finished != tt.wantFinished {
				t.Errorf("completed_at set = %v, want %v", finished, tt.wantFinished)
			}
			if tt.wantFinished && !completed.Equal(now) {
				t.Errorf("completed = %v, want %v", completed, now)
			}
		})
	}
}
