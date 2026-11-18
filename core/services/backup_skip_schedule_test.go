package services

import (
	"context"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
)

// A slot skipped because the allowance is full stamped last_run_at, so the
// panel said "last run 2m ago" about a backup that never happened.
func TestAllowanceSkipIsRecordedNotRun(t *testing.T) {
	st := newTransferFakeStore()
	st.quotaGB, st.usedBytes = "1", 2*gib
	rdb := heartbeatRedis(t, map[string]string{"node-hosting": presignedMultipartSince})
	b := transferScheduler(st, &fakeMultipartStorage{}, rdb)

	planned := time.Now().Add(-time.Minute)
	if err := b.dispatch(context.Background(), models.BackupJob{ID: 10, ServerID: 100, Schedule: "every 1h", NextRunAt: &planned}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if cmds := dispatchedCommands(t, rdb, "node-hosting"); len(cmds) != 0 {
		t.Fatalf("a run reached the node over the allowance: %v", cmds)
	}
	if st.advanced != 0 || st.skipped != 1 {
		t.Fatalf("advanced %d / skipped %d, want 0 / 1", st.advanced, st.skipped)
	}
	if !strings.Contains(st.skipReason, "allowance") {
		t.Errorf("skip reason = %q, want it to name the allowance", st.skipReason)
	}
}

func TestNextScheduledRunFollowsThePlan(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 40, 0, time.UTC)
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	tests := []struct {
		name    string
		planned *time.Time
		want    time.Time
	}{
		// The tick fired 40 s late: the next slot is the plan + 1h, not now + 1h.
		{name: "late tick keeps the plan", planned: at(-40 * time.Second), want: now.Add(-40*time.Second + time.Hour)},
		{name: "no plan uses now", planned: nil, want: now.Add(time.Hour)},
		// Core was down for hours: start again from now instead of catching up.
		{name: "plan older than an interval restarts", planned: at(-3 * time.Hour), want: now.Add(time.Hour)},
		{name: "exactly one interval late restarts", planned: at(-time.Hour), want: now.Add(time.Hour)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nextScheduledRun("every 1h", "", tt.planned, now)
			if got == nil || !got.Equal(tt.want) {
				t.Errorf("nextScheduledRun = %v, want %v", got, tt.want)
			}
		})
	}
	if got := nextScheduledRun("manual", "", at(-time.Minute), now); got != nil {
		t.Errorf("manual schedule = %v, want nil", got)
	}
}

// R23-04: a scheduled backup of a server that is moving reached the source
// node, whose move cleanup then deleted the archive the run row still pointed
// at. The slot is skipped with the reason instead.
func TestAMovingServerSkipsItsScheduledBackup(t *testing.T) {
	st := newTransferFakeStore()
	st.servers[100].Status = "migrating"
	rdb := heartbeatRedis(t, map[string]string{"node-hosting": presignedMultipartSince})
	b := transferScheduler(st, &fakeMultipartStorage{}, rdb)

	planned := time.Now().Add(-time.Minute)
	if err := b.dispatch(context.Background(), models.BackupJob{ID: 10, ServerID: 100, Schedule: "every 1h", NextRunAt: &planned}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if cmds := dispatchedCommands(t, rdb, "node-hosting"); len(cmds) != 0 {
		t.Fatalf("a backup reached the node of a moving server: %v", cmds)
	}
	if st.skipped != 1 || !strings.Contains(st.skipReason, "moved") {
		t.Fatalf("skipped %d, reason %q; want one skip naming the move", st.skipped, st.skipReason)
	}
}
