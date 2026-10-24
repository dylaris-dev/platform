package packs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/store"
)

// TestCountingWriter pins the byte tracking the streaming share handler relies
// on to tell "failed before any output" (a clean error is still possible) from
// "failed mid-stream" (only a log is left).
func TestCountingWriter(t *testing.T) {
	var sink bytes.Buffer
	cw := &countingWriter{w: &sink}

	if cw.written != 0 {
		t.Fatalf("written = %d before any write, want 0", cw.written)
	}
	n, err := cw.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
	}
	cw.Write([]byte("!!"))
	if cw.written != 7 {
		t.Errorf("written = %d, want 7", cw.written)
	}
	if sink.String() != "hello!!" {
		t.Errorf("sink = %q, want the bytes passed through", sink.String())
	}
}

// serverPackFakeStore serves only the two settings NewProviderFromSettings reads
// for a local modpack backend. Everything else falls through to the embedded
// nil Store, which panics if touched - a guard that the render path reads
// nothing else from the store.
type serverPackFakeStore struct {
	store.Store
	settings map[string]string
}

func (f *serverPackFakeStore) GetSetting(key string) (string, error) {
	return f.settings[key], nil
}

// localModpackHandler wires a PacksHandler at a local modpack backend rooted at
// dir, the cheapest real provider to exercise renderServerPack end to end.
func localModpackHandler(t *testing.T, dir string) *PacksHandler {
	t.Helper()
	paths, _ := json.Marshal([]string{dir})
	return &PacksHandler{state: &handlers.AppState{
		Store: &serverPackFakeStore{settings: map[string]string{
			"modpack_storage_provider": "local",
			"modpack_storage_paths":    string(paths),
		}},
	}}
}

// storeZip writes a zip holding one file at entryName into the local modpack
// backend under key, the shape a SourceUpload content entry points at.
func storeZip(t *testing.T, dir, key, entryName string, body []byte) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(entryName)
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	full := filepath.Join(dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write stored zip: %v", err)
	}
}

func uploadEntry(key string) models.BuildContentEntry {
	e := models.BuildContentEntry{Side: models.SideServer, ModSlug: "test-mod"}
	e.StorageKey = key
	e.Source = models.SourceUpload
	return e
}

