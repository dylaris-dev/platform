package storagemigrate

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"dylaris-core/models"
)

// fakeManifestStore captures what CaptureManifest persists. Local to this
// file; never redeclared.
type fakeManifestStore struct {
	nextID    int
	manifests map[int]*models.StorageManifest
	entries   map[int][]models.StorageManifestEntry
	createErr error
}

func newFakeManifestStore() *fakeManifestStore {
	return &fakeManifestStore{
		nextID:    1,
		manifests: map[int]*models.StorageManifest{},
		entries:   map[int][]models.StorageManifestEntry{},
	}
}

func (f *fakeManifestStore) CreateStorageManifest(m *models.StorageManifest, entries []models.StorageManifestEntry) (int, error) {
	if f.createErr != nil {
		return 0, f.createErr
	}
	id := f.nextID
	f.nextID++
	cp := *m
	cp.ID = id
	f.manifests[id] = &cp
	f.entries[id] = append([]models.StorageManifestEntry(nil), entries...)
	m.ID = id
	return id, nil
}

func (f *fakeManifestStore) GetStorageManifest(id int) (*models.StorageManifest, error) {
	return f.manifests[id], nil
}

func (f *fakeManifestStore) ListStorageManifestEntries(id int) ([]models.StorageManifestEntry, error) {
	return f.entries[id], nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestCaptureManifest_PersistsEveryObjectWithItsChecksum(t *testing.T) {
	ctx := context.Background()
	ds := newMemDataSet(DataSetAttachments, "Ticket attachments")
	ds.put("a.jar", "aaa")
	ds.put("sub/b.jar", "bbbb")
	ds.put("sub/c.jar", "c")

	st := newFakeManifestStore()
	m, entries, err := CaptureManifest(ctx, ds, st, CaptureOptions{
		BackendLabel: "path:/mnt/shared/library",
		CreatedBy:    "admin-uuid",
	})
	if err != nil {
		t.Fatalf("CaptureManifest: %v", err)
	}

	if m.ID == 0 {
		t.Error("manifest was not persisted (ID still 0)")
	}
	if m.DataSet != DataSetAttachments {
		t.Errorf("DataSet = %q, want %q", m.DataSet, DataSetAttachments)
	}
	if m.Algo != ChecksumAlgo {
		t.Errorf("Algo = %q, want %q", m.Algo, ChecksumAlgo)
	}
	if m.BackendLabel != "path:/mnt/shared/library" {
		t.Errorf("BackendLabel = %q", m.BackendLabel)
	}
	if m.CreatedBy != "admin-uuid" {
		t.Errorf("CreatedBy = %q", m.CreatedBy)
	}
	if m.ObjectCount != 3 {
		t.Errorf("ObjectCount = %d, want 3", m.ObjectCount)
	}
	if m.TotalBytes != 8 {
		t.Errorf("TotalBytes = %d, want 8 (3+4+1)", m.TotalBytes)
	}
	if m.CapturedAt.IsZero() {
		t.Error("CapturedAt is zero")
	}

	want := map[string]struct {
		size int64
		sum  string
	}{
		"a.jar":     {3, sha256Hex("aaa")},
		"sub/b.jar": {4, sha256Hex("bbbb")},
		"sub/c.jar": {1, sha256Hex("c")},
	}
	if len(entries) != len(want) {
		t.Fatalf("entries = %+v, want %d", entries, len(want))
	}
	for _, e := range entries {
		w, ok := want[e.Key]
		if !ok {
			t.Errorf("unexpected entry %q", e.Key)
			continue
		}
		if e.Size != w.size {
			t.Errorf("%s size = %d, want %d", e.Key, e.Size, w.size)
		}
		if e.Checksum != w.sum {
			t.Errorf("%s checksum = %q, want %q", e.Key, e.Checksum, w.sum)
		}
	}
	// And the same rows reached the store.
	persisted, _ := st.ListStorageManifestEntries(m.ID)
	if len(persisted) != len(want) {
		t.Errorf("persisted %d entries, want %d", len(persisted), len(want))
	}
}

// lyingSizeDataSet wraps memDataSet and makes List report a fabricated Size
// for every ref, independent of the object's real length, so a test can
// prove the manifest records the size actually READ rather than trusting
// the listing hint. Same wrapper technique as duplicateKeyDataSet above.
// Local to this file; never redeclared.
type lyingSizeDataSet struct {
	*memDataSet
	lieSize int64
}

func (l *lyingSizeDataSet) List(ctx context.Context) ([]ObjectRef, error) {
	refs, err := l.memDataSet.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ObjectRef, len(refs))
	for i, r := range refs {
		out[i] = ObjectRef{Key: r.Key, Size: l.lieSize}
	}
	return out, nil
}

