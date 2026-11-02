package packs

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/services/corestorage"

	"github.com/gorilla/mux"

	"dylaris-core/models"
	"dylaris-core/storage/modpack"
)

// serverPackFixture is a handler with one upload entry in a local modpack
// storage, and a fresh, private server-pack cache directory.
func serverPackFixture(t *testing.T) (*PacksHandler, *draftRenderStore, modpack.ModpackStorageProvider, *models.Pack, *models.PackBuild) {
	t.Helper()
	dir := t.TempDir()
	base := localModpackHandler(t, dir)
	st := &draftRenderStore{serverPackFakeStore: base.d.Store.(*serverPackFakeStore)}
	h := &PacksHandler{d: Deps{Store: st, ClusterSecret: "cluster-secret-under-test", CoreStorage: &corestorage.Manager{Store: st, ClusterSecret: "cluster-secret-under-test"}}}
	storeZip(t, dir, "uploads/test.zip", "mods/test.jar", []byte("jar-bytes"))
	st.content = []models.BuildContentEntry{uploadEntry("uploads/test.zip")}
	pack := &models.Pack{ID: 1, OwnerID: "u", InternalName: "P", InternalSlug: "p"}
	build := &models.PackBuild{ID: 1, PackID: 1, VersionString: "1.0", Channel: models.ChannelDraft}
	useServerPackCache(t)
	return h, st, modpackProviderAt(t, dir), pack, build
}

func useServerPackCache(t *testing.T) string {
	t.Helper()
	old := serverPackCacheDir
	serverPackCacheDir = t.TempDir()
	t.Cleanup(func() { serverPackCacheDir = old })
	return serverPackCacheDir
}

func cachedServerPacks(t *testing.T) []string {
	t.Helper()
	entries, _ := os.ReadDir(serverPackCacheDir)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func readServerPack(t *testing.T, f *os.File) []string {
	t.Helper()
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		t.Fatalf("cached file is not a zip: %v", err)
	}
	var names []string
	for _, zf := range zr.File {
		names = append(names, zf.Name)
	}
	return names
}

// Concurrent requests for one input share one render, an unchanged pack is
// served from the cache, and nothing is ever written to modpack storage.
func TestServerPackIsRenderedOncePerInputAndNeverStored(t *testing.T) {
	h, st, _, _, _ := serverPackFixture(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, err := h.openServerPack(ctx, st.content)
			if err != nil {
				t.Errorf("render: %v", err)
				return
			}
			f.Close()
		}()
	}
	wg.Wait()
	if got := cachedServerPacks(t); len(got) != 1 || !strings.HasSuffix(got[0], ".zip") {
		t.Fatalf("8 concurrent requests left %v in the cache, want one zip", got)
	}
	f, err := h.openServerPack(ctx, st.content)
	if err != nil {
		t.Fatal(err)
	}
	if names := readServerPack(t, f); len(names) != 1 || names[0] != "mods/test.jar" {
		t.Fatalf("server pack holds %v", names)
	}

	st.content[0].Side = models.SideBoth
	f, err = h.openServerPack(ctx, st.content)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got := cachedServerPacks(t); len(got) != 2 {
		t.Fatalf("a changed pack was not rendered under a new name: %v", got)
	}
	if len(serverPackRenders) != 0 {
		t.Error("a render slot is still held after the file was returned")
	}
	for _, fn := range []string{"openServerPack", "renderServerPackFile", "exportServerPack"} {
		if body := handlertest.FuncBody(t, "packs_server_export.go", fn); strings.Contains(body, ".Put(") || strings.Contains(body, ".PutStream(") {
			t.Errorf("%s writes to modpack storage", fn)
		}
	}
}

// The sweep removes what is older than the TTL, partial renders included, and
// leaves fresh files alone.
func TestServerPackCacheSweep(t *testing.T) {
	dir := useServerPackCache(t)
	now := time.Now()
	files := []struct {
		name string
		age  time.Duration
		kept bool
	}{
		{"fresh.zip", time.Minute, true},
		{"almost.zip", serverPackCacheTTL - time.Minute, true},
		{"old.zip", serverPackCacheTTL + time.Minute, false},
		{"abc-123.part", 2 * serverPackCacheTTL, false},
	}
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-f.age), now.Add(-f.age)); err != nil {
			t.Fatal(err)
		}
	}
	SweepServerPackCache(now)
	for _, f := range files {
		_, err := os.Stat(filepath.Join(dir, f.name))
		if kept := err == nil; kept != f.kept {
			t.Errorf("%s: kept=%v, want %v", f.name, kept, f.kept)
		}
	}
	serverPackCacheDir = filepath.Join(dir, "missing")
	SweepServerPackCache(now) // no cache directory yet: nothing to do, no panic
}

// With every slot taken the caller gets a retryable answer instead of queueing
// without limit, and the detached render gives up too.
func TestServerPackRenderGivesUpWhenSlotsStayBusy(t *testing.T) {
	h, st, _, _, _ := serverPackFixture(t)
	release := holdAllServerPackSlots(t)
	defer release()
	old := serverPackSlotWait
	serverPackSlotWait = 50 * time.Millisecond
	defer func() { serverPackSlotWait = old }()

	if _, err := h.openServerPack(context.Background(), st.content); !errors.Is(err, errServerPackBusy) {
		t.Fatalf("err = %v, want errServerPackBusy", err)
	}
	release()
	time.Sleep(200 * time.Millisecond)
	if got := cachedServerPacks(t); len(got) != 0 {
		t.Errorf("a render that found no slot ran later anyway: %v", got)
	}
}

