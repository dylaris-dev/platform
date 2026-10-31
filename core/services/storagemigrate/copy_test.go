package storagemigrate

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"

	"dylaris-core/models"
)

// manifestOf builds the entry list a capture of ds would have produced, so
// copy tests do not have to hand-compute checksums.
func manifestOf(t *testing.T, ds *memDataSet) []models.StorageManifestEntry {
	t.Helper()
	st := newFakeManifestStore()
	_, entries, err := CaptureManifest(context.Background(), ds, st, CaptureOptions{BackendLabel: "test"})
	if err != nil {
		t.Fatalf("manifestOf: %v", err)
	}
	return entries
}

func TestCopyAll_CopiesEverythingIntoAnEmptyTarget(t *testing.T) {
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "Library (source)")
	src.put("a.jar", "aaa")
	src.put("sub/b.jar", "bbbb")
	entries := manifestOf(t, src)

	target := newMemDataSet(DataSetAttachments, "Library (target)")
	res, err := CopyAll(ctx, src, target, entries, CopyOptions{})
	if err != nil {
		t.Fatalf("CopyAll: %v", err)
	}
	if res.ObjectsCopied != 2 || res.ObjectsSkipped != 0 {
		t.Errorf("result = %+v, want 2 copied / 0 skipped", res)
	}
	if res.BytesCopied != 7 || res.BytesTotal != 7 {
		t.Errorf("bytes = %d/%d, want 7/7", res.BytesCopied, res.BytesTotal)
	}
	got := target.snapshot()
	if got["a.jar"] != "aaa" || got["sub/b.jar"] != "bbbb" {
		t.Errorf("target = %v, want the source contents", got)
	}
}

func TestCopyAll_SkipsOnlyOnAChecksumMatch(t *testing.T) {
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("same.jar", "identical")
	src.put("differs.jar", "AAAAAAAA")
	entries := manifestOf(t, src)

	target := newMemDataSet(DataSetAttachments, "target")
	target.put("same.jar", "identical")
	// Same NAME and same SIZE, different content: the truncated /
	// half-written-from-a-crashed-run case. Existence alone cannot tell this
	// apart, which is exactly why the skip check hashes.
	target.put("differs.jar", "AAAAAAAB")

	res, err := CopyAll(ctx, src, target, entries, CopyOptions{})
	if err != nil {
		t.Fatalf("CopyAll: %v", err)
	}
	if res.ObjectsSkipped != 1 {
		t.Errorf("skipped = %d, want 1 (only the checksum-identical object)", res.ObjectsSkipped)
	}
	if res.ObjectsCopied != 1 {
		t.Errorf("copied = %d, want 1 (the differing object must be re-copied)", res.ObjectsCopied)
	}
	if got := target.snapshot()["differs.jar"]; got != "AAAAAAAA" {
		t.Errorf("differs.jar = %q, want the source content AAAAAAAA", got)
	}
}

func TestCopyAll_SameChecksumDifferentSizeIsRecopied(t *testing.T) {
	// Belt and braces: the skip requires BOTH size and checksum to match.
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("a.jar", "aaa")
	entries := manifestOf(t, src)
	entries[0].Size = 999 // manifest claims a different size

	target := newMemDataSet(DataSetAttachments, "target")
	target.put("a.jar", "aaa")

	res, err := CopyAll(ctx, src, target, entries, CopyOptions{})
	if err != nil {
		t.Fatalf("CopyAll: %v", err)
	}
	if res.ObjectsSkipped != 0 || res.ObjectsCopied != 1 {
		t.Fatalf("result = %+v, want 0 skipped / 1 copied when the size disagrees", res)
	}
}

func TestCopyAll_TargetOpenFailureThatIsNotMissingFailsTheJob(t *testing.T) {
	// A 503 / throttle / permission error is NOT "missing". Treating it as
	// missing would overwrite live data on a target that merely hiccupped.
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("a.jar", "aaa")
	entries := manifestOf(t, src)

	target := newMemDataSet(DataSetAttachments, "target")
	// Deliberately DIFFERENT from the source content ("aaa"): if the loop
	// wrongly treated the throttle as "absent" and overwrote the target,
	// identical content would make that overwrite invisible to snapshot().
	target.put("a.jar", "DO-NOT-OVERWRITE")
	throttled := errors.New("api error SlowDown")
	target.openErr["a.jar"] = throttled

	_, err := CopyAll(ctx, src, target, entries, CopyOptions{})
	if err == nil {
		t.Error("CopyAll err = nil, want the target-open failure to fail the object")
	}
	if !errors.Is(err, throttled) {
		t.Errorf("CopyAll err = %v, want it to wrap %v", err, throttled)
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Error("a throttle was reported as a missing object")
	}
	// The rule is "must NOT write", not merely "returns an error": prove the
	// target was never touched.
	if got := target.snapshot()["a.jar"]; got != "DO-NOT-OVERWRITE" {
		t.Errorf("target a.jar = %q after the failed run, want it byte-for-byte UNCHANGED", got)
	}
}

