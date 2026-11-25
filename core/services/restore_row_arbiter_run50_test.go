package services

import (
	"context"
	"fmt"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-pkg/queue"
)

// The kept result was deleted before its row was written, so a Core that
// exited in between lost a succeeded restore for good, and the reaper later
// closed it as failed (R50-01). The row decides who applies; the key goes only
// once the row holds the result.
func TestTheRowDecidesWhoAppliesAKeptRestoreResult(t *testing.T) {
	now := time.Now()
	channel := queue.BackupRestoresChannel("tok")
	payload := run48Payload(now.Unix())
	tests := []struct {
		name string
		// interrupt runs at the first row write, before it: the competing
		// caller, or a process exit (panic).
		interrupt    func(b *BackupScheduler)
		liveFirst    bool
		updateFails  int
		secondPass   bool
		wantUpdated  int
		wantReplaced int
		wantKept     bool
	}{
		{name: "the live consumer runs while the tick holds the result",
			interrupt:   func(b *BackupScheduler) { b.handleRestoreResult(context.Background(), channel, payload) },
			wantUpdated: 1, wantReplaced: 1},
		{name: "the tick runs while the live consumer holds the result",
			interrupt: func(b *BackupScheduler) { b.reapAbandonedRestores(context.Background(), now) },
			liveFirst: true, wantUpdated: 1, wantReplaced: 1},
		{name: "core exits between the read and the row write",
			interrupt:  func(*BackupScheduler) { panic("core exited") },
			secondPass: true, wantUpdated: 1, wantReplaced: 1},
		{name: "the row write fails", updateFails: 1, wantKept: true},
		{name: "the row write fails, the next pass applies it", updateFails: 1, secondPass: true, wantUpdated: 1, wantReplaced: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, st, mr := newRun48Scheduler(t, nil)
			key := queue.RestoreResultKey("u", 1)
			mr.Set(key, payload)
			mr.SetTTL(key, queue.RestoreResultTTL-time.Second)
			st.updateFails = tt.updateFails
			if tt.interrupt != nil {
				st.beforeUpdate = func() { tt.interrupt(b) }
			}
			first := func() {
				defer func() { _ = recover() }()
				if tt.liveFirst {
					b.handleRestoreResult(context.Background(), channel, payload)
				} else {
					b.reapAbandonedRestores(context.Background(), now)
				}
			}
			first()
			if tt.secondPass {
				b.reapAbandonedRestores(context.Background(), now.Add(time.Minute))
			}
			if len(st.updated) != tt.wantUpdated {
				t.Errorf("row changed %v, want %d times", st.updated, tt.wantUpdated)
			}
			if len(st.replaced) != tt.wantReplaced {
				t.Errorf("mod rows replaced for %v, want %d", st.replaced, tt.wantReplaced)
			}
			if mr.Exists(key) != tt.wantKept {
				t.Errorf("result kept = %v, want %v", mr.Exists(key), tt.wantKept)
			}
		})
	}
}

// The prompt kept-result read went through the reaper's 25-row oldest-first
// batch, so with 25 older restores open a newer one's result was read only
// once it was no longer fresh (R50-02). Every open restore is read.
func TestAKeptResultIsReadPromptlyBehindManyOpenRestores(t *testing.T) {
	b, st, mr := newRun48Scheduler(t, nil)
	now := time.Now()
	var open []models.AbandonedBackupRestore
	for i := 0; i < backupReapBatchSize+5; i++ {
		open = append(open, models.AbandonedBackupRestore{ID: 100 + i, ServerUUID: fmt.Sprintf("old-%d", i), NodeID: 1, RequestedAt: now.Add(-time.Duration(5*60-i) * time.Minute)})
	}
	st.open = append(open, models.AbandonedBackupRestore{ID: 1, ServerUUID: "u", NodeID: 1, RequestedAt: now.Add(-time.Minute)})
	key := queue.RestoreResultKey("u", 1)
	mr.Set(key, run48Payload(now.Unix()))
	mr.SetTTL(key, queue.RestoreResultTTL-30*time.Second)

	b.reapAbandonedRestores(context.Background(), now)

	if len(st.updated) != 1 || len(st.replaced) != 1 {
		t.Errorf("updated %v replaced %v, want the newer restore applied in full", st.updated, st.replaced)
	}
	if mr.Exists(key) {
		t.Errorf("the applied result is still kept")
	}
}
