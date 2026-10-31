package packs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"dylaris-core/handlers"
	"dylaris-core/models"

	"golang.org/x/sync/singleflight"
)

// A server pack embeds every server-side Modrinth jar, so it is a copy of
// third-party files. It is rendered on request into a local temp cache, served
// from there to the one caller allowed to have it, and swept after an hour. It
// is never written to modpack storage: the stored, shareable copy was what the
// removed share links handed out.

// serverPackRenders bounds the server-pack renders running in this Core, and
// serverPackFlight collapses concurrent requests for the same render into one.
// A render goes to a temp file once per distinct input and the client is served
// from there; the slot is released before a single byte goes to a client, so a
// slow reader holds nothing shared.
var (
	serverPackRenders = make(chan struct{}, 2)
	serverPackFlight  singleflight.Group
	// serverPackFailures holds when a render last failed, per fingerprint. A
	// pack that cannot render (a broken entry at the end of 2 GiB of downloads)
	// would otherwise redo all of it on every hit and keep both slots busy.
	serverPackFailures sync.Map
)

// errServerPackBusy is the answer when the pack is not ready within
// serverPackRequestWait; the render goes on and a retry finds it cached.
var errServerPackBusy = errors.New("server pack is being built")

// Vars so a test can shorten them or point them elsewhere.
var (
	serverPackSlotWait    = time.Minute
	serverPackRequestWait = time.Minute
	serverPackCacheDir    = filepath.Join(os.TempDir(), "dylaris-serverpacks")
)

const (
	serverPackRenderBudget = 15 * time.Minute
	serverPackFailureHold  = 5 * time.Minute
	// serverPackCacheTTL is how long a rendered server pack stays on disk. It
	// is longer than the render budget, so the sweep never removes a render
	// that is still being written.
	serverPackCacheTTL = time.Hour
)

// sortServerPackContent puts the content in the one order both the fingerprint
// and the render use. The store orders by mod slug only, so two rows of one mod
// could swap between requests; and when two entries write the same path, the
// order decides which one wins, so it has to be part of what the key means.
func sortServerPackContent(content []models.BuildContentEntry) []models.BuildContentEntry {
	out := append([]models.BuildContentEntry(nil), content...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TargetPath != out[j].TargetPath {
			return out[i].TargetPath < out[j].TargetPath
		}
		return out[i].StorageKey+out[i].ModrinthDownloadURL < out[j].StorageKey+out[j].ModrinthDownloadURL
	})
	return out
}