func TestCopyAll_ChecksumMismatchAfterWriteDeletesTheTargetObjectAndFails(t *testing.T) {
	// A corrupt target object is worse than an absent one: a later run would
	// see it, hash it, find it wrong, and re-copy - but a verify in between
	// would report a checksum_mismatch on data the operator may already have
	// switched to. So the bad write is removed immediately.
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("a.jar", "aaa")
	entries := manifestOf(t, src)
	// Corrupt the expectation so the post-write comparison must fail.
	entries[0].Checksum = "00000000000000000000000000000000000000000000000000000000000000ff"

	target := newMemDataSet(DataSetAttachments, "target")
	_, err := CopyAll(ctx, src, target, entries, CopyOptions{})
	if err == nil {
		t.Fatal("CopyAll err = nil, want a checksum-mismatch failure")
	}
	if _, present := target.snapshot()["a.jar"]; present {
		t.Error("the mismatched object was left on the target; it must be deleted")
	}
	found := false
	for _, k := range target.deleted {
		if k == "a.jar" {
			found = true
		}
	}
	if !found {
		t.Errorf("target.Delete was not called for the bad write; deleted = %v", target.deleted)
	}
}

func TestCopyAll_SourceIsNeverMutated(t *testing.T) {
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("a.jar", "aaa")
	src.put("b.jar", "bbb")
	entries := manifestOf(t, src)
	before := src.snapshot()

	target := newMemDataSet(DataSetAttachments, "target")
	if _, err := CopyAll(ctx, src, target, entries, CopyOptions{}); err != nil {
		t.Fatalf("CopyAll: %v", err)
	}
	after := src.snapshot()
	for k, v := range before {
		if after[k] != v {
			t.Errorf("source %s changed: %q -> %q", k, v, after[k])
		}
	}
	if len(src.deleted) != 0 {
		t.Errorf("copy deleted %v from the SOURCE; only deleting_source may write to it", src.deleted)
	}
}

func TestCopyAll_SourceUntouchedAfterAFailure(t *testing.T) {
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("a.jar", "aaa")
	src.put("b.jar", "bbb")
	entries := manifestOf(t, src)
	before := src.snapshot()

	target := newMemDataSet(DataSetAttachments, "target")
	target.writeErr["b.jar"] = errors.New("disk full")

	if _, err := CopyAll(ctx, src, target, entries, CopyOptions{}); err == nil {
		t.Fatal("CopyAll err = nil, want the write failure")
	}
	after := src.snapshot()
	if len(before) != len(after) {
		t.Fatalf("source object count changed after a failure: %d -> %d", len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("source %s changed after a failure: %q -> %q", k, v, after[k])
		}
	}
}

func TestCopyAll_ResumesAfterAPartialFailure(t *testing.T) {
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("a.jar", "aaa")
	src.put("b.jar", "bbb")
	src.put("c.jar", "ccc")
	entries := manifestOf(t, src)

	target := newMemDataSet(DataSetAttachments, "target")
	target.writeErr["b.jar"] = errors.New("disk full")
	if _, err := CopyAll(ctx, src, target, entries, CopyOptions{}); err == nil {
		t.Fatal("first run err = nil, want the injected failure")
	}

	// Fix the target and re-run the SAME manifest: a.jar is already
	// checksum-identical so it is skipped, b.jar and c.jar are copied.
	delete(target.writeErr, "b.jar")
	res, err := CopyAll(ctx, src, target, entries, CopyOptions{})
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if res.ObjectsSkipped != 1 {
		t.Errorf("skipped = %d, want 1 (a.jar was already identical)", res.ObjectsSkipped)
	}
	if res.ObjectsCopied != 2 {
		t.Errorf("copied = %d, want 2 (b.jar and c.jar)", res.ObjectsCopied)
	}
	got := target.snapshot()
	if got["a.jar"] != "aaa" || got["b.jar"] != "bbb" || got["c.jar"] != "ccc" {
		t.Errorf("target after resume = %v, want all three objects", got)
	}
}

func TestCopyAll_ResumesOverATruncatedTargetObject(t *testing.T) {
	// The case that actually matters for the later source-delete step: a
	// crashed/interrupted write can leave a PARTIAL object on the target -
	// not "no object", not "the whole object". A resume run must detect that
	// via checksum mismatch (same as any other corrupt target) and re-copy
	// it, never skip it because a key with that name merely exists.
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("a.jar", "abcdefghij") // 10 bytes
	entries := manifestOf(t, src)

	target := newMemDataSet(DataSetAttachments, "target")
	target.writeErr["a.jar"] = errors.New("connection reset")
	target.partialWriteN["a.jar"] = 3 // only 3 of 10 bytes land before the failure
	if _, err := CopyAll(ctx, src, target, entries, CopyOptions{}); err == nil {
		t.Fatal("first run err = nil, want the injected mid-write failure")
	}
	if got := target.snapshot()["a.jar"]; got != "abc" {
		t.Fatalf("target after the crashed write = %q, want the truncated prefix %q", got, "abc")
	}

	// Fix the target and re-run the SAME manifest. The truncated object must
	// be re-copied, not skipped: its size (3) already disagrees with the
	// manifest, but the property under test is the checksum path, which is
	// what actually protects a same-size truncation/corruption too.
	delete(target.writeErr, "a.jar")
	delete(target.partialWriteN, "a.jar")
	res, err := CopyAll(ctx, src, target, entries, CopyOptions{})
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if res.ObjectsSkipped != 0 || res.ObjectsCopied != 1 {
		t.Errorf("result = %+v, want 0 skipped / 1 copied (a truncated object must never be skipped)", res)
	}
	if got := target.snapshot()["a.jar"]; got != "abcdefghij" {
		t.Errorf("target after resume = %q, want the full source content %q", got, "abcdefghij")
	}
}