// TestRenderServerPack_StreamsAValidZip proves the refactor still produces a
// correct pack, now written to the destination rather than returned as a buffer.
func TestRenderServerPack_StreamsAValidZip(t *testing.T) {
	dir := t.TempDir()
	storeZip(t, dir, "uploads/test.zip", "mods/test.jar", []byte("jar-bytes"))
	h := localModpackHandler(t, dir)

	var out bytes.Buffer
	if err := h.renderServerPack(context.Background(), []models.BuildContentEntry{uploadEntry("uploads/test.zip")}, &out); err != nil {
		t.Fatalf("renderServerPack: %v", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatalf("output is not a valid zip: %v", err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b := new(bytes.Buffer)
		b.ReadFrom(rc)
		rc.Close()
		got[f.Name] = b.String()
	}
	if got["mods/test.jar"] != "jar-bytes" {
		t.Fatalf("output zip = %v, want mods/test.jar with the stored bytes", got)
	}
}

// TestRenderServerPack_RejectsUnsafeEntryWithoutWritingIt is the security half.
// A stored zip whose entry names traverse out of the extraction root must abort
// the render, and - because the output is now streamed - the unsafe content
// must not have been written to the destination before the abort.
func TestRenderServerPack_RejectsUnsafeEntryWithoutWritingIt(t *testing.T) {
	dir := t.TempDir()
	storeZip(t, dir, "uploads/evil.zip", "../../etc/pwned", []byte("evil-bytes"))
	h := localModpackHandler(t, dir)

	var out bytes.Buffer
	err := h.renderServerPack(context.Background(), []models.BuildContentEntry{uploadEntry("uploads/evil.zip")}, &out)
	if err == nil {
		t.Fatal("renderServerPack accepted a zip with a traversal entry, want an error")
	}
	if !strings.Contains(err.Error(), "unsafe") {
		t.Errorf("error = %v, want it to name the unsafe entry", err)
	}
	if strings.Contains(out.String(), "evil-bytes") {
		t.Fatal("the unsafe content was written to the destination before the abort")
	}
}

// missingKeyEntry pins that a first-entry failure returns cleanly (nothing
// written), which is what lets the handler still send a 500 rather than a
// truncated 200.
func TestRenderServerPack_FirstEntryErrorWritesNothing(t *testing.T) {
	dir := t.TempDir()
	h := localModpackHandler(t, dir)

	var out bytes.Buffer
	err := h.renderServerPack(context.Background(), []models.BuildContentEntry{uploadEntry("uploads/missing.zip")}, &out)
	if err == nil {
		t.Fatal("renderServerPack on a missing key returned nil, want an error")
	}
	if out.Len() != 0 {
		t.Fatalf("wrote %d bytes on a first-entry failure, want 0 so the caller can still send a clean error", out.Len())
	}
}

// TestSpoolToTemp pins the primitive the constant-memory render rests on: it
// copies within the limit, rewinds so the caller can read from the start, and
// rejects anything over the limit without having buffered it.
func TestSpoolToTemp(t *testing.T) {
	t.Run("within limit, rewound", func(t *testing.T) {
		f, n, err := spoolToTemp(strings.NewReader("hello world"), 100)
		if err != nil {
			t.Fatalf("spoolToTemp: %v", err)
		}
		defer cleanupTemp(f)
		if n != 11 {
			t.Errorf("size = %d, want 11", n)
		}
		got, _ := io.ReadAll(f)
		if string(got) != "hello world" {
			t.Errorf("content = %q, want it readable from the start", got)
		}
	})

	t.Run("over limit is rejected", func(t *testing.T) {
		f, _, err := spoolToTemp(strings.NewReader("way too many bytes"), 5)
		if err == nil {
			cleanupTemp(f)
			t.Fatal("spoolToTemp accepted a source over the limit, want an error")
		}
	})
}

// TestCopyPackEntry_EnforcesEntryCapOnAStream is the security-relevant one: the
// per-entry cap must hold even when the source lies about (or does not declare)
// its size, because the bytes now flow through a stream rather than a measured
// []byte. A zip header claiming 1 byte must not be able to write 1 GiB.
func TestCopyPackEntry_EnforcesEntryCapOnAStream(t *testing.T) {
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	total := &packSizeBudget{limit: 1 << 30}

	// 20 bytes into a 10-byte-capped entry.
	err := copyPackEntry(zw, "big.bin", strings.NewReader("0123456789abcdefghij"), 10, total)
	if err == nil {
		t.Fatal("copyPackEntry wrote a source larger than the entry cap, want an error")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error = %v, want it to name the cap", err)
	}
}

// TestCopyPackEntry_EnforcesTotalBudget: the running total spans entries, so a
// pack that stays under the per-entry cap but exceeds the total is still caught.
func TestCopyPackEntry_EnforcesTotalBudget(t *testing.T) {
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	total := &packSizeBudget{limit: 15}

	if err := copyPackEntry(zw, "a", strings.NewReader("0123456789"), 100, total); err != nil {
		t.Fatalf("first entry: %v", err)
	}
	if err := copyPackEntry(zw, "b", strings.NewReader("0123456789"), 100, total); err == nil {
		t.Fatal("second entry pushed the total to 20 over a 15 budget, want an error")
	}
}

// TestRenderServerPack_StreamsLargeUploadContentWithoutBuffering renders a
// stored zip whose inner file is several MB - comfortably past any single copy
// buffer - and checks the output is byte-correct. It exercises the
// spool-to-temp then stream-out path end to end; the memory stays flat by
// construction (io.Copy buffers, never the whole object).
func TestRenderServerPack_StreamsLargeUploadContentWithoutBuffering(t *testing.T) {
	dir := t.TempDir()
	big := bytes.Repeat([]byte("A"), 3<<20) // 3 MiB
	storeZip(t, dir, "uploads/big.zip", "mods/big.jar", big)
	h := localModpackHandler(t, dir)

	var out bytes.Buffer
	if err := h.renderServerPack(context.Background(), []models.BuildContentEntry{uploadEntry("uploads/big.zip")}, &out); err != nil {
		t.Fatalf("renderServerPack: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatalf("output is not a valid zip: %v", err)
	}
	var got []byte
	for _, f := range zr.File {
		if f.Name == "mods/big.jar" {
			rc, _ := f.Open()
			got, _ = io.ReadAll(rc)
			rc.Close()
		}
	}
	if !bytes.Equal(got, big) {
		t.Fatalf("streamed entry = %d bytes, want the %d stored bytes intact", len(got), len(big))
	}
}
