package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/storage/modpack"
)

// countingProvider counts stored server packs, so a render that happened can be
// told apart from one that was reused.
type countingProvider struct {
	modpack.ModpackStorageProvider
	puts atomic.Int32
}

func (c *countingProvider) PutStream(ctx context.Context, key string, r io.Reader, size int64) error {
	if strings.Contains(key, "/server-") {
		c.puts.Add(1)
		time.Sleep(20 * time.Millisecond) // widen the window concurrent callers race in
	}
	return c.ModpackStorageProvider.PutStream(ctx, key, r, size)
}

func serverPackFixture(t *testing.T) (*PacksHandler, *draftRenderStore, *countingProvider, *models.Pack, *models.PackBuild) {
	t.Helper()
	dir := t.TempDir()
	base := localModpackHandler(t, dir)
	st := &draftRenderStore{serverPackFakeStore: base.state.Store.(*serverPackFakeStore)}
	h := &PacksHandler{state: &AppState{Store: st, ClusterSecret: "cluster-secret-under-test"}}
	storeZip(t, dir, "uploads/test.zip", "mods/test.jar", []byte("jar-bytes"))
	st.content = []models.BuildContentEntry{uploadEntry("uploads/test.zip")}
	pack := &models.Pack{ID: 1, OwnerID: "u", InternalName: "P", InternalSlug: "p"}
	build := &models.PackBuild{ID: 1, PackID: 1, VersionString: "1.0", Channel: models.ChannelDraft}
	return h, st, &countingProvider{ModpackStorageProvider: modpackProviderAt(t, dir)}, pack, build
}

