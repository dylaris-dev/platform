package packs

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"runtime"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/storage/modpack"
)

// The wrap path stores exactly what the buffered wrappers produced, so the key
// of a re-uploaded file (derived from the stored sha1) does not move.
func TestWrappedUploadStoresTheSameBytesAsTheBufferedWrapper(t *testing.T) {
	dir := t.TempDir()
	h := localModpackHandler(t, dir)
	prov := modpackProviderAt(t, dir)
	// Larger than one io.Copy chunk, and incompressible, so the streamed zip is
	// written in many pieces.
	raw := make([]byte, 200<<10)
	rand.New(rand.NewSource(61)).Read(raw)

	cases := []struct {
		name, contentType string
		want              func() ([]byte, error)
	}{
		{"cool.jar", models.ContentTypeMod, func() ([]byte, error) { return modpack.WrapJarAsContentZip("cool.jar", raw) }},
		{"pack.zip", models.ContentTypeResourcepack, func() ([]byte, error) {
			return modpack.BuildContentZip(targetPathFor(models.ContentTypeResourcepack, "pack.zip"), raw)
		}},
	}
	for _, c := range cases {
		want, err := c.want()
		if err != nil {
			t.Fatal(err)
		}
		f, hdr := multipartFrom(raw)
		meta, herr := h.storeUploadedContent(context.Background(), prov, f, hdr, c.name, c.contentType, "user-1", "slug")
		if herr != nil {
			t.Fatalf("%s: %d %s", c.name, herr.status, herr.msg)
		}
		got, err := prov.Get(context.Background(), meta.key)
		if err != nil {
			t.Fatal(err)
		}
		wantMD5, wantSHA1, _ := hashesOf(want)
		_, rawSHA1, rawSHA512 := hashesOf(raw)
		if !bytes.Equal(got, want) || meta.size != int64(len(want)) || meta.md5 != wantMD5 {
			t.Errorf("%s: stored object differs from the buffered wrapper", c.name)
		}
		if wantKey, _ := uploadKey("user-1", "slug", wantSHA1); meta.key != wantKey {
			t.Errorf("%s: key = %s, want %s", c.name, meta.key, wantKey)
		}
		if meta.innerSha1 != rawSHA1 || meta.innerSha512 != rawSHA512 {
			t.Errorf("%s: inner hashes are not of the raw upload", c.name)
		}
	}
}

// A large jar is wrapped through a temp file: the heap does not grow with it.
func TestWrappedUploadDoesNotBufferTheFile(t *testing.T) {
	dir := t.TempDir()
	h := localModpackHandler(t, dir)
	prov := modpackProviderAt(t, dir)
	const size = 64 << 20
	f, hdr := multipartFrom(make([]byte, size))

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if _, herr := h.storeUploadedContent(context.Background(), prov, f, hdr, "big.jar", models.ContentTypeMod, "user-1", "big"); herr != nil {
		t.Fatalf("%d %s", herr.status, herr.msg)
	}
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > size/4 {
		t.Fatalf("wrapping a %d byte jar allocated %d bytes", size, alloc)
	}
}

// draftRenderStore serves the content list EnsureInstallMrpack renders from.
type draftRenderStore struct {
	*serverPackFakeStore
	content []models.BuildContentEntry
}

func (s *draftRenderStore) ListBuildContent(int) ([]models.BuildContentEntry, error) {
	return s.content, nil
}

// The text editor reads at most one editable entry's worth of a stored object.
func TestTextEditorRefusesAnOversizedStoredObject(t *testing.T) {
	dir := t.TempDir()
	prov := modpackProviderAt(t, dir)
	ctx := context.Background()
	if err := prov.Put(ctx, "big.zip", make([]byte, maxEditableObjectBytes+1)); err != nil {
		t.Fatal(err)
	}
	if _, err := readEditableObject(ctx, prov, "big.zip"); !errors.Is(err, errNotEditableObject) {
		t.Fatalf("oversized object: err = %v", err)
	}
	if err := prov.Put(ctx, "ok.zip", make([]byte, maxEditableObjectBytes)); err != nil {
		t.Fatal(err)
	}
	if b, err := readEditableObject(ctx, prov, "ok.zip"); err != nil || len(b) != maxEditableObjectBytes {
		t.Fatalf("object at the cap: %d bytes, %v", len(b), err)
	}
	for _, fn := range []string{"GetContentText", "SetContentText"} {
		if body := funcBody(t, "packs_text.go", fn); strings.Contains(body, "prov.Get(") || !strings.Contains(body, "readEditableObject(") {
			t.Errorf("%s does not read through readEditableObject", fn)
		}
	}
}