var _ DataSet = (*lyingSizeDataSet)(nil)

func TestCaptureManifest_SizeComesFromTheActualRead(t *testing.T) {
	// List's Size is a hint from the backend listing; the manifest must
	// record what was actually streamed, so a listing that lies (or a file
	// that changed between List and Open) is recorded truthfully. The
	// listing here is rigged to claim every object is 999 bytes while the
	// real content is 10 bytes: this test would fail if production code
	// ever started trusting ref.Size instead of the bytes actually read.
	ctx := context.Background()
	inner := newMemDataSet(DataSetAttachments, "Ticket attachments")
	inner.put("a.jar", "0123456789")
	ds := &lyingSizeDataSet{memDataSet: inner, lieSize: 999}

	var lines []string
	st := newFakeManifestStore()
	m, entries, err := CaptureManifest(ctx, ds, st, CaptureOptions{
		BackendLabel: "path:/x",
		Log:          func(l string) { lines = append(lines, l) },
	})
	if err != nil {
		t.Fatalf("CaptureManifest: %v", err)
	}
	if len(entries) != 1 || entries[0].Size != 10 {
		t.Fatalf("entries = %+v, want one entry of size 10 (the true read length, not the lying listed 999)", entries)
	}
	if m.TotalBytes != 10 {
		t.Errorf("TotalBytes = %d, want 10", m.TotalBytes)
	}
	// The listing/read disagreement is a real signal (the object may have
	// changed mid-capture) and must not be silently discarded, so it gets
	// the same warning treatment as the duplicate-key and checksum-hint
	// anomalies elsewhere in this file.
	found := false
	for _, l := range lines {
		if strings.Contains(l, "a.jar") && strings.Contains(l, "999") && strings.Contains(l, "10") {
			found = true
		}
	}
	if !found {
		t.Errorf("no warning logged for the listing/read size disagreement; lines = %v", lines)
	}
}

func TestCaptureManifest_EmptyDataSetIsNotAnError(t *testing.T) {
	st := newFakeManifestStore()
	m, entries, err := CaptureManifest(context.Background(), newMemDataSet(DataSetAttachments, "Ticket attachments"), st, CaptureOptions{BackendLabel: "path:/x"})
	if err != nil {
		t.Fatalf("CaptureManifest on empty data set err = %v, want nil", err)
	}
	if m.ObjectCount != 0 || m.TotalBytes != 0 || len(entries) != 0 {
		t.Fatalf("empty capture = %+v / %+v, want zeroes", m, entries)
	}
	if m.ID == 0 {
		t.Error("an empty manifest must still be persisted - it is a valid inventory")
	}
}

func TestCaptureManifest_ReadFailureAbortsAndPersistsNothing(t *testing.T) {
	// Partial failure is failure. A manifest missing objects would make a
	// perfectly good target look full of "extra" keys at verification.
	ctx := context.Background()
	ds := newMemDataSet(DataSetAttachments, "Ticket attachments")
	ds.put("a.jar", "aaa")
	ds.put("b.jar", "bbb")
	boom := errors.New("backend 503")
	ds.openErr["b.jar"] = boom

	st := newFakeManifestStore()
	_, _, err := CaptureManifest(ctx, ds, st, CaptureOptions{BackendLabel: "path:/x"})
	if !errors.Is(err, boom) {
		t.Fatalf("CaptureManifest err = %v, want it to wrap %v", err, boom)
	}
	if len(st.manifests) != 0 {
		t.Errorf("a partial manifest was persisted: %+v", st.manifests)
	}
}

