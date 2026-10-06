package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"math/rand"
	"os"
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

// draftRenderStore serves the content list ensureInstallMrpack renders from.
type draftRenderStore struct {
	*serverPackFakeStore
	content []models.BuildContentEntry
}

func (s *draftRenderStore) ListBuildContent(int) ([]models.BuildContentEntry, error) {
	return s.content, nil
}

// A share link renders a draft at most once a minute while it is unchanged, and
// at once when it changed. Installs always render.
func TestDraftShareRendersOnlyWhenChangedOrStale(t *testing.T) {
	dir := t.TempDir()
	base := localModpackHandler(t, dir)
	st := &draftRenderStore{serverPackFakeStore: base.state.Store.(*serverPackFakeStore)}
	h := &PacksHandler{state: &AppState{Store: st, ClusterSecret: "cluster-secret-under-test"}}
	prov := modpackProviderAt(t, dir)

	storeZip(t, dir, "packs/u/mods/cfg/cfg-u-1.zip", "config/a.cfg", []byte("a=1"))
	st.content = []models.BuildContentEntry{{
		Modversion: models.Modversion{ID: 1, StorageKey: "packs/u/mods/cfg/cfg-u-1.zip", Source: models.SourceUpload, TargetPath: "config/a.cfg"},
		Side:       models.SideBoth, ContentType: models.ContentTypeConfig,
	}}
	pack := &models.Pack{ID: 1, OwnerID: "u", InternalName: "P", InternalSlug: "p"}
	build := &models.PackBuild{ID: 1, PackID: 1, VersionString: "1.0", Minecraft: "1.20.1", Loader: "fabric", LoaderVersion: "0.15.0", Channel: models.ChannelDraft}
	key := h.mrpackStorageKey(pack, build)
	t.Cleanup(func() { draftShareRenders.Delete(key) })

	ctx := context.Background()
	sentinel := []byte("not-a-fresh-render")
	fetch := func(get func(context.Context, *models.Pack, *models.PackBuild) (string, error)) bool {
		t.Helper()
		got, err := get(ctx, pack, build)
		if err != nil || got != key {
			t.Fatalf("key = %q, %v", got, err)
		}
		b, err := prov.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		return !bytes.Equal(b, sentinel)
	}
	plant := func() {
		t.Helper()
		if err := prov.Put(ctx, key, sentinel); err != nil {
			t.Fatal(err)
		}
	}

	fetch(h.shareMrpackKey)
	plant()
	if fetch(h.shareMrpackKey) {
		t.Fatal("an unchanged draft was rendered again within the minute")
	}

	st.content[0].Side = models.SideClient
	if !fetch(h.shareMrpackKey) {
		t.Fatal("a changed draft was not rendered again")
	}

	plant()
	v, _ := draftShareRenders.Load(key)
	last := v.(draftShareRender)
	last.at = last.at.Add(-draftShareReuse)
	draftShareRenders.Store(key, last)
	if !fetch(h.shareMrpackKey) {
		t.Fatal("an unchanged draft was reused after the minute")
	}

	plant()
	if !fetch(h.ensureInstallMrpack) {
		t.Fatal("an install reused a stored draft")
	}

	if err := prov.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if !fetch(h.shareMrpackKey) {
		t.Fatal("a missing object was not rendered again")
	}
	b, _ := prov.Get(ctx, key)
	if _, err := zip.NewReader(bytes.NewReader(b), int64(len(b))); err != nil {
		t.Fatalf("rendered object is not a zip: %v", err)
	}
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

// Revoking a share link is not behind the authoring switches that stop new
// links, and the share route reuses a draft render instead of always rendering.
func TestShareRoutesAreBounded(t *testing.T) {
	if body := funcBody(t, "packs_share.go", "ServeShare"); !strings.Contains(body, "h.shareMrpackKey(r.Context(), pack, build)") {
		t.Error("the share route renders a draft on every hit")
	}
	b, err := os.ReadFile("../routes.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `RequireCap("modpack.delete")(appState.AllowReadOnlyWhenDisabled(packsHandler.RevokeShareLink))`) {
		t.Error("revoking a share link is gated on authoring")
	}
}