func holdAllServerPackSlots(t *testing.T) (release func()) {
	t.Helper()
	for i := 0; i < cap(serverPackRenders); i++ {
		serverPackRenders <- struct{}{}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			for i := 0; i < cap(serverPackRenders); i++ {
				<-serverPackRenders
			}
		})
	}
}

// A cached pack is served without waiting for a render slot.
func TestCachedServerPackNeedsNoRenderSlot(t *testing.T) {
	h, st, _, _, _ := serverPackFixture(t)
	f, err := h.openServerPack(context.Background(), st.content)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer holdAllServerPackSlots(t)()
	old := serverPackSlotWait
	serverPackSlotWait = 50 * time.Millisecond
	defer func() { serverPackSlotWait = old }()

	f, err = h.openServerPack(context.Background(), st.content)
	if err != nil {
		t.Fatalf("cached pack not served while slots are busy: %v", err)
	}
	f.Close()
}

// The hourly update check stamps every mod row it looks at. Hashing the whole
// content row made each stamp a new key and a full re-render of a pack nothing
// in had changed.
func TestServerPackFingerprintCoversOnlyWhatTheRenderReads(t *testing.T) {
	a := uploadEntry("uploads/a.zip")
	a.TargetPath = "mods/a.jar"
	b := uploadEntry("uploads/b.zip")
	b.TargetPath = "mods/b.jar"
	base, _ := serverPackFingerprint(sortServerPackContent([]models.BuildContentEntry{a, b}))

	stamped := a
	stamped.UpdatedAt = time.Now()
	stamped.PrettyName = "renamed"
	now := time.Now()
	stamped.ModrinthLastChecked = &now
	if fp, _ := serverPackFingerprint(sortServerPackContent([]models.BuildContentEntry{b, stamped})); fp != base {
		t.Error("a timestamp, a display name or the row order changed the key")
	}
	changed := a
	changed.SHA1 = "different"
	if fp, _ := serverPackFingerprint([]models.BuildContentEntry{changed, b}); fp == base {
		t.Error("a different file kept the same key")
	}
}

// A pack that fails to render is not rendered again on the next hit.
func TestServerPackFailureIsNotRetriedAtOnce(t *testing.T) {
	h, st, _, _, _ := serverPackFixture(t)
	st.content = append(st.content, uploadEntry("")) // missing storage key: fails
	if _, err := h.openServerPack(context.Background(), st.content); err == nil {
		t.Fatal("a broken pack rendered")
	}
	if got := cachedServerPacks(t); len(got) != 0 {
		t.Errorf("a failed render left %v behind", got)
	}
	defer holdAllServerPackSlots(t)()
	old := serverPackSlotWait
	serverPackSlotWait = 50 * time.Millisecond
	defer func() { serverPackSlotWait = old }()

	_, err := h.openServerPack(context.Background(), st.content)
	if err == nil || errors.Is(err, errServerPackBusy) {
		t.Fatalf("err = %v, want the held failure without asking for a slot", err)
	}
}

// The first request for a large pack answers within the request wait instead of
// holding the connection for the whole render, and the render finishes anyway.
func TestServerPackRequestDoesNotWaitForTheWholeRender(t *testing.T) {
	h, st, _, _, _ := serverPackFixture(t)
	release := holdAllServerPackSlots(t)
	defer release()
	old := serverPackRequestWait
	serverPackRequestWait = 50 * time.Millisecond
	defer func() { serverPackRequestWait = old }()

	if _, err := h.openServerPack(context.Background(), st.content); !errors.Is(err, errServerPackBusy) {
		t.Fatalf("err = %v, want errServerPackBusy", err)
	}
	release()
	for i := 0; i < 100; i++ {
		if got := cachedServerPacks(t); len(got) == 1 && strings.HasSuffix(got[0], ".zip") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the render did not carry on after the request gave up")
}

// The store orders build content by mod slug only, so rows can come back in a
// different order between requests; that must not be a new render.
func TestServerPackKeyIgnoresTheRowOrder(t *testing.T) {
	h, st, prov, _, _ := serverPackFixture(t)
	if err := prov.Put(context.Background(), "uploads/other.zip", zipBytes(t, map[string]string{"zz/other": "another artifact"})); err != nil {
		t.Fatal(err)
	}
	second := uploadEntry("uploads/other.zip")
	second.TargetPath = "zz/other"
	for _, content := range [][]models.BuildContentEntry{{st.content[0], second}, {second, st.content[0]}} {
		f, err := h.openServerPack(context.Background(), content)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	if got := cachedServerPacks(t); len(got) != 1 {
		t.Fatalf("row order changed the render: %v", got)
	}
}

// Every old public share link answers 410 Gone with a message, whatever the
// token, and never a pack.
func TestRemovedShareRouteAnswersGone(t *testing.T) {
	h := &PacksHandler{d: Deps{CoreStorage: &corestorage.Manager{}}}
	r := mux.NewRouter()
	r.HandleFunc("/api/share/{token}", h.ServeRemovedShare).Methods("GET")
	for _, token := range []string{"tok", "a-real-looking-token-0123456789"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/share/"+token, nil))
		body, _ := io.ReadAll(rec.Body)
		if rec.Code != http.StatusGone || !strings.Contains(string(body), "removed") {
			t.Errorf("%s: %d %s, want 410 with a message", token, rec.Code, body)
		}
	}
}