func TestCaptureManifest_VanishedObjectAbortsAsNotFoundNotTransient(t *testing.T) {
	// An object can disappear between List and Open (deleted concurrently, a
	// cleanup job, ...). The capture must still abort and persist nothing
	// (a manifest entry for something that turned out not to exist is exactly
	// the inaccuracy this system exists to prevent), but the failure must
	// stay DISTINGUISHABLE as "not found" rather than collapse into a generic
	// backend error, so a future caller could choose to retry the whole
	// capture on this specific condition.
	ctx := context.Background()
	ds := newMemDataSet(DataSetAttachments, "Ticket attachments")
	ds.put("a.jar", "aaa")
	ds.put("b.jar", "bbb")
	// Present at List time (put above), but Open reports it gone - simulating
	// the vanish-between-list-and-read race.
	ds.openErr["b.jar"] = fmt.Errorf("simulated vanish: %w", fs.ErrNotExist)

	st := newFakeManifestStore()
	_, _, err := CaptureManifest(ctx, ds, st, CaptureOptions{BackendLabel: "path:/x"})
	if err == nil {
		t.Fatal("CaptureManifest err = nil, want an abort on the vanished object")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("CaptureManifest err = %v, want errors.Is(err, fs.ErrNotExist) to stay true through the wrap", err)
	}
	// The message itself must name the distinction, not just the wrapped
	// sentinel: a generic "read %s: ..." wrap would ALSO satisfy
	// errors.Is(err, fs.ErrNotExist) here (fs.ErrNotExist is already inside
	// the injected error), so that assertion alone would not catch a
	// regression that collapsed this into the generic branch.
	if !strings.Contains(err.Error(), "vanished") {
		t.Errorf("CaptureManifest err = %q, want it to explicitly name the vanish-between-list-and-read case, not read it as a generic failure", err.Error())
	}
	if len(st.manifests) != 0 {
		t.Errorf("a partial manifest was persisted despite the vanished object: %+v", st.manifests)
	}
}

func TestCaptureManifest_PersistFailureIsWrappedAndDiscoverable(t *testing.T) {
	// fakeManifestStore.createErr existed and was honoured but no test ever
	// set it, so the "persist: %w" wrap in CaptureManifest was never
	// actually exercised - a dead fixture.
	ctx := context.Background()
	ds := newMemDataSet(DataSetAttachments, "Ticket attachments")
	ds.put("a.jar", "aaa")

	st := newFakeManifestStore()
	boom := errors.New("db unavailable")
	st.createErr = boom

	_, _, err := CaptureManifest(ctx, ds, st, CaptureOptions{BackendLabel: "path:/x"})
	if !errors.Is(err, boom) {
		t.Fatalf("CaptureManifest err = %v, want it to wrap %v", err, boom)
	}
	if !strings.Contains(err.Error(), "persist") {
		t.Errorf("CaptureManifest err = %q, want it to name the persist step", err.Error())
	}
}

func TestCaptureManifest_NeverMutatesTheSource(t *testing.T) {
	ctx := context.Background()
	ds := newMemDataSet(DataSetAttachments, "Ticket attachments")
	ds.put("a.jar", "aaa")
	ds.put("b.jar", "bbb")
	before := ds.snapshot()

	st := newFakeManifestStore()
	if _, _, err := CaptureManifest(ctx, ds, st, CaptureOptions{BackendLabel: "path:/x"}); err != nil {
		t.Fatalf("CaptureManifest: %v", err)
	}
	after := ds.snapshot()
	if len(before) != len(after) {
		t.Fatalf("object count changed: %d -> %d", len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed: %q -> %q", k, v, after[k])
		}
	}
	if len(ds.deleted) != 0 {
		t.Errorf("capture deleted %v from the source; manifesting must be read-only", ds.deleted)
	}
}