// The share route built the whole server zip on every request, straight into
// the response, with nothing bounding how many ran: one anonymous link holder
// could keep Core's disk and CPU busy for every tenant. It is now rendered once
// per distinct input, concurrent requests share that render, and the stored
// object is what gets served.
func TestServerPackIsRenderedOncePerInput(t *testing.T) {
	h, st, prov, pack, build := serverPackFixture(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	keys := make([]string, 8)
	for i := range keys {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k, err := h.shareServerPackKey(ctx, prov, pack, build, st.content)
			if err != nil {
				t.Errorf("render: %v", err)
			}
			keys[i] = k
		}(i)
	}
	wg.Wait()
	if n := prov.puts.Load(); n != 1 {
		t.Fatalf("8 concurrent requests stored %d server packs, want 1", n)
	}
	first := keys[0]
	b, err := prov.Get(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil || len(zr.File) != 1 || zr.File[0].Name != "mods/test.jar" {
		t.Fatalf("stored object is not the server pack: %v", err)
	}

	if _, err := h.shareServerPackKey(ctx, prov, pack, build, st.content); err != nil || prov.puts.Load() != 1 {
		t.Fatalf("an unchanged pack was rendered again (puts=%d, %v)", prov.puts.Load(), err)
	}

	st.content[0].Side = models.SideBoth
	second, err := h.shareServerPackKey(ctx, prov, pack, build, st.content)
	if err != nil || second == first || prov.puts.Load() != 2 {
		t.Fatalf("a changed pack was not rendered under a new key (puts=%d, %v)", prov.puts.Load(), err)
	}
	if _, exists, _ := prov.Stat(ctx, first); exists {
		t.Error("the previous render was left behind after the pack changed")
	}
	if len(serverPackRenders) != 0 {
		t.Error("a render slot is still held after the key was returned")
	}
}

// With every slot taken the caller gets a retryable answer instead of queueing
// without limit.
func TestServerPackRenderGivesUpWhenSlotsStayBusy(t *testing.T) {
	h, st, prov, pack, build := serverPackFixture(t)
	release := holdAllServerPackSlots(t)
	defer release()
	old := serverPackSlotWait
	serverPackSlotWait = 50 * time.Millisecond
	defer func() { serverPackSlotWait = old }()

	_, err := h.shareServerPackKey(context.Background(), prov, pack, build, st.content)
	if !errors.Is(err, errServerPackBusy) {
		t.Fatalf("err = %v, want errServerPackBusy", err)
	}
	// The detached render gave up too, rather than queueing for a slot: with
	// one waiting render per distinct pack nothing would bound the backlog.
	release()
	time.Sleep(200 * time.Millisecond)
	if prov.puts.Load() != 0 {
		t.Error("a render that found no slot ran later anyway")
	}
}

type shareRouteStore struct {
	*draftRenderStore
	pack  *models.Pack
	build *models.PackBuild
}

func (s *shareRouteStore) GetSetting(key string) (string, error) {
	if key == "feature_modpacks_enabled" {
		return "true", nil
	}
	return s.draftRenderStore.GetSetting(key)
}
func (s *shareRouteStore) GetShareLinkByToken(string) (*models.ShareLink, error) {
	return &models.ShareLink{ID: 1, BuildID: s.build.ID, Kind: models.ShareLinkServerPack}, nil
}
func (s *shareRouteStore) GetPackBuild(int) (*models.PackBuild, error) { return s.build, nil }
func (s *shareRouteStore) GetPack(int) (*models.Pack, error)           { return s.pack, nil }

// The route serves the stored object rather than rendering into the response.
func TestShareRouteServesTheStoredServerPack(t *testing.T) {
	h, st, prov, pack, build := serverPackFixture(t)
	rs := &shareRouteStore{draftRenderStore: st, pack: pack, build: build}
	h.state.Store = rs
	h.state.FeatureFlags = services.NewFeatureFlags(rs)

	key, err := h.shareServerPackKey(context.Background(), prov, pack, build, st.content)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := []byte("served-from-storage")
	if err := prov.Put(context.Background(), key, sentinel); err != nil {
		t.Fatal(err)
	}

	r := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/api/share/tok", nil), map[string]string{"token": "tok"})
	rec := httptest.NewRecorder()
	h.ServeShare(rec, r)

	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), sentinel) {
		t.Fatalf("status %d, body %q: want the stored object", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "p-1.0-server.zip") {
		t.Errorf("Content-Disposition = %q", cd)
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

// A pack already in storage is served without waiting for a render slot, so a
// burst of new renders cannot make the stored ones unavailable.
func TestStoredServerPackNeedsNoRenderSlot(t *testing.T) {
	h, st, prov, pack, build := serverPackFixture(t)
	key, err := h.shareServerPackKey(context.Background(), prov, pack, build, st.content)
	if err != nil {
		t.Fatal(err)
	}
	defer holdAllServerPackSlots(t)()
	old := serverPackSlotWait
	serverPackSlotWait = 50 * time.Millisecond
	defer func() { serverPackSlotWait = old }()

	got, err := h.shareServerPackKey(context.Background(), prov, pack, build, st.content)
	if err != nil || got != key {
		t.Fatalf("stored pack not served while slots are busy: %q, %v", got, err)
	}
}

// Another replica may store the same render while this one waits for a slot;
// the waiter then uses it instead of rendering again.
func TestServerPackWaiterUsesWhatAnotherReplicaStored(t *testing.T) {
	h, st, prov, pack, build := serverPackFixture(t)
	release := holdAllServerPackSlots(t)
	defer release()

	done := make(chan error, 1)
	go func() {
		_, err := h.shareServerPackKey(context.Background(), prov, pack, build, st.content)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond) // the request is now waiting for a slot

	fp, _ := serverPackFingerprint(sortServerPackContent(st.content))
	key := strings.TrimSuffix(h.mrpackStorageKey(pack, build), "pack.mrpack") + "server-" + fp + ".zip"
	if err := prov.Put(context.Background(), key, []byte("stored-by-another-replica")); err != nil {
		t.Fatal(err)
	}
	release()

	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := prov.puts.Load(); n != 0 {
		t.Errorf("rendered again although the object was already stored (puts=%d)", n)
	}
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

// A pack that fails to render is not rendered again on the next hit; a broken
// entry at the end of gigabytes of downloads would otherwise keep both slots busy.
func TestServerPackFailureIsNotRetriedAtOnce(t *testing.T) {
	h, st, prov, pack, build := serverPackFixture(t)
	st.content = append(st.content, uploadEntry("")) // missing storage key: fails
	if _, err := h.shareServerPackKey(context.Background(), prov, pack, build, st.content); err == nil {
		t.Fatal("a broken pack rendered")
	}
	defer holdAllServerPackSlots(t)()
	old := serverPackSlotWait
	serverPackSlotWait = 50 * time.Millisecond
	defer func() { serverPackSlotWait = old }()

	_, err := h.shareServerPackKey(context.Background(), prov, pack, build, st.content)
	if err == nil || errors.Is(err, errServerPackBusy) {
		t.Fatalf("err = %v, want the held failure without asking for a slot", err)
	}
}

// The first request for a large pack answers within the request wait instead of
// holding the connection for the whole render, and the render finishes anyway.
func TestServerPackRequestDoesNotWaitForTheWholeRender(t *testing.T) {
	h, st, prov, pack, build := serverPackFixture(t)
	release := holdAllServerPackSlots(t)
	defer release()
	old := serverPackRequestWait
	serverPackRequestWait = 50 * time.Millisecond
	defer func() { serverPackRequestWait = old }()

	if _, err := h.shareServerPackKey(context.Background(), prov, pack, build, st.content); !errors.Is(err, errServerPackBusy) {
		t.Fatalf("err = %v, want errServerPackBusy", err)
	}
	release()
	fp, _ := serverPackFingerprint(sortServerPackContent(st.content))
	key := strings.TrimSuffix(h.mrpackStorageKey(pack, build), "pack.mrpack") + "server-" + fp + ".zip"
	for i := 0; i < 100; i++ {
		if _, exists, _ := prov.Stat(context.Background(), key); exists {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the render did not carry on after the request gave up")
}

// The store orders build content by mod slug only, so rows can come back in a
// different order between requests; that must not be a new render.
func TestServerPackKeyIgnoresTheRowOrder(t *testing.T) {
	h, st, prov, pack, build := serverPackFixture(t)
	second := uploadEntry("uploads/test.zip")
	second.TargetPath = "zz/other"
	ab := []models.BuildContentEntry{st.content[0], second}
	ba := []models.BuildContentEntry{second, st.content[0]}
	k1, err1 := h.shareServerPackKey(context.Background(), prov, pack, build, ab)
	k2, err2 := h.shareServerPackKey(context.Background(), prov, pack, build, ba)
	if err1 != nil || err2 != nil || k1 != k2 || prov.puts.Load() != 1 {
		t.Fatalf("row order changed the render: %q %q puts=%d (%v %v)", k1, k2, prov.puts.Load(), err1, err2)
	}
}
