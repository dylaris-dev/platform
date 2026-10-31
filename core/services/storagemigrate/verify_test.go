package storagemigrate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
)

func manifestHeader(id int, entries []models.StorageManifestEntry) *models.StorageManifest {
	var bytes int64
	for _, e := range entries {
		bytes += e.Size
	}
	return &models.StorageManifest{
		ID:          id,
		DataSet:     DataSetAttachments,
		Algo:        ChecksumAlgo,
		CapturedAt:  time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC),
		ObjectCount: int64(len(entries)),
		TotalBytes:  bytes,
	}
}

func statusOf(rep StorageVerifyReport, key string) StorageVerifyStatus {
	for _, p := range rep.Problems {
		if p.Key == key {
			return p.Status
		}
	}
	return VerifyOK
}

func TestVerify_AllGoodIsOK(t *testing.T) {
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("a.jar", "aaa")
	src.put("b.jar", "bbbb")
	entries := manifestOf(t, src)

	target := newMemDataSet(DataSetAttachments, "target")
	target.put("a.jar", "aaa")
	target.put("b.jar", "bbbb")

	rep, err := Verify(ctx, target, manifestHeader(1, entries), entries, VerifyModeFull, VerifyOptions{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !rep.OK {
		t.Fatalf("OK = false, problems = %+v", rep.Problems)
	}
	if rep.ProblemsTotal != 0 {
		t.Errorf("ProblemsTotal = %d, want 0", rep.ProblemsTotal)
	}
	if rep.Mode != VerifyModeFull {
		t.Errorf("Mode = %q, want full", rep.Mode)
	}
	if rep.ManifestID != 1 {
		t.Errorf("ManifestID = %d, want 1", rep.ManifestID)
	}
	if rep.ObjectsInManifest != 2 || rep.ObjectsChecked != 2 {
		t.Errorf("objects = %d/%d, want 2/2", rep.ObjectsChecked, rep.ObjectsInManifest)
	}
	if rep.BytesInManifest != 7 || rep.BytesChecked != 7 {
		t.Errorf("bytes = %d/%d, want 7/7", rep.BytesChecked, rep.BytesInManifest)
	}
	if rep.CheckedFraction != 1 || rep.BytesFraction != 1 {
		t.Errorf("fractions = %v/%v, want 1/1", rep.CheckedFraction, rep.BytesFraction)
	}
}

func TestVerify_ClassifiesEveryStatus(t *testing.T) {
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("ok.jar", "good")
	src.put("missing.jar", "gone")
	src.put("sizediff.jar", "12345")
	src.put("contentdiff.jar", "AAAAAAAA")
	src.put("unreadable.jar", "boom")
	entries := manifestOf(t, src)

	target := newMemDataSet(DataSetAttachments, "target")
	target.put("ok.jar", "good")
	// missing.jar deliberately absent
	target.put("sizediff.jar", "123")         // shorter
	target.put("contentdiff.jar", "AAAAAAAB") // SAME SIZE, different bytes
	target.put("unreadable.jar", "boom")
	target.openErr["unreadable.jar"] = errors.New("api error AccessDenied")
	target.put("extra.jar", "nobody asked for this")

	rep, err := Verify(ctx, target, manifestHeader(2, entries), entries, VerifyModeFull, VerifyOptions{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rep.OK {
		t.Fatal("OK = true despite problems")
	}
	want := map[string]StorageVerifyStatus{
		"ok.jar":          VerifyOK,
		"missing.jar":     VerifyMissing,
		"sizediff.jar":    VerifySizeMismatch,
		"contentdiff.jar": VerifyChecksumMismatch,
		"unreadable.jar":  VerifyUnreadable,
		"extra.jar":       VerifyExtra,
	}
	for key, wantStatus := range want {
		if got := statusOf(rep, key); got != wantStatus {
			t.Errorf("%s status = %q, want %q", key, got, wantStatus)
		}
	}
	if rep.ProblemsTotal != 5 {
		t.Errorf("ProblemsTotal = %d, want 5", rep.ProblemsTotal)
	}
}

func TestVerify_SameSizeDifferentContentIsOnlyCaughtByTheChecksum(t *testing.T) {
	// The load-bearing case. A size-only comparison passes this.
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("a.jar", "AAAAAAAA")
	entries := manifestOf(t, src)

	target := newMemDataSet(DataSetAttachments, "target")
	target.put("a.jar", "AAAAAAAB")

	rep, err := Verify(ctx, target, manifestHeader(1, entries), entries, VerifyModeFull, VerifyOptions{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rep.OK {
		t.Fatal("OK = true for same-size different-content")
	}
	if statusOf(rep, "a.jar") != VerifyChecksumMismatch {
		t.Errorf("status = %q, want checksum_mismatch", statusOf(rep, "a.jar"))
	}
}

func TestVerify_OKIsFalseWheneverProblemsTotalIsPositive(t *testing.T) {
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("a.jar", "aaa")
	entries := manifestOf(t, src)
	target := newMemDataSet(DataSetAttachments, "target") // empty -> one "missing"

	rep, err := Verify(ctx, target, manifestHeader(1, entries), entries, VerifyModeFull, VerifyOptions{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rep.ProblemsTotal == 0 {
		t.Fatal("ProblemsTotal = 0, want 1")
	}
	if rep.OK {
		t.Fatal("OK must be false whenever ProblemsTotal > 0")
	}
}

func TestVerify_ProblemsAreCappedButProblemsTotalIsTruthful(t *testing.T) {
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	n := verifyMaxProblems + 37
	for i := 0; i < n; i++ {
		src.put(fmt.Sprintf("f%04d.jar", i), "x")
	}
	entries := manifestOf(t, src)
	target := newMemDataSet(DataSetAttachments, "target") // everything missing

	rep, err := Verify(ctx, target, manifestHeader(1, entries), entries, VerifyModeFull, VerifyOptions{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(rep.Problems) != verifyMaxProblems {
		t.Errorf("len(Problems) = %d, want the cap %d", len(rep.Problems), verifyMaxProblems)
	}
	if rep.ProblemsTotal != n {
		t.Errorf("ProblemsTotal = %d, want the TRUE count %d", rep.ProblemsTotal, n)
	}
}

func TestVerify_PresenceIsNeverSampled(t *testing.T) {
	// "sample" only ever reduces how many objects get their CONTENTS hashed.
	// missing and extra come from a full key listing, which costs no bytes,
	// and are always computed in full - in BOTH modes.
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	for i := 0; i < 50; i++ {
		src.put(fmt.Sprintf("f%03d.jar", i), "x")
	}
	entries := manifestOf(t, src)

	target := newMemDataSet(DataSetAttachments, "target")
	for i := 0; i < 50; i++ {
		target.put(fmt.Sprintf("f%03d.jar", i), "x")
	}
	target.Delete(ctx, "f007.jar")
	target.put("surprise.jar", "y")

	rep, err := Verify(ctx, target, manifestHeader(1, entries), entries, VerifyModeSample, VerifyOptions{
		Rand: rand.New(rand.NewSource(1)),
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if statusOf(rep, "f007.jar") != VerifyMissing {
		t.Errorf("f007.jar status = %q, want missing (presence is never sampled)", statusOf(rep, "f007.jar"))
	}
	if statusOf(rep, "surprise.jar") != VerifyExtra {
		t.Errorf("surprise.jar status = %q, want extra (presence is never sampled)", statusOf(rep, "surprise.jar"))
	}
}

func TestSampleLargeCount(t *testing.T) {
	// n = min(|L|, max(200, ceil(0.10 * |L|)))
	cases := []struct {
		numLarge int
		want     int
	}{
		{0, 0},
		{1, 1},        // ceil(0.1)=1 -> max(200,1)=200 -> min(1,200)=1
		{199, 199},    // max(200,20)=200 -> min(199,200)=199
		{200, 200},    // max(200,20)=200 -> min(200,200)=200
		{201, 200},    // max(200,21)=200 -> min(201,200)=200
		{2000, 200},   // max(200,200)=200
		{3000, 300},   // max(200,300)=300
		{10000, 1000}, // max(200,1000)=1000
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("L=%d", c.numLarge), func(t *testing.T) {
			if got := sampleLargeCount(c.numLarge); got != c.want {
				t.Errorf("sampleLargeCount(%d) = %d, want %d", c.numLarge, got, c.want)
			}
		})
	}
}

func TestSelectSample_AlwaysIncludesEverySmallObject(t *testing.T) {
	var entries []models.StorageManifestEntry
	for i := 0; i < 500; i++ {
		entries = append(entries, models.StorageManifestEntry{
			Key: fmt.Sprintf("small%03d", i), Size: sampleSmallObjectBytes, Checksum: "c",
		})
	}
	for i := 0; i < 1000; i++ {
		entries = append(entries, models.StorageManifestEntry{
			Key: fmt.Sprintf("large%04d", i), Size: sampleSmallObjectBytes + 1, Checksum: "c",
		})
	}

	got := SelectSample(entries, rand.New(rand.NewSource(7)))
	smalls, larges := 0, 0
	for _, e := range got {
		if e.Size <= sampleSmallObjectBytes {
			smalls++
		} else {
			larges++
		}
	}
	if smalls != 500 {
		t.Errorf("small objects sampled = %d, want all 500 (they are trivial to hash)", smalls)
	}
	if larges != sampleLargeCount(1000) {
		t.Errorf("large objects sampled = %d, want %d", larges, sampleLargeCount(1000))
	}
}

func TestSelectSample_BoundarySizeIsSmall(t *testing.T) {
	// "small" is <= 1 MiB, inclusive.
	entries := []models.StorageManifestEntry{
		{Key: "exact", Size: sampleSmallObjectBytes},
		{Key: "over", Size: sampleSmallObjectBytes + 1},
	}
	got := SelectSample(entries, rand.New(rand.NewSource(1)))
	var haveExact bool
	for _, e := range got {
		if e.Key == "exact" {
			haveExact = true
		}
	}
	if !haveExact {
		t.Error("a 1 MiB object was treated as large; the boundary is inclusive")
	}
}

func TestSelectSample_DrawsWithoutReplacement(t *testing.T) {
	var entries []models.StorageManifestEntry
	for i := 0; i < 3000; i++ {
		entries = append(entries, models.StorageManifestEntry{
			Key: fmt.Sprintf("large%04d", i), Size: sampleSmallObjectBytes * 2, Checksum: "c",
		})
	}
	got := SelectSample(entries, rand.New(rand.NewSource(3)))
	seen := map[string]bool{}
	for _, e := range got {
		if seen[e.Key] {
			t.Fatalf("%s appeared twice; the draw must be without replacement", e.Key)
		}
		seen[e.Key] = true
	}
	if len(got) != sampleLargeCount(3000) {
		t.Errorf("sample size = %d, want %d", len(got), sampleLargeCount(3000))
	}
}

func TestSelectSample_ReturnsSortedByKey(t *testing.T) {
	entries := []models.StorageManifestEntry{
		{Key: "c", Size: 1}, {Key: "a", Size: 1}, {Key: "b", Size: 1},
	}
	got := SelectSample(entries, rand.New(rand.NewSource(1)))
	var keys []string
	for _, e := range got {
		keys = append(keys, e.Key)
	}
	if strings.Join(keys, ",") != "a,b,c" {
		t.Errorf("sample = %v, want ascending", keys)
	}
}

func TestVerify_SampleReportsTheCheckedFraction(t *testing.T) {
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	// 3 small (always hashed) + nothing large, so a sample checks everything
	// here; the assertion is that the arithmetic is right, not that it skips.
	src.put("a", "a")
	src.put("b", "bb")
	src.put("c", "ccc")
	entries := manifestOf(t, src)

	target := newMemDataSet(DataSetAttachments, "target")
	target.put("a", "a")
	target.put("b", "bb")
	target.put("c", "ccc")

	rep, err := Verify(ctx, target, manifestHeader(5, entries), entries, VerifyModeSample, VerifyOptions{
		Rand: rand.New(rand.NewSource(1)),
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rep.Mode != VerifyModeSample {
		t.Errorf("Mode = %q, want sample", rep.Mode)
	}
	if rep.ObjectsInManifest != 3 || rep.ObjectsChecked != 3 {
		t.Errorf("objects = %d/%d, want 3/3", rep.ObjectsChecked, rep.ObjectsInManifest)
	}
	if math.Abs(rep.CheckedFraction-1) > 1e-9 {
		t.Errorf("CheckedFraction = %v, want 1", rep.CheckedFraction)
	}
	if math.Abs(rep.BytesFraction-1) > 1e-9 {
		t.Errorf("BytesFraction = %v, want 1", rep.BytesFraction)
	}
}

func TestVerify_FractionsAreZeroSafeOnAnEmptyManifest(t *testing.T) {
	rep, err := Verify(context.Background(), newMemDataSet(DataSetAttachments, "target"),
		manifestHeader(1, nil), nil, VerifyModeFull, VerifyOptions{})
	if err != nil {
		t.Fatalf("Verify(empty): %v", err)
	}
	if !rep.OK {
		t.Errorf("an empty manifest against an empty target must verify OK; problems = %+v", rep.Problems)
	}
	if rep.CheckedFraction != 0 || rep.BytesFraction != 0 {
		t.Errorf("fractions = %v/%v, want 0/0 with no division by zero", rep.CheckedFraction, rep.BytesFraction)
	}
}

func TestVerify_RejectsAnUnknownMode(t *testing.T) {
	_, err := Verify(context.Background(), newMemDataSet(DataSetAttachments, "target"),
		manifestHeader(1, nil), nil, "partial", VerifyOptions{})
	if err == nil {
		t.Fatal("Verify with an unknown mode err = nil, want an error")
	}
	if ValidVerifyMode("partial") {
		t.Error("ValidVerifyMode(\"partial\") = true, want false")
	}
	for _, m := range []string{VerifyModeFull, VerifyModeSample} {
		if !ValidVerifyMode(m) {
			t.Errorf("ValidVerifyMode(%q) = false", m)
		}
	}
}

func TestVerify_NeverMutatesTheTarget(t *testing.T) {
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("a.jar", "aaa")
	entries := manifestOf(t, src)

	target := newMemDataSet(DataSetAttachments, "target")
	target.put("a.jar", "wrong")
	target.put("extra.jar", "x")
	before := target.snapshot()

	if _, err := Verify(ctx, target, manifestHeader(1, entries), entries, VerifyModeFull, VerifyOptions{}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	after := target.snapshot()
	if len(before) != len(after) {
		t.Fatalf("target object count changed: %d -> %d; verification never deletes an extra", len(before), len(after))
	}
	if len(target.deleted) != 0 {
		t.Errorf("verification deleted %v; orphan reaping is explicitly out of scope", target.deleted)
	}
}

func TestVerify_HonoursCancellation(t *testing.T) {
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("a.jar", "a")
	src.put("b.jar", "b")
	src.put("c.jar", "c")
	entries := manifestOf(t, src)

	target := newMemDataSet(DataSetAttachments, "target")
	target.put("a.jar", "a")
	target.put("b.jar", "b")
	target.put("c.jar", "c")

	calls := 0
	_, err := Verify(ctx, target, manifestHeader(1, entries), entries, VerifyModeFull, VerifyOptions{
		Cancelled: func() bool { calls++; return calls > 1 },
	})
	if !errors.Is(err, ErrVerifyCancelled) {
		t.Fatalf("Verify err = %v, want ErrVerifyCancelled", err)
	}
}

// --- Additions beyond the brief: the two safety properties the task's own
// requirements call out by name (see task-11-report.md for the rationale). ---

// TestVerify_ChecksumIsAuthoritativeOverACorruptManifestSizeField is the
// direct test for "do NOT compare Size the way the copy loop's skip check
// does." The copy loop only ever uses a Size/checksum disagreement to decide
// skip-vs-recopy, which is harmless either way. A verify report is different:
// it is read by a human and (in full mode) authorizes a source delete, so a
// corrupt Size column on an otherwise-perfect manifest entry must NOT turn
// into a stored, permanent "size_mismatch" failure when the checksum -
// computed from the bytes actually read - proves the content is correct.
func TestVerify_ChecksumIsAuthoritativeOverACorruptManifestSizeField(t *testing.T) {
	ctx := context.Background()
	target := newMemDataSet(DataSetAttachments, "target")
	body := "the real, byte-for-byte correct content of this object"
	target.put("a.jar", body)

	sum, n, err := Checksum(strings.NewReader(body))
	if err != nil {
		t.Fatalf("Checksum: %v", err)
	}
	// The manifest's Size column is wrong (corrupted, or a bad hand-edit) but
	// the Checksum column is exactly right for the real content above.
	entries := []models.StorageManifestEntry{
		{Key: "a.jar", Size: n + 999, Checksum: sum},
	}

	rep, err := Verify(ctx, target, manifestHeader(9, entries), entries, VerifyModeFull, VerifyOptions{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if statusOf(rep, "a.jar") != VerifyOK {
		t.Errorf("a.jar status = %q, want ok: the checksum matches, so a corrupt manifest Size field must not fail the object", statusOf(rep, "a.jar"))
	}
	if !rep.OK {
		t.Fatalf("OK = false; problems = %+v (a checksum match must win over a bad Size column)", rep.Problems)
	}
}

// TestVerify_AuthorizesSourceDeleteOnlyForAPassingFullRun is the structural
// test named in the task brief: "there must be a test that would FAIL if a
// sample verify could authorize a delete." AuthorizesSourceDelete is the one
// and only sanctioned way to ask that question; if it were implemented as
// merely "return r.OK" (ignoring Mode), this test fails on the sample case
// below even though every object actually matches.
func TestVerify_AuthorizesSourceDeleteOnlyForAPassingFullRun(t *testing.T) {
	ctx := context.Background()
	src := newMemDataSet(DataSetAttachments, "source")
	src.put("a.jar", "aaa")
	entries := manifestOf(t, src)

	target := newMemDataSet(DataSetAttachments, "target")
	target.put("a.jar", "aaa")

	sampleRep, err := Verify(ctx, target, manifestHeader(1, entries), entries, VerifyModeSample, VerifyOptions{
		Rand: rand.New(rand.NewSource(1)),
	})
	if err != nil {
		t.Fatalf("Verify (sample): %v", err)
	}
	if !sampleRep.OK {
		t.Fatalf("sample OK = false, problems = %+v", sampleRep.Problems)
	}
	if sampleRep.AuthorizesSourceDelete() {
		t.Fatal("a SAMPLE verify report must NEVER authorize a source delete, even when OK is true")
	}

	fullRep, err := Verify(ctx, target, manifestHeader(1, entries), entries, VerifyModeFull, VerifyOptions{})
	if err != nil {
		t.Fatalf("Verify (full): %v", err)
	}
	if !fullRep.AuthorizesSourceDelete() {
		t.Fatal("a passing FULL verify report must authorize a source delete")
	}

	// A full run that found a problem must not authorize a delete either.
	target.put("extra.jar", "surprise")
	failingFullRep, err := Verify(ctx, target, manifestHeader(1, entries), entries, VerifyModeFull, VerifyOptions{})
	if err != nil {
		t.Fatalf("Verify (full, failing): %v", err)
	}
	if failingFullRep.AuthorizesSourceDelete() {
		t.Fatal("a full verify report with problems must not authorize a source delete")
	}
}

// --- Fix pass 1: closing test gaps found by review (see task-11-report.md,
// "## Fix pass 1"). ---

// sampleFixture builds n manifest entries whose declared Size is just over
// the small/large threshold, so SelectSample treats every one of them as
// "large" and subjects them to the bounded, randomized draw. Every entry's
// REAL content is a short, fixed-width string (always the same length,
// regardless of n), deliberately divorced from the declared Size.
//
// This is safe, not sloppy: Verify's checksum-first classification (see its
// doc comment, and TestVerify_ChecksumIsAuthoritativeOverACorruptManifestSizeField)
// means a manifest Size that disagrees with the real content never fails
// verification as long as the checksum matches what's really there. Relying
// on that here is what makes a 200+ object fixture - required before
// sampleLargeCount excludes anything at all, see TestSampleLargeCount -
// cheap to build and hash instead of requiring literal multi-hundred-MB
// payloads.
func sampleFixture(t *testing.T, n int) (entries []models.StorageManifestEntry, bodies map[string]string) {
	t.Helper()
	bodies = make(map[string]string, n)
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("large%04d", i)
		body := fmt.Sprintf("body%04d", i) // fixed width: always 8 bytes
		sum, _, err := Checksum(strings.NewReader(body))
		if err != nil {
			t.Fatalf("Checksum: %v", err)
		}
		entries = append(entries, models.StorageManifestEntry{
			Key:      key,
			Size:     sampleSmallObjectBytes + 1,
			Checksum: sum,
		})
		bodies[key] = body
	}
	return entries, bodies
}

// TestVerify_SampleReportsUndrawnEntryAsMissing is the direct test for
// verify.go's "presence is never sampled" block: a manifest key that was
// neither drawn into the sample NOR present at the target must still be
// reported missing. Every prior sample-mode test (including
// TestVerify_PresenceIsNeverSampled) used objects small enough that
// SelectSample draws all of them, so a missing key was always caught
// earlier, by the target.Open fs.ErrNotExist branch - never by this block.
// Deleting the block left the whole suite green.
//
// Entries here are large enough, and numerous enough (sampleFixture, n=300),
// that SelectSample genuinely excludes some (see TestSampleLargeCount: it
// takes more than 200 large objects before any get excluded). Which key ends
// up undrawn is never hard-coded: it is discovered by calling SelectSample
// with the same seed Verify itself will use, so the test does not depend on
// - and cannot accidentally pin - the randomized draw's specific outcome.
func TestVerify_SampleReportsUndrawnEntryAsMissing(t *testing.T) {
	const n = 300
	const seed = 42
	entries, bodies := sampleFixture(t, n)

	drawn := SelectSample(entries, rand.New(rand.NewSource(seed)))
	drawnKeys := EntriesByKey(drawn)
	var missingKey string
	for _, e := range entries {
		if _, ok := drawnKeys[e.Key]; !ok {
			missingKey = e.Key
			break
		}
	}
	if missingKey == "" {
		t.Fatalf("setup: SelectSample drew every one of %d entries; want it to exclude at least one (sampleLargeCount(%d) = %d)", n, n, sampleLargeCount(n))
	}

	target := newMemDataSet(DataSetAttachments, "target")
	for _, e := range entries {
		if e.Key == missingKey {
			continue // deliberately absent: undrawn from the sample AND missing from the target
		}
		target.put(e.Key, bodies[e.Key])
	}

	rep, err := Verify(context.Background(), target, manifestHeader(1, entries), entries, VerifyModeSample, VerifyOptions{
		Rand: rand.New(rand.NewSource(seed)),
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got := statusOf(rep, missingKey); got != VerifyMissing {
		t.Errorf("%s status = %q, want missing (it was never drawn into the sample and is absent from the target)", missingKey, got)
	}
	if rep.OK {
		t.Error("OK = true despite a missing manifest key")
	}
}

// TestVerify_SampleFractionsDropBelowOneWhenObjectsAreSkipped is the
// non-vacuous counterpart to TestVerify_SampleReportsTheCheckedFraction,
// whose own comment concedes it only proves the arithmetic is right when
// nothing is actually skipped (3 small objects, all drawn). This test drives
// Verify over a data set large enough that SelectSample genuinely excludes
// some large objects, and pins both fractions at their real,
// less-than-one values - so nothing can compute either fraction over the
// SAMPLE (which would read 1) instead of the FULL manifest and still pass.
func TestVerify_SampleFractionsDropBelowOneWhenObjectsAreSkipped(t *testing.T) {
	const n = 300
	var bodyLen = int64(len("body0000")) // every fixture body is 8 bytes
	entries, bodies := sampleFixture(t, n)

	target := newMemDataSet(DataSetAttachments, "target")
	for _, e := range entries {
		target.put(e.Key, bodies[e.Key])
	}

	rep, err := Verify(context.Background(), target, manifestHeader(1, entries), entries, VerifyModeSample, VerifyOptions{
		Rand: rand.New(rand.NewSource(7)),
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !rep.OK {
		t.Fatalf("OK = false, problems = %+v", rep.Problems)
	}

	wantChecked := int64(sampleLargeCount(n))
	if rep.ObjectsChecked != wantChecked {
		t.Fatalf("ObjectsChecked = %d, want %d (sampleLargeCount(%d))", rep.ObjectsChecked, wantChecked, n)
	}
	if rep.ObjectsInManifest != int64(n) {
		t.Fatalf("ObjectsInManifest = %d, want %d", rep.ObjectsInManifest, n)
	}

	wantCheckedFraction := float64(wantChecked) / float64(n)
	if math.Abs(rep.CheckedFraction-wantCheckedFraction) > 1e-9 {
		t.Errorf("CheckedFraction = %v, want %v", rep.CheckedFraction, wantCheckedFraction)
	}
	if rep.CheckedFraction >= 1 {
		t.Errorf("CheckedFraction = %v, want < 1 (the sample excluded %d of %d objects)", rep.CheckedFraction, int64(n)-wantChecked, n)
	}

	wantBytesChecked := wantChecked * bodyLen
	if rep.BytesChecked != wantBytesChecked {
		t.Fatalf("BytesChecked = %d, want %d (%d objects hashed x %d real bytes each)", rep.BytesChecked, wantBytesChecked, wantChecked, bodyLen)
	}
	wantBytesInManifest := int64(n) * (sampleSmallObjectBytes + 1)
	if rep.BytesInManifest != wantBytesInManifest {
		t.Fatalf("BytesInManifest = %d, want %d", rep.BytesInManifest, wantBytesInManifest)
	}
	wantBytesFraction := float64(wantBytesChecked) / float64(wantBytesInManifest)
	if math.Abs(rep.BytesFraction-wantBytesFraction) > 1e-9 {
		t.Errorf("BytesFraction = %v, want %v", rep.BytesFraction, wantBytesFraction)
	}
	if rep.BytesFraction >= 1 {
		t.Errorf("BytesFraction = %v, want < 1", rep.BytesFraction)
	}
}

// TestVerify_BytesExaminedIsTheSampledPopulationNotTheWholeManifest pins the
// denominator the panel puts BytesChecked over.
//
// BytesChecked counts only the objects the hashing loop visited, but the report
// used to offer BytesInManifest as its only companion figure. In sample mode
// that pairs a sampled numerator with a whole-manifest denominator - the exact
// mixed-population bug already fixed for the progress bar - so a flawless
// sample run rendered as though almost every byte had failed to read. That
// under-claim is in the safe direction, but it is still a number that does not
// mean what the sentence around it says.
func TestVerify_BytesExaminedIsTheSampledPopulationNotTheWholeManifest(t *testing.T) {
	const n = 300
	entries, bodies := sampleFixture(t, n)
	target := newMemDataSet(DataSetAttachments, "target")
	for _, e := range entries {
		target.put(e.Key, bodies[e.Key])
	}

	t.Run("sample mode examines less than the manifest", func(t *testing.T) {
		rep, err := Verify(context.Background(), target, manifestHeader(1, entries), entries, VerifyModeSample, VerifyOptions{
			Rand: rand.New(rand.NewSource(7)),
		})
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		// The population BytesExamined describes must be exactly the one
		// ObjectsChecked counts, not the manifest.
		wantExamined := int64(sampleLargeCount(n)) * (sampleSmallObjectBytes + 1)
		if rep.BytesExamined != wantExamined {
			t.Fatalf("BytesExamined = %d, want %d (%d sampled objects x %d manifest bytes each)",
				rep.BytesExamined, wantExamined, sampleLargeCount(n), int64(sampleSmallObjectBytes+1))
		}
		if rep.BytesExamined >= rep.BytesInManifest {
			t.Fatalf("BytesExamined = %d, BytesInManifest = %d: the sample must examine strictly fewer bytes, or this figure adds nothing",
				rep.BytesExamined, rep.BytesInManifest)
		}
		// The point of the field: over its own population a clean sample run
		// reads a far higher share than the whole-manifest figure suggests.
		overExamined := float64(rep.BytesChecked) / float64(rep.BytesExamined)
		if overExamined <= rep.BytesFraction {
			t.Errorf("bytes over the examined population = %v, over the manifest = %v: the honest denominator must not read worse than the mixed one",
				overExamined, rep.BytesFraction)
		}
	})

	t.Run("full mode examines the whole manifest", func(t *testing.T) {
		rep, err := Verify(context.Background(), target, manifestHeader(1, entries), entries, VerifyModeFull, VerifyOptions{})
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if rep.BytesExamined != rep.BytesInManifest {
			t.Fatalf("BytesExamined = %d, BytesInManifest = %d: full mode attempts every object, so the two must agree",
				rep.BytesExamined, rep.BytesInManifest)
		}
	})
}

// TestVerify_SampleReportsProgressOverTheSamplePopulation proves Progress's
// objectsTotal and bytesTotal describe the SAME population in sample mode.
// Progress is invoked only from inside the content-hashing loop, which in
// sample mode iterates over the SAMPLE, not the full manifest (see Verify's
// doc comment: "sample only ever reduces how many objects get their
// CONTENTS hashed"). Pairing the sample's object count with the full
// manifest's byte count - what the code did before this fix - would make a
// panel progress bar show objects reading 100% while bytes sit at a fraction
// of that. objectsSkipped must likewise be truthful: the count of manifest
// entries this loop will never visit because sampling excluded them, not a
// hard-coded 0. Progress was previously never invoked by any test at all.
//
// One of the DRAWN entries is deliberately left absent from the target, so
// this also drives the Open() fs.ErrNotExist branch's Progress call (the
// loop's other call site, which shares the exact same objectsSkipped/
// hashBytesTotal values) - not just the success-path call site - so a bug
// confined to only one of the two identical call sites cannot slip past this
// test the same way the block in finding 1 slipped past every prior test.
func TestVerify_SampleReportsProgressOverTheSamplePopulation(t *testing.T) {
	const n = 300
	const seed = 7
	var bodyLen = int64(len("body0000"))
	entries, bodies := sampleFixture(t, n)

	drawn := SelectSample(entries, rand.New(rand.NewSource(seed)))
	if len(drawn) == 0 {
		t.Fatal("setup: SelectSample drew nothing")
	}
	missingDrawnKey := drawn[0].Key

	target := newMemDataSet(DataSetAttachments, "target")
	for _, e := range entries {
		if e.Key == missingDrawnKey {
			continue // drawn, but absent: exercises the Open() not-found Progress call site too
		}
		target.put(e.Key, bodies[e.Key])
	}

	type tick struct {
		objectsDone, objectsSkipped, objectsTotal, bytesDone, bytesTotal int64
	}
	var ticks []tick
	_, err := Verify(context.Background(), target, manifestHeader(1, entries), entries, VerifyModeSample, VerifyOptions{
		Rand: rand.New(rand.NewSource(seed)),
		Progress: func(objectsDone, objectsSkipped, objectsTotal, bytesDone, bytesTotal int64, _ string) {
			ticks = append(ticks, tick{objectsDone, objectsSkipped, objectsTotal, bytesDone, bytesTotal})
		},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	wantSample := int64(sampleLargeCount(n))
	if int64(len(ticks)) != wantSample {
		t.Fatalf("progress ticks = %d, want %d (one per hashed object)", len(ticks), wantSample)
	}

	wantSkipped := int64(n) - wantSample
	wantBytesTotal := wantSample * (sampleSmallObjectBytes + 1)

	first := ticks[0]
	if first.objectsTotal != wantSample {
		t.Errorf("first tick objectsTotal = %d, want %d (the SAMPLE size - the population this loop actually iterates)", first.objectsTotal, wantSample)
	}
	if first.objectsSkipped != wantSkipped {
		t.Errorf("first tick objectsSkipped = %d, want %d (manifest entries excluded from the sample)", first.objectsSkipped, wantSkipped)
	}
	if first.bytesTotal != wantBytesTotal {
		t.Errorf("first tick bytesTotal = %d, want %d (declared Size summed over the SAME sample population as objectsTotal, not the full manifest)", first.bytesTotal, wantBytesTotal)
	}

	last := ticks[len(ticks)-1]
	if last.objectsDone != wantSample || last.objectsTotal != wantSample {
		t.Errorf("final tick objects = %d/%d, want %d/%d", last.objectsDone, last.objectsTotal, wantSample, wantSample)
	}
	// One fewer than wantSample real reads: missingDrawnKey was drawn (so it
	// still advances objectsDone via the not-found branch) but was never
	// opened successfully, so it never contributed real bytes.
	wantBytesDone := (wantSample - 1) * bodyLen
	if last.bytesDone != wantBytesDone {
		t.Errorf("final tick bytesDone = %d, want %d (%d hashed objects x %d real bytes each, minus the one drawn-but-missing object)", last.bytesDone, wantBytesDone, wantSample, bodyLen)
	}
	if last.bytesTotal != wantBytesTotal {
		t.Errorf("final tick bytesTotal = %d, want %d", last.bytesTotal, wantBytesTotal)
	}
}
