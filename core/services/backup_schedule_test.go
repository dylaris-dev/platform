package services

import (
	"testing"
	"time"
)

// TestComputeBackupNextRun pins the schedule-string parser: "every Nh"
// / "every Nd" via fmt.Sscanf; "manual"/empty/malformed all fall back to nil
// (the caller then treats the job as manual-only).
func TestComputeBackupNextRun(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("empty schedule returns nil", func(t *testing.T) {
		if got := ComputeBackupNextRun("", "", from); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
	t.Run("manual returns nil", func(t *testing.T) {
		if got := ComputeBackupNextRun("manual", "", from); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
	t.Run("every 6h adds 6 hours", func(t *testing.T) {
		got := ComputeBackupNextRun("every 6h", "", from)
		if got == nil || !got.Equal(from.Add(6*time.Hour)) {
			t.Errorf("got %v, want %v", got, from.Add(6*time.Hour))
		}
	})
	t.Run("every 2d adds 48 hours", func(t *testing.T) {
		got := ComputeBackupNextRun("every 2d", "", from)
		if got == nil || !got.Equal(from.Add(48*time.Hour)) {
			t.Errorf("got %v, want %v", got, from.Add(48*time.Hour))
		}
	})
	t.Run("every 0h (non-positive n) returns nil", func(t *testing.T) {
		if got := ComputeBackupNextRun("every 0h", "", from); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
	t.Run("every -1h (negative n) returns nil", func(t *testing.T) {
		if got := ComputeBackupNextRun("every -1h", "", from); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
	t.Run("unknown unit returns nil", func(t *testing.T) {
		if got := ComputeBackupNextRun("every 3x", "", from); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
	t.Run("garbage schedule returns nil", func(t *testing.T) {
		if got := ComputeBackupNextRun("whenever I feel like it", "", from); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
}

// The parser above has always been right about a schedule it cannot act on -
// it returns nil. What was missing is anyone ACTING on that: CreateJob stored
// the job regardless, so "banana" became a job that is listed, enabled, and
// never runs. CheckBackupSchedule is that answer in the shape a handler can
// refuse on, and it is defined in terms of the parser so the two cannot drift.
func TestCheckBackupSchedule(t *testing.T) {
	// The first five are what the panel's dropdown offered before the schedule
	// editor (panel/src/components/BackupScheduleEditor.tsx), so jobs saved
	// with them exist and must stay valid. The editor's own forms are pinned in
	// backup_schedule_grammar_test.go and round-tripped in the panel's
	// backupSchedule.test.ts.
	valid := []string{
		"manual", "every 6h", "every 12h", "every 1d", "every 7d",
		"", "every 1h", "every 2d", "every 30d", "  every 6h  ",
	}
	for _, s := range valid {
		if err := CheckBackupSchedule(s, ""); err != nil {
			t.Errorf("CheckBackupSchedule(%q) = %v, want nil", s, err)
		}
	}
	// Every one of these was accepted and stored by the API before the check
	// existed, each producing a job with no next run.
	invalid := []string{
		"banana", "daily", "every 0h", "every -3h", "every 6m", "* * * * *",
		"every 6H", "Every 6h", "every6h", "0 */6 * * *", "every h",
	}
	for _, s := range invalid {
		if CheckBackupSchedule(s, "") == nil {
			t.Errorf("CheckBackupSchedule(%q) = nil, want an error", s)
		}
	}
}
