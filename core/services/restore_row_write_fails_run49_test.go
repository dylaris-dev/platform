package services

import (
	"context"
	"testing"
	"time"

	"dylaris-pkg/queue"
)

// A result whose row write failed was reconciled anyway, got its key back and
// was reconciled a second time by the next tick, overwriting whatever the
// customer changed in between (R49-01). The rows follow the archive once, on
// the pass that also writes the row.
func TestARestoreWhoseRowWriteFailsIsReconciledOnce(t *testing.T) {
	now := time.Now()
	b, st, mr := newRun48Scheduler(t, nil)
	st.updateFails = 1
	key := queue.RestoreResultKey("u", 1)
	payload := run48Payload(now.Unix())
	mr.Set(key, payload)
	mr.SetTTL(key, queue.RestoreResultTTL-time.Second)

	b.handleRestoreResult(context.Background(), queue.BackupRestoresChannel("tok"), payload)
	if len(st.replaced) != 0 {
		t.Fatalf("mod rows replaced for %v although the row write failed", st.replaced)
	}
	if !mr.Exists(key) {
		t.Fatalf("the result was not kept for a retry after the failed row write")
	}

	b.reapAbandonedRestores(context.Background(), now)
	if len(st.updated) != 1 {
		t.Errorf("row written %v, want once on the retry", st.updated)
	}
	if len(st.replaced) != 1 {
		t.Errorf("mod rows replaced for %v, want exactly once", st.replaced)
	}
}