// serverPackFingerprint hashes exactly what renderServerPack reads, in the
// order given (sortServerPackContent). Hashing the whole build content would
// change the key whenever the hourly update check stamps a mod row, and
// re-render gigabytes for nothing.
func serverPackFingerprint(content []models.BuildContentEntry) (string, error) {
	type in struct {
		Side, Source, StorageKey, URL, SHA1, SHA512, TargetPath string
	}
	ins := make([]in, 0, len(content))
	for _, e := range content {
		if e.Side == models.SideClient {
			continue
		}
		ins = append(ins, in{string(e.Side), string(e.Source), e.StorageKey, e.ModrinthDownloadURL, e.SHA1, e.SHA512, e.TargetPath})
	}
	b, err := json.Marshal(ins)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// openServerPack returns the cached server pack for content, rendering it
// first when these inputs have no fresh file yet. The file name is the
// fingerprint, so a cached file is never stale and two builds with the same
// server content share one render. The caller closes the file.
func (h *PacksHandler) openServerPack(ctx context.Context, content []models.BuildContentEntry) (*os.File, error) {
	content = sortServerPackContent(content)
	fp, err := serverPackFingerprint(content)
	if err != nil {
		return nil, err
	}
	dir := serverPackCacheDir
	final := filepath.Join(dir, fp+".zip")
	if f, err := os.Open(final); err == nil {
		return f, nil
	}
	if v, ok := serverPackFailures.Load(fp); ok {
		if time.Since(v.(time.Time)) < serverPackFailureHold {
			return nil, errors.New("server pack failed to render recently")
		}
		serverPackFailures.Delete(fp)
	}

	ch := serverPackFlight.DoChan(fp, func() (any, error) {
		// Detached from the request: the render is shared by every waiter, so
		// one client leaving must not cancel it for the rest.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serverPackRenderBudget)
		defer cancel()
		select {
		case serverPackRenders <- struct{}{}:
			defer func() { <-serverPackRenders }()
		case <-time.After(serverPackSlotWait):
			return nil, errServerPackBusy
		}
		if _, err := os.Stat(final); err == nil {
			return nil, nil
		}
		if err := h.renderServerPackFile(rctx, dir, fp, content, final); err != nil {
			serverPackFailures.Store(fp, time.Now())
			return nil, err
		}
		serverPackFailures.Delete(fp)
		return nil, nil
	})
	// Bounded so the first request for a large pack answers before a proxy in
	// front of Core gives up on it; the render carries on and a retry is served.
	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		// The sweep can only take a file an hour old, so it is still here.
		return os.Open(final)
	case <-time.After(serverPackRequestWait):
		return nil, errServerPackBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// renderServerPackFile renders into a partial file next to final and renames
// it into place, so a reader never sees half a pack.
func (h *PacksHandler) renderServerPackFile(ctx context.Context, dir, fp string, content []models.BuildContentEntry, final string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, fp+"-*.part")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := h.renderServerPack(ctx, content, tmp); err != nil {
		cleanupTemp(tmp)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, final); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// SweepServerPackCache deletes cached server packs older than the TTL, and
// leftover partial renders with them.
func SweepServerPackCache(now time.Time) {
	entries, err := os.ReadDir(serverPackCacheDir)
	if err != nil {
		return // nothing cached yet
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || e.IsDir() || now.Sub(info.ModTime()) < serverPackCacheTTL {
			continue
		}
		if err := os.Remove(filepath.Join(serverPackCacheDir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("server pack cache: could not remove %s: %v", e.Name(), err)
		}
	}
}

// StartServerPackSweeper sweeps the cache at once (a restart leaves the
// previous run's files behind) and then every few minutes until ctx ends. Every
// replica sweeps its own disk, so it is not leader-gated.
func (h *PacksHandler) StartServerPackSweeper(ctx context.Context) {
	go func() {
		SweepServerPackCache(time.Now())
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				SweepServerPackCache(now)
			}
		}
	}()
}

// exportServerPack serves the server ZIP after ExportMrpack has checked the
// caller may edit the pack.
func (h *PacksHandler) exportServerPack(w http.ResponseWriter, r *http.Request, pack *models.Pack, build *models.PackBuild, content []models.BuildContentEntry) {
	f, err := h.openServerPack(r.Context(), content)
	if errors.Is(err, errServerPackBusy) {
		w.Header().Set("Retry-After", "60")
		handlers.SendJSONError(w, "The server archive is being prepared. Try the download again in a minute.", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		if r.Context().Err() == nil {
			log.Printf("server export: render failed for build %d: %v", build.ID, err)
		}
		handlers.SendJSONError(w, "Failed to prepare server archive", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		handlers.SendJSONError(w, "Failed to read server archive", http.StatusInternalServerError)
		return
	}
	filename := pack.InternalSlug + "-" + build.VersionString + "-server.zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, filename, info.ModTime(), f)
}

// ServeRemovedShare GET /api/share/{token} - public build share links were
// removed (owner decision 2026-10-31): they redistributed Modrinth files to
// anyone holding the URL. A link already out there gets a clear 410 rather
// than a 404 that reads like a typo.
func (h *PacksHandler) ServeRemovedShare(w http.ResponseWriter, r *http.Request) {
	handlers.SendJSONError(w, "Public pack share links have been removed. Ask the pack's owner for access.", http.StatusGone)
}
