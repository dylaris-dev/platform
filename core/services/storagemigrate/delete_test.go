package storagemigrate

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAuthorizeSourceDelete(t *testing.T) {
	// ObjectsInManifest is set to a non-zero value on every fixture below that
	// is meant to represent a REAL manifest, so the "authorized" case is
	// authorized on its merits and not accidentally through a zero value -
	// the empty-manifest rule is exercised separately, on its own case and its
	// own dedicated test (TestAuthorizeSourceDelete_EmptyManifestCanNeverAuthorizeADelete),
	// so it cannot be lost silently the way this workstream has lost
	// safety-critical test coverage before.
	pass := &StorageVerifyReport{OK: true, Mode: VerifyModeFull, ObjectsInManifest: 2}
	failing := &StorageVerifyReport{OK: false, Mode: VerifyModeFull, ProblemsTotal: 3, ObjectsInManifest: 2}
	sampled := &StorageVerifyReport{OK: true, Mode: VerifyModeSample, CheckedFraction: 0.1, ObjectsInManifest: 2}
	emptyManifest := &StorageVerifyReport{OK: true, Mode: VerifyModeFull, ObjectsInManifest: 0}

	cases := []struct {
		name           string
		deleteSource   bool
		verifyMode     string
		report         *StorageVerifyReport
		configSwitched bool
		wantErr        bool
	}{
		{"authorized: opt-in + full + passing + switched", true, VerifyModeFull, pass, true, false},
		{"blocked: no opt-in", false, VerifyModeFull, pass, true, true},
		{"blocked: verification failed", true, VerifyModeFull, failing, true, true},
		{"blocked: sample mode cannot authorize a delete", true, VerifyModeSample, sampled, true, true},
		{"blocked: sample mode even with a full report attached", true, VerifyModeSample, pass, true, true},
		{"blocked: full mode but the report says sample", true, VerifyModeFull, sampled, true, true},
		{"blocked: no report at all", true, VerifyModeFull, nil, true, true},
		{"blocked: nothing opted in and nothing verified", false, VerifyModeSample, nil, true, true},
		// Phase ordering. Everything else is in order and it STILL must not
		// delete, because the live config would then point at deleted data.
		{"blocked: the config switch has not happened yet", true, VerifyModeFull, pass, false, true},
		// Safety rule: a full, passing verify of a ZERO-object manifest must
		// NOT authorize a delete on its own. Verify has no source handle, so
		// it cannot distinguish "genuinely empty" from "capture produced
		// nothing" - see the dedicated test below for the full rationale.
		{"blocked: empty manifest, even though everything else authorizes", true, VerifyModeFull, emptyManifest, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := AuthorizeSourceDelete(c.deleteSource, c.verifyMode, c.report, c.configSwitched)
			if (err != nil) != c.wantErr {
				t.Fatalf("AuthorizeSourceDelete(%v, %q, %+v, switched=%v) err = %v, wantErr %v",
					c.deleteSource, c.verifyMode, c.report, c.configSwitched, err, c.wantErr)
			}
			if c.wantErr && !errors.Is(err, ErrDeleteNotAuthorized) {
				t.Errorf("err = %v, want it to wrap ErrDeleteNotAuthorized", err)
			}
		})
	}
}

func TestAuthorizeSourceDelete_DeleteCanNeverPrecedeASuccessfulSwitch(t *testing.T) {
	// The single most important ordering property in the engine, asserted on
	// its own so it cannot be lost in a table edit. Nothing - not an explicit
	// opt-in, not a full-mode passing verification - authorizes a delete while
	// the active config still points at the source.
	pass := &StorageVerifyReport{OK: true, Mode: VerifyModeFull, ObjectsInManifest: 2}
	err := AuthorizeSourceDelete(true, VerifyModeFull, pass, false)
	if !errors.Is(err, ErrDeleteNotAuthorized) {
		t.Fatalf("AuthorizeSourceDelete(..., configSwitched=false) err = %v, want ErrDeleteNotAuthorized", err)
	}
	if !strings.Contains(err.Error(), "switch") {
		t.Errorf("err = %q, want it to name the missing config switch", err)
	}
	if err := AuthorizeSourceDelete(true, VerifyModeFull, pass, true); err != nil {
		t.Fatalf("AuthorizeSourceDelete(..., configSwitched=true) err = %v, want nil", err)
	}
}

func TestAuthorizeSourceDelete_EmptyManifestCanNeverAuthorizeADelete(t *testing.T) {
	// Task 11's review found that AuthorizesSourceDelete() returns true for a
	// full verify of a ZERO-object manifest (OK=true, Mode=full,
	// CheckedFraction=0), because Verify receives no source handle and so
	// cannot structurally distinguish "the source really is empty" from "the
	// capture step failed and produced nothing". This workstream already hit
	// exactly this shape once for real: a node-local backup source that
	// enumerated as silently empty, which would have let a migration report
	// success having copied nothing. The delete gate must refuse an empty
	// manifest on its own, independent of what the report's OK/Mode says -
	// asserted on its own, like the phase-ordering property above, so it
	// cannot be lost in a table edit.
	emptyButPassing := &StorageVerifyReport{OK: true, Mode: VerifyModeFull, ObjectsInManifest: 0}
	err := AuthorizeSourceDelete(true, VerifyModeFull, emptyButPassing, true)
	if !errors.Is(err, ErrDeleteNotAuthorized) {
		t.Fatalf("AuthorizeSourceDelete(empty manifest) err = %v, want ErrDeleteNotAuthorized", err)
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %q, want it to name the empty manifest", err)
	}

	nonEmpty := &StorageVerifyReport{OK: true, Mode: VerifyModeFull, ObjectsInManifest: 1}
	if err := AuthorizeSourceDelete(true, VerifyModeFull, nonEmpty, true); err != nil {
		t.Fatalf("AuthorizeSourceDelete(non-empty manifest) err = %v, want nil", err)
	}
}

