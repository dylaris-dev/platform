package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"mime/multipart"
	"testing"

	"dylaris-core/models"
	"dylaris-core/storage/modpack"
)

// packUploadFile adapts a bytes.Reader to multipart.File: it already has
// Read, ReadAt and Seek, so only Close is missing. The upload path needs the
// ReaderAt + Seeker to validate and hash without buffering.
type packUploadFile struct{ *bytes.Reader }

func (packUploadFile) Close() error { return nil }

func multipartFrom(b []byte) (multipart.File, *multipart.FileHeader) {
	return packUploadFile{bytes.NewReader(b)}, &multipart.FileHeader{Size: int64(len(b))}
}

func zipBytes(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func hashesOf(b []byte) (md5hex, sha1hex, sha512hex string) {
	m := md5.Sum(b)
	s1 := sha1.Sum(b)
	s5 := sha512.Sum512(b)
	return hex.EncodeToString(m[:]), hex.EncodeToString(s1[:]), hex.EncodeToString(s5[:])
}

func modpackProviderAt(t *testing.T, dir string) modpack.ModpackStorageProvider {
	t.Helper()
	h := localModpackHandler(t, dir)
	prov, err := modpack.NewProviderFromSettings(h.state.Store.GetSetting, h.state.buildCoreStorageProvider)
	if err != nil || prov == nil {
		t.Fatalf("provider: %v", err)
	}
	return prov
}

// TestStoreUploadedContent_StreamsAStoredZip is the constant-memory upload path:
// a pre-built content .zip is stored as-is via PutStream, its hashes are the raw
// upload's hashes, and the object is retrievable byte-for-byte.
func TestStoreUploadedContent_StreamsAStoredZip(t *testing.T) {
	dir := t.TempDir()
	h := localModpackHandler(t, dir)
	prov := modpackProviderAt(t, dir)

	raw := zipBytes(t, map[string]string{"config/foo.cfg": "hello"})
	wantMD5, wantSHA1, wantSHA512 := hashesOf(raw)

	f, hdr := multipartFrom(raw)
	meta, herr := h.storeUploadedContent(context.Background(), prov, f, hdr, "bundle.zip", models.ContentTypeMod, "user-1", "bundle")
	if herr != nil {
		t.Fatalf("storeUploadedContent: %d %s", herr.status, herr.msg)
	}

	if meta.size != int64(len(raw)) {
		t.Errorf("size = %d, want %d", meta.size, len(raw))
	}
	if meta.md5 != wantMD5 || meta.innerSha1 != wantSHA1 || meta.innerSha512 != wantSHA512 {
		t.Errorf("hashes differ from the raw upload:\n md5 %s vs %s\n sha1 %s vs %s\n sha512 %s vs %s",
			meta.md5, wantMD5, meta.innerSha1, wantSHA1, meta.innerSha512, wantSHA512)
	}
	got, err := prov.Get(context.Background(), meta.key)
	if err != nil {
		t.Fatalf("Get stored object: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("stored object differs from the upload (%d vs %d bytes)", len(got), len(raw))
	}
}

// TestStoreUploadedContent_RejectsUnsafeZip is the security guard: a stored zip
// whose entry names traverse out of the root is refused with a 400, before it
// is stored.
func TestStoreUploadedContent_RejectsUnsafeZip(t *testing.T) {
	dir := t.TempDir()
	h := localModpackHandler(t, dir)
	prov := modpackProviderAt(t, dir)

	evil := zipBytes(t, map[string]string{"../../etc/pwned": "x"})
	f, hdr := multipartFrom(evil)
	_, herr := h.storeUploadedContent(context.Background(), prov, f, hdr, "evil.zip", models.ContentTypeMod, "user-1", "evil")
	if herr == nil {
		t.Fatal("storeUploadedContent accepted a traversal-bearing zip, want a 400")
	}
	if herr.status != 400 {
		t.Errorf("status = %d, want 400", herr.status)
	}
}

// TestStoreUploadedContent_WrapsAJar covers the buffered wrap path: a raw jar is
// wrapped into a content zip at mods/<file> and stored, with the inner hashes
// taken over the raw jar (for Modrinth linking) and the stored hashes over the
// wrapped zip.
func TestStoreUploadedContent_WrapsAJar(t *testing.T) {
	dir := t.TempDir()
	h := localModpackHandler(t, dir)
	prov := modpackProviderAt(t, dir)

	jar := []byte("this-is-a-jar")
	_, rawSHA1, rawSHA512 := hashesOf(jar)

	f, hdr := multipartFrom(jar)
	meta, herr := h.storeUploadedContent(context.Background(), prov, f, hdr, "cool.jar", models.ContentTypeMod, "user-1", "cool")
	if herr != nil {
		t.Fatalf("storeUploadedContent: %d %s", herr.status, herr.msg)
	}
	if meta.innerSha1 != rawSHA1 || meta.innerSha512 != rawSHA512 {
		t.Error("inner hashes should be of the raw jar, for Modrinth auto-link")
	}
	got, err := prov.Get(context.Background(), meta.key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(got), int64(len(got)))
	if err != nil {
		t.Fatalf("stored object is not a zip: %v", err)
	}
	found := false
	for _, e := range zr.File {
		if e.Name == "mods/cool.jar" {
			found = true
		}
	}
	if !found {
		t.Fatal("wrapped zip does not contain mods/cool.jar")
	}
}