func TestCopyAll_CancelsAtAnObjectBoundaryNeverMidObject(t *testing.T) {
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("a.jar", "aaa")
	src.put("b.jar", "bbb")
	src.put("c.jar", "ccc")
	entries := manifestOf(t, src)

	target := newMemDataSet(DataSetAttachments, "target")
	calls := 0
	_, err := CopyAll(ctx, src, target, entries, CopyOptions{
		Cancelled: func() bool {
			calls++
			return calls > 1 // allow exactly one object through
		},
	})
	if !errors.Is(err, ErrCopyCancelled) {
		t.Fatalf("CopyAll err = %v, want ErrCopyCancelled", err)
	}
	got := target.snapshot()
	if len(got) != 1 {
		t.Fatalf("target holds %d objects (%v), want exactly 1 fully-written object", len(got), got)
	}
	// Whatever landed must be WHOLE - never a partial object.
	for k, v := range got {
		if v != src.snapshot()[k] {
			t.Errorf("target %s = %q, want the complete source content %q", k, v, src.snapshot()[k])
		}
	}
}

func TestCopyAll_ProcessesKeysInAscendingOrder(t *testing.T) {
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("c.jar", "c")
	src.put("a.jar", "a")
	src.put("b.jar", "b")
	entries := manifestOf(t, src)
	// Shuffle the manifest; CopyAll must sort it so a resumed run visits the
	// same keys in the same order regardless of storage order.
	entries[0], entries[2] = entries[2], entries[0]

	var order []string
	target := newMemDataSet(DataSetAttachments, "target")
	if _, err := CopyAll(ctx, src, target, entries, CopyOptions{
		Progress: func(_, _, _, _, _ int64, key string) { order = append(order, key) },
	}); err != nil {
		t.Fatalf("CopyAll: %v", err)
	}
	if len(order) != 3 || order[0] != "a.jar" || order[1] != "b.jar" || order[2] != "c.jar" {
		t.Fatalf("visit order = %v, want ascending", order)
	}
}

func TestCopyAll_ReportsProgressIncludingSkips(t *testing.T) {
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("a.jar", "aa")
	src.put("b.jar", "bb")
	entries := manifestOf(t, src)

	target := newMemDataSet(DataSetAttachments, "target")
	target.put("a.jar", "aa")

	var lastDone, lastSkipped, lastTotal, lastBytes, lastBytesTotal int64
	if _, err := CopyAll(ctx, src, target, entries, CopyOptions{
		Progress: func(done, skipped, total, bytesDone, bytesTotal int64, _ string) {
			lastDone, lastSkipped, lastTotal, lastBytes, lastBytesTotal = done, skipped, total, bytesDone, bytesTotal
		},
	}); err != nil {
		t.Fatalf("CopyAll: %v", err)
	}
	if lastDone != 2 || lastTotal != 2 {
		t.Errorf("final objects = %d/%d, want 2/2 (a skip still advances done)", lastDone, lastTotal)
	}
	if lastSkipped != 1 {
		t.Errorf("final skipped = %d, want 1", lastSkipped)
	}
	if lastBytes != 4 || lastBytesTotal != 4 {
		t.Errorf("final bytes = %d/%d, want 4/4", lastBytes, lastBytesTotal)
	}
}

func TestCopyAll_EmptyManifestIsANoOp(t *testing.T) {
	target := newMemDataSet(DataSetAttachments, "target")
	res, err := CopyAll(context.Background(), newMemDataSet(DataSetAttachments, "source"), target, nil, CopyOptions{})
	if err != nil {
		t.Fatalf("CopyAll(empty) err = %v, want nil", err)
	}
	if res.ObjectsCopied != 0 || res.ObjectsSkipped != 0 || res.BytesTotal != 0 {
		t.Fatalf("result = %+v, want zeroes", res)
	}
}

func TestHashingReader(t *testing.T) {
	hr := newHashingReader(readerOf("hello world"))
	got, err := drain(hr)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got != "hello world" {
		t.Errorf("passthrough = %q, want hello world", got)
	}
	if hr.count() != 11 {
		t.Errorf("count = %d, want 11", hr.count())
	}
	want, _, _ := Checksum(readerOf("hello world"))
	if hr.sum() != want {
		t.Errorf("sum = %q, want %q", hr.sum(), want)
	}
}

func readerOf(s string) io.Reader { return strings.NewReader(s) }

func drain(r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	return string(b), err
}