func TestCaptureManifest_ReportsProgress(t *testing.T) {
	ctx := context.Background()
	ds := newMemDataSet(DataSetAttachments, "Ticket attachments")
	ds.put("a.jar", "aa")
	ds.put("b.jar", "bbbb")

	type tick struct {
		done, total, bytesDone, bytesTotal int64
		key                                string
	}
	var ticks []tick
	st := newFakeManifestStore()
	_, _, err := CaptureManifest(ctx, ds, st, CaptureOptions{
		BackendLabel: "path:/x",
		Progress: func(done, total, bytesDone, bytesTotal int64, key string) {
			ticks = append(ticks, tick{done, total, bytesDone, bytesTotal, key})
		},
	})
	if err != nil {
		t.Fatalf("CaptureManifest: %v", err)
	}
	if len(ticks) != 2 {
		t.Fatalf("progress ticks = %d, want 2 (one per object)", len(ticks))
	}
	// The intermediate tick is what actually distinguishes "bytesTotal is the
	// listed total" from "bytesTotal is just a copy of the running bytesDone":
	// at the FINAL tick bytesDone == bytesTotal == 6 either way, since the
	// listing (List is sorted ascending by Key: a.jar then b.jar) totals 6
	// bytes and by the last object bytesDone has also reached 6. Only the
	// first tick (bytesDone=2 after a.jar, listed total still 6) tells the
	// two implementations apart.
	first := ticks[0]
	if first.bytesDone != 2 || first.bytesTotal != 6 {
		t.Errorf("first tick bytes = %d/%d, want 2/6 (bytes read so far / total bytes listed up front)", first.bytesDone, first.bytesTotal)
	}
	last := ticks[len(ticks)-1]
	if last.done != 2 || last.total != 2 {
		t.Errorf("final tick objects = %d/%d, want 2/2", last.done, last.total)
	}
	if last.bytesDone != 6 || last.bytesTotal != 6 {
		t.Errorf("final tick bytes = %d/%d, want 6/6", last.bytesDone, last.bytesTotal)
	}
}

func TestCaptureManifest_HonoursCancellationAtAnObjectBoundary(t *testing.T) {
	ctx := context.Background()
	ds := newMemDataSet(DataSetAttachments, "Ticket attachments")
	ds.put("a.jar", "aa")
	ds.put("b.jar", "bb")
	ds.put("c.jar", "cc")

	calls := 0
	st := newFakeManifestStore()
	_, _, err := CaptureManifest(ctx, ds, st, CaptureOptions{
		BackendLabel: "path:/x",
		Cancelled: func() bool {
			calls++
			return calls > 2 // let two objects through, then cancel
		},
	})
	if !errors.Is(err, ErrCaptureCancelled) {
		t.Fatalf("CaptureManifest err = %v, want ErrCaptureCancelled", err)
	}
	if len(st.manifests) != 0 {
		t.Errorf("a cancelled capture persisted a manifest: %+v", st.manifests)
	}
}

func TestCaptureManifest_LogsAChecksumHintDisagreementWithoutFailing(t *testing.T) {
	// The opportunistic modversions.sha512 cross-check: a disagreement is a
	// warning about PRE-EXISTING data, never a job failure, and it is never
	// persisted - the manifest checksum stays the fresh SHA-256.
	ctx := context.Background()
	ds := &hintingDataSet{
		memDataSet: newMemDataSet(DataSetModpacks, "Modpacks"),
		hints:      map[string]string{"a.jar": "0000000000000000"},
	}
	ds.put("a.jar", "aaa")

	var lines []string
	st := newFakeManifestStore()
	m, entries, err := CaptureManifest(ctx, ds, st, CaptureOptions{
		BackendLabel: "path:/x",
		Log:          func(l string) { lines = append(lines, l) },
	})
	if err != nil {
		t.Fatalf("CaptureManifest err = %v, want nil (a hint disagreement must not fail the job)", err)
	}
	if len(entries) != 1 || entries[0].Checksum != sha256Hex("aaa") {
		t.Fatalf("entries = %+v, want the fresh sha256, not the hint", entries)
	}
	if m.Algo != ChecksumAlgo {
		t.Errorf("Algo = %q, want %q", m.Algo, ChecksumAlgo)
	}
	found := false
	for _, l := range lines {
		if strings.Contains(l, "a.jar") && strings.Contains(strings.ToLower(l), "sha512") {
			found = true
		}
	}
	if !found {
		t.Errorf("no warning logged for the disagreeing hint; lines = %v", lines)
	}
}

func TestCaptureManifest_AgreeingHintLogsNothing(t *testing.T) {
	ctx := context.Background()
	// sha512 of "aaa", lower-case hex.
	ds := &hintingDataSet{
		memDataSet: newMemDataSet(DataSetModpacks, "Modpacks"),
		hints:      map[string]string{"a.jar": sha512Hex("aaa")},
	}
	ds.put("a.jar", "aaa")

	var lines []string
	st := newFakeManifestStore()
	if _, _, err := CaptureManifest(ctx, ds, st, CaptureOptions{
		BackendLabel: "path:/x",
		Log:          func(l string) { lines = append(lines, l) },
	}); err != nil {
		t.Fatalf("CaptureManifest: %v", err)
	}
	for _, l := range lines {
		if strings.Contains(strings.ToLower(l), "sha512") {
			t.Errorf("an agreeing hint produced a warning: %q", l)
		}
	}
}