func TestDeleteSource_RemovesEveryManifestKeyAndNothingElse(t *testing.T) {
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("a.jar", "aaa")
	src.put("b.jar", "bbbb")
	src.put("written-after-the-manifest.jar", "new")

	// The manifest covers only the two objects that existed when it was
	// captured, not the one written afterwards.
	manifest := manifestOf(t, newSubsetOf(src, "a.jar", "b.jar"))

	res, err := DeleteSource(ctx, src, manifest, DeleteOptions{})
	if err != nil {
		t.Fatalf("DeleteSource: %v", err)
	}
	if res.ObjectsDeleted != 2 {
		t.Errorf("ObjectsDeleted = %d, want 2", res.ObjectsDeleted)
	}
	if res.BytesFreed != 7 {
		t.Errorf("BytesFreed = %d, want 7", res.BytesFreed)
	}
	left := src.snapshot()
	if len(left) != 1 || left["written-after-the-manifest.jar"] != "new" {
		t.Fatalf("source left = %v, want only the object written after the manifest was captured", left)
	}
}

func TestDeleteSource_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("a.jar", "aaa")
	manifest := manifestOf(t, newSubsetOf(src, "a.jar"))

	if _, err := DeleteSource(ctx, src, manifest, DeleteOptions{}); err != nil {
		t.Fatalf("first DeleteSource: %v", err)
	}
	res, err := DeleteSource(ctx, src, manifest, DeleteOptions{})
	if err != nil {
		t.Fatalf("second DeleteSource err = %v, want nil (DataSet.Delete is idempotent)", err)
	}
	if res.ObjectsDeleted != 1 {
		t.Errorf("ObjectsDeleted = %d, want 1 (a repeated delete still counts the attempt)", res.ObjectsDeleted)
	}
}

func TestDeleteSource_ReportsProgress(t *testing.T) {
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("a.jar", "aa")
	src.put("b.jar", "bb")
	manifest := manifestOf(t, newSubsetOf(src, "a.jar", "b.jar"))

	var lastDone, lastTotal int64
	if _, err := DeleteSource(ctx, src, manifest, DeleteOptions{
		Progress: func(done, _, total, _, _ int64, _ string) { lastDone, lastTotal = done, total },
	}); err != nil {
		t.Fatalf("DeleteSource: %v", err)
	}
	if lastDone != 2 || lastTotal != 2 {
		t.Errorf("final progress = %d/%d, want 2/2", lastDone, lastTotal)
	}
}

func TestDeleteSource_PropagatesADeleteFailure(t *testing.T) {
	ctx := context.Background()
	src := &deleteFailingDataSet{memDataSet: newMemDataSet(DataSetAttachments, "source"), failOn: "b.jar"}
	src.put("a.jar", "aa")
	src.put("b.jar", "bb")
	manifest := manifestOf(t, newSubsetOf(src.memDataSet, "a.jar", "b.jar"))

	res, err := DeleteSource(ctx, src, manifest, DeleteOptions{})
	if err == nil {
		t.Fatal("DeleteSource err = nil, want the delete failure surfaced")
	}

	// A bare err != nil check would stay green if a refactor discarded the
	// partial result or carried on past the failure. The three properties an
	// operator has to be able to trust after a partial delete are pinned here.
	if !strings.Contains(err.Error(), "b.jar") {
		t.Errorf("err = %v, want it to name the key that failed", err)
	}
	if res.ObjectsDeleted != 1 {
		t.Errorf("ObjectsDeleted = %d, want 1 (the work done before the failure must survive the error)", res.ObjectsDeleted)
	}
	if _, stillThere := src.snapshot()["b.jar"]; !stillThere {
		t.Error("b.jar is gone, want the failing key and everything after it left untouched")
	}
}

// deleteFailingDataSet fails Delete for one key. Local to this file.
type deleteFailingDataSet struct {
	*memDataSet
	failOn string
}

func (d *deleteFailingDataSet) Delete(ctx context.Context, key string) error {
	if key == d.failOn {
		return errors.New("permission denied")
	}
	return d.memDataSet.Delete(ctx, key)
}

// newSubsetOf returns a memDataSet holding only the named keys of src, so a
// manifest can be captured over a SUBSET of what the source now holds - the
// "objects written after the manifest was captured" case.
func newSubsetOf(src *memDataSet, keys ...string) *memDataSet {
	sub := newMemDataSet(src.ID(), src.Label())
	all := src.snapshot()
	for _, k := range keys {
		sub.put(k, all[k])
	}
	return sub
}