// duplicateKeyDataSet wraps memDataSet and injects a duplicate ObjectRef into
// List's result, simulating an adapter whose enumeration accidentally yields
// the same key twice (a LocalProvider mirrored across N paths is the
// plausible real shape - see storage/modpack.ModpackStorageProvider's local
// backend). Local to this file; never redeclared.
type duplicateKeyDataSet struct {
	*memDataSet
	dupKey string
}

func (d *duplicateKeyDataSet) List(ctx context.Context) ([]ObjectRef, error) {
	refs, err := d.memDataSet.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ObjectRef, 0, len(refs)+1)
	for _, r := range refs {
		out = append(out, r)
		if r.Key == d.dupKey {
			out = append(out, r) // inject the duplicate right after the original
		}
	}
	return out, nil
}

var _ DataSet = (*duplicateKeyDataSet)(nil)

func TestCaptureManifest_DedupesADuplicateKeyFromListSoObjectCountStaysHonest(t *testing.T) {
	// Task 7 review carry-forward: storage_manifest_entries has
	// PRIMARY KEY (manifest_id, key) and CreateStorageManifest's insert uses
	// ON CONFLICT (manifest_id, key) DO NOTHING, so a duplicate key reaching
	// the store would be silently dropped there while the header's
	// ObjectCount (supplied by THIS function) would still count it - an
	// over-report of what the manifest actually holds. The fix must live
	// here, in the writer, since it cannot assume every DataSet.List
	// implementation is duplicate-free.
	ctx := context.Background()
	inner := newMemDataSet(DataSetAttachments, "Ticket attachments")
	inner.put("a.jar", "aaa")
	inner.put("b.jar", "bb")
	ds := &duplicateKeyDataSet{memDataSet: inner, dupKey: "a.jar"}

	var lines []string
	st := newFakeManifestStore()
	m, entries, err := CaptureManifest(ctx, ds, st, CaptureOptions{
		BackendLabel: "path:/x",
		Log:          func(l string) { lines = append(lines, l) },
	})
	if err != nil {
		t.Fatalf("CaptureManifest: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want 2 (the duplicate collapsed)", entries)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if seen[e.Key] {
			t.Fatalf("duplicate key %q reached the persisted entries: %+v", e.Key, entries)
		}
		seen[e.Key] = true
	}
	if m.ObjectCount != int64(len(entries)) {
		t.Errorf("ObjectCount = %d, must equal len(entries) = %d so it can never exceed what was actually persisted", m.ObjectCount, len(entries))
	}
	persisted, _ := st.ListStorageManifestEntries(m.ID)
	if len(persisted) != 2 {
		t.Errorf("persisted %d entries, want 2", len(persisted))
	}

	found := false
	for _, l := range lines {
		if strings.Contains(l, "a.jar") && strings.Contains(strings.ToLower(l), "duplicate") {
			found = true
		}
	}
	if !found {
		t.Errorf("no warning logged for the duplicate key; lines = %v", lines)
	}
}

func TestEntriesByKey(t *testing.T) {
	got := EntriesByKey([]models.StorageManifestEntry{
		{Key: "a", Size: 1, Checksum: "aa"},
		{Key: "b", Size: 2, Checksum: "bb"},
	})
	if len(got) != 2 || got["a"].Checksum != "aa" || got["b"].Size != 2 {
		t.Fatalf("EntriesByKey = %+v", got)
	}
	if len(EntriesByKey(nil)) != 0 {
		t.Error("EntriesByKey(nil) must be an empty, non-nil map")
	}
}

// hintingDataSet is a memDataSet that also implements ChecksumHinter. Local
// to this file; never redeclared.
type hintingDataSet struct {
	*memDataSet
	hints map[string]string
}

func (h *hintingDataSet) ChecksumHint(key string) (string, string, bool) {
	sum, ok := h.hints[key]
	if !ok || sum == "" {
		return "", "", false
	}
	return "sha512", sum, true
}

var _ ChecksumHinter = (*hintingDataSet)(nil)

func sha512Hex(s string) string {
	sum := sha512.Sum512([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---------- Security carry-forward: SanitizeBackendLabel ----------

func TestSanitizeBackendLabel(t *testing.T) {
	tests := []struct {
		name           string
		endpoint       string
		bucket, prefix string
		want           string
	}{
		{
			name:     "plain endpoint with no credentials passes through",
			endpoint: "https://minio.internal:9000",
			bucket:   "backups",
			prefix:   "library",
			want:     "s3:https://minio.internal:9000/backups/library",
		},
		{
			name:     "strips embedded userinfo credentials (scheme://user:pass@host)",
			endpoint: "https://AKIAEXAMPLE:supersecret@minio.internal",
			bucket:   "backups",
			prefix:   "library",
			want:     "s3:https://minio.internal/backups/library",
		},
		{
			name:     "strips embedded userinfo and keeps the port",
			endpoint: "https://AKIAEXAMPLE:supersecret@minio.internal:9000",
			bucket:   "b",
			prefix:   "p",
			want:     "s3:https://minio.internal:9000/b/p",
		},
		{
			name:     "strips percent-encoded userinfo",
			endpoint: "http://user%40name:p%40ss@minio.internal:9000",
			bucket:   "b",
			prefix:   "p",
			want:     "s3:http://minio.internal:9000/b/p",
		},
		{
			name:     "strips userinfo even without a URL scheme",
			endpoint: "AKIAEXAMPLE:supersecret@minio.internal:9000",
			bucket:   "b",
			prefix:   "p",
			want:     "s3:minio.internal:9000/b/p",
		},
		{
			name:     "endpoint with no userinfo and no scheme passes through unchanged",
			endpoint: "minio.internal:9000",
			bucket:   "b",
			prefix:   "p",
			want:     "s3:minio.internal:9000/b/p",
		},
		{
			name:     "endpoint that fails to parse as a URL and has no userinfo falls back to verbatim, no panic",
			endpoint: "https://exa\x7fmple.com", // DEL control char: net/url refuses to parse this
			bucket:   "b",
			prefix:   "p",
			want:     "s3:https://exa\x7fmple.com/b/p",
		},
		{
			name:     "empty endpoint",
			endpoint: "",
			bucket:   "b",
			prefix:   "p",
			want:     "s3:/b/p",
		},
		// The four rows below all carry BOTH userinfo and an element that
		// defeats url.Parse on every attempt, so neither parse in
		// sanitizeEndpoint can PROVE the endpoint is credential-free. Unlike
		// the "no userinfo" fallback row above, these must fail CLOSED
		// (redacted placeholder), never fall back to the raw string, or the
		// credential leaks into Postgres, the panel manifest list, and the
		// CSV export.
		{
			name:     "userinfo with a space in the password fails closed (space breaks url.Parse userinfo validation on both attempts)",
			endpoint: "https://AKIAEXAMPLE:super secret@minio.internal",
			bucket:   "b",
			prefix:   "p",
			want:     "s3:" + redactedEndpointPlaceholder + "/b/p",
		},
		{
			name:     "userinfo with a control character in the password fails closed",
			endpoint: "https://AKIAEXAMPLE:super\x7fsecret@minio.internal",
			bucket:   "b",
			prefix:   "p",
			want:     "s3:" + redactedEndpointPlaceholder + "/b/p",
		},
		{
			name:     "userinfo present but a control character elsewhere in the host fails closed",
			endpoint: "https://AKIAEXAMPLE:supersecret@exa\x7fmple.com",
			bucket:   "b",
			prefix:   "p",
			want:     "s3:" + redactedEndpointPlaceholder + "/b/p",
		},
		{
			name:     "userinfo present but a malformed bracket host fails closed",
			endpoint: "https://AKIAEXAMPLE:supersecret@[not a host]",
			bucket:   "b",
			prefix:   "p",
			want:     "s3:" + redactedEndpointPlaceholder + "/b/p",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeBackendLabel(tc.endpoint, tc.bucket, tc.prefix)
			if got != tc.want {
				t.Errorf("SanitizeBackendLabel(%q, %q, %q) = %q, want %q", tc.endpoint, tc.bucket, tc.prefix, got, tc.want)
			}
			if strings.Contains(got, "supersecret") || strings.Contains(got, "p%40ss") || strings.Contains(strings.ToLower(got), "akiaexample") {
				t.Errorf("SanitizeBackendLabel(%q, ...) = %q leaked a credential", tc.endpoint, got)
			}
		})
	}
}
