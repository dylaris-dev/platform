package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dylaris-core/models"
	"dylaris-core/storage/modpack"

	"github.com/gorilla/mux"
	"golang.org/x/sync/singleflight"
)

type createShareLinkRequest struct {
	Kind          string `json:"kind"`
	ExpiresInDays int    `json:"expiresInDays,omitempty"`
}

// CreateShareLink POST /api/packs/{id}/builds/{buildId}/share-link - mints a
// share token for one build, so it can be downloaded without a session.
func (h *PacksHandler) CreateShareLink(w http.ResponseWriter, r *http.Request) {
	b, ok := h.loadOwnedBuild(r)
	if !ok {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	userID, _ := r.Context().Value("userID").(string)
	var req createShareLinkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Kind != models.ShareLinkClientMrpack && req.Kind != models.ShareLinkServerPack {
		sendJSONError(w, "kind must be client-mrpack or server-pack", http.StatusBadRequest)
		return
	}
	token, err := generatePlaintextKey()
	if err != nil {
		sendJSONError(w, "Failed to mint token", http.StatusInternalServerError)
		return
	}
	link := &models.ShareLink{
		BuildID:   b.ID,
		Kind:      req.Kind,
		Token:     token,
		CreatedBy: userID,
	}
	if req.ExpiresInDays > 0 {
		exp := time.Now().Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour)
		link.ExpiresAt = &exp
	}
	id, err := h.state.Store.CreateShareLink(link)
	if err != nil {
		sendJSONError(w, "Failed to create share link", http.StatusInternalServerError)
		return
	}
	link.ID = id
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "link": link})
}

// ListShareLinks GET /api/packs/{id}/builds/{buildId}/share-links - the share
// links issued for one build.
func (h *PacksHandler) ListShareLinks(w http.ResponseWriter, r *http.Request) {
	b, ok := h.loadOwnedBuild(r)
	if !ok {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	links, err := h.state.Store.ListShareLinksByBuild(b.ID)
	if err != nil {
		sendJSONError(w, "Failed to list share links", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "links": links})
}

// RevokeShareLink DELETE /api/packs/{id}/builds/{buildId}/share-links/{linkId}
// - revokes one share link. Owning the pack and build is proven first, and the
// revoke itself is additionally scoped by creator, so one owner cannot revoke
// another's link.
func (h *PacksHandler) RevokeShareLink(w http.ResponseWriter, r *http.Request) {
	// loadOwnedBuild proves the caller owns the pack+build in the path; the store
	// revoke is additionally created_by-scoped, so cross-user revoke is blocked.
	if _, ok := h.loadOwnedBuild(r); !ok {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	userID, _ := r.Context().Value("userID").(string)
	linkID, _ := strconv.Atoi(mux.Vars(r)["linkId"])
	if err := h.state.Store.RevokeShareLink(linkID, userID); err != nil {
		sendJSONError(w, "Share link not found", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// ServeShare GET /api/share/{token} is PUBLIC, unauthenticated. Registered on the
// root router as a sibling of /mirror so it bypasses setup-lock, maintenance, and
// auth (the token is the credential). The modpacks feature is gated in-handler.
func (h *PacksHandler) ServeShare(w http.ResponseWriter, r *http.Request) {
	// Uniform 404 for every negative case (feature off, unknown/revoked/expired
	// token, missing build/pack) so the endpoint is not a validity oracle.
	notFound := func() { sendJSONError(w, "Not found", http.StatusNotFound) }
	if !h.state.FeatureFlags.IsModpacksEnabled(r.Context()) {
		notFound()
		return
	}
	token := mux.Vars(r)["token"]
	link, err := h.state.Store.GetShareLinkByToken(token)
	if err != nil || link == nil || link.Revoked {
		notFound()
		return
	}
	if link.ExpiresAt != nil && time.Now().After(*link.ExpiresAt) {
		notFound()
		return
	}
	build, err := h.state.Store.GetPackBuild(link.BuildID)
	if err != nil || build == nil {
		notFound()
		return
	}
	pack, err := h.state.Store.GetPack(build.PackID)
	if err != nil || pack == nil {
		notFound()
		return
	}

	switch link.Kind {
	case models.ShareLinkClientMrpack:
		key, err := h.shareMrpackKey(r.Context(), pack, build)
		if err != nil {
			sendJSONError(w, "Failed to render pack", http.StatusInternalServerError)
			return
		}
		prov, err := h.state.buildModpackStorageProvider()
		if err != nil || prov == nil {
			sendJSONError(w, "Storage unavailable", http.StatusInternalServerError)
			return
		}
		// deliverRedirect is safe here where the pack mirror streams instead:
		// this link is opened by a browser, and browsers follow a 302, while a
		// node downloads from the mirror against a host allowlist that a
		// storage-bucket redirect would step outside of. On an
		// S3-backed modpack storage the pack then never enters this process
		// at all.
		filename := pack.InternalSlug + "-" + build.VersionString + ".mrpack"
		if err := serveModpackObject(w, r, prov, key, deliverRedirect, "application/x-modrinth-modpack+zip", filename); err != nil {
			// A missing object is a 404 like every other absent resource on
			// this route, not a 500. The route's documented shape is a uniform
			// 404 so a token cannot be probed by status code, and answering 500
			// for a deleted pack broke both that and the helper's own contract.
			if errors.Is(err, modpack.ErrNotFound) {
				notFound()
				return
			}
			sendJSONError(w, "Failed to read pack", http.StatusInternalServerError)
			return
		}

	case models.ShareLinkServerPack:
		content, err := h.state.Store.ListBuildContent(build.ID)
		if err != nil {
			sendJSONError(w, "Failed to load content", http.StatusInternalServerError)
			return
		}
		prov, err := h.state.buildModpackStorageProvider()
		if err != nil || prov == nil {
			sendJSONError(w, "Storage unavailable", http.StatusInternalServerError)
			return
		}
		key, err := h.shareServerPackKey(r.Context(), prov, pack, build, content)
		if errors.Is(err, errServerPackBusy) {
			w.Header().Set("Retry-After", "60")
			sendJSONError(w, "Server pack is being built, try again shortly", http.StatusServiceUnavailable)
			return
		}
		if err != nil {
			if r.Context().Err() == nil {
				log.Printf("share: server-pack render failed for %s: %v", pack.InternalSlug, err)
			}
			sendJSONError(w, "Failed to render server pack", http.StatusInternalServerError)
			return
		}
		// Streamed, not redirected: a presigned URL would show the storage key,
		// and the pack's directory is reachable over the anonymous /mirror/ by
		// path - it would outlive a revoked link and open the client .mrpack
		// this link never shared. Streaming also keeps the filename and a
		// plain `curl -o` working as before. No render slot is held here.
		filename := pack.InternalSlug + "-" + build.VersionString + "-server.zip"
		if err := serveModpackObject(w, r, prov, key, deliverStream, "application/zip", filename); err != nil {
			if errors.Is(err, modpack.ErrNotFound) {
				notFound()
				return
			}
			sendJSONError(w, "Failed to read pack", http.StatusInternalServerError)
			return
		}

	default:
		notFound()
	}
}

// draftShareReuse is how long a share link reuses a draft's stored mrpack
// instead of rendering it again, while the draft is unchanged.
const draftShareReuse = time.Minute

// draftShareRenders maps a draft's mrpack storage key to the inputs and time of
// the last render a share link triggered in this process.
//
// A draft has no persisted mrpack, so every hit on its public share link
// rendered it: gigabytes of memory under the size caps, on the same two slots
// every tenant's installs and exports wait for. An anonymous holder of one link
// could keep both busy. Now a link renders at most once a minute per draft and
// replica, and at once when the draft changed.
//
// ponytail: the object can be overwritten by another replica or an install, so
// reuse is bounded in time rather than trusted forever. A draft edited and then
// reverted within the minute across replicas can serve the edited pack until
// the minute is up. Content-address the key if that ever matters.
var draftShareRenders sync.Map

type draftShareRender struct {
	inputs string
	at     time.Time
}

// shareMrpackKey is ensureInstallMrpack for the anonymous share route.
func (h *PacksHandler) shareMrpackKey(ctx context.Context, pack *models.Pack, build *models.PackBuild) (string, error) {
	if build.MrpackStorageKey != "" {
		return build.MrpackStorageKey, nil
	}
	content, err := h.state.Store.ListBuildContent(build.ID)
	if err != nil {
		return "", err
	}
	inputs, err := mrpackInputsFingerprint(pack, build, content)
	if err != nil {
		return "", err
	}
	key := h.mrpackStorageKey(pack, build)
	if v, ok := draftShareRenders.Load(key); ok {
		last := v.(draftShareRender)
		if last.inputs == inputs && time.Since(last.at) < draftShareReuse {
			if prov, err := h.state.buildModpackStorageProvider(); err == nil && prov != nil {
				if _, exists, err := prov.Stat(ctx, key); err == nil && exists {
					return key, nil
				}
			}
		}
	}
	if key, err = h.storeDraftMrpack(ctx, pack, build, content); err != nil {
		return "", err
	}
	draftShareRenders.Store(key, draftShareRender{inputs: inputs, at: time.Now()})
	return key, nil
}

// serverPackRenders bounds the server-pack renders running in this Core, and
// serverPackFlight collapses concurrent requests for the same render into one.
//
// The share route used to build the server zip on every request, straight into
// the response: up to 2 GiB of downloads, temp disk and deflate per hit, held
// open for as long as the client cared to read, with nothing bounding how many
// ran. One anonymous holder of a link could fill Core's disk and CPU for every
// tenant. Now a render goes to a temp file and into storage once per distinct
// input, and the client is served from there. The slot is released before a
// single byte goes to a client, so a slow reader holds nothing shared.
var (
	serverPackRenders = make(chan struct{}, 2)
	serverPackFlight  singleflight.Group
	// serverPackFailures holds when a render last failed, per key. A pack
	// that cannot render (a broken entry at the end of 2 GiB of downloads)
	// would otherwise redo all of it on every hit and keep both slots busy.
	serverPackFailures sync.Map
)

// errServerPackBusy is the answer when the pack is not ready within
// serverPackRequestWait; the render goes on and a retry finds it stored.
var errServerPackBusy = errors.New("server pack is being built")

// Vars so a test can shorten them.
var (
	serverPackSlotWait    = time.Minute
	serverPackRequestWait = time.Minute
)

const (
	serverPackRenderBudget = 15 * time.Minute
	serverPackFailureHold  = 5 * time.Minute
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

// shareServerPackKey returns the storage key of the build's server pack,
// rendering and storing it first when these inputs have not been rendered yet.
//
// The key is content-addressed, so a stored object is never stale and replicas
// never overwrite each other's work. The previous render's key is kept in a
// small marker object and deleted when a new one is stored, so editing a shared
// draft does not leave one zip behind per edit.
//
// ponytail: two replicas storing two new renders at once can both delete the
// same previous key and orphan one of theirs; deleting a build does not remove
// its stored objects (the .mrpack neither). A sweep by prefix closes both.
func (h *PacksHandler) shareServerPackKey(ctx context.Context, prov modpack.ModpackStorageProvider, pack *models.Pack, build *models.PackBuild, content []models.BuildContentEntry) (string, error) {
	content = sortServerPackContent(content)
	fp, err := serverPackFingerprint(content)
	if err != nil {
		return "", err
	}
	dir := strings.TrimSuffix(h.mrpackStorageKey(pack, build), "pack.mrpack")
	key := dir + "server-" + fp + ".zip"
	if _, exists, err := prov.Stat(ctx, key); err == nil && exists {
		return key, nil
	}
	if v, ok := serverPackFailures.Load(key); ok {
		if time.Since(v.(time.Time)) < serverPackFailureHold {
			return "", errors.New("server pack failed to render recently")
		}
		serverPackFailures.Delete(key)
	}

	ch := serverPackFlight.DoChan(key, func() (any, error) {
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
		if _, exists, err := prov.Stat(rctx, key); err == nil && exists {
			return nil, nil // another replica stored it while we waited
		}
		if err := h.storeServerPack(rctx, prov, content, key); err != nil {
			serverPackFailures.Store(key, time.Now())
			return nil, err
		}
		serverPackFailures.Delete(key)
		marker := dir + "server.last"
		if rc, _, err := prov.Stream(rctx, marker); err == nil {
			prev, _ := io.ReadAll(io.LimitReader(rc, 512))
			rc.Close()
			if p := string(prev); p != key && strings.HasPrefix(p, dir+"server-") {
				_ = prov.Delete(rctx, p)
			}
		}
		_ = prov.Put(rctx, marker, []byte(key))
		return nil, nil
	})
	// Bounded so the first request for a large pack answers before a proxy in
	// front of Core gives up on it; the render carries on and a retry is served.
	select {
	case res := <-ch:
		return key, res.Err
	case <-time.After(serverPackRequestWait):
		return "", errServerPackBusy
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// storeServerPack renders the server pack into a temp file and stores it.
func (h *PacksHandler) storeServerPack(ctx context.Context, prov modpack.ModpackStorageProvider, content []models.BuildContentEntry, key string) error {
	tmp, err := os.CreateTemp("", "serverpack-*.zip")
	if err != nil {
		return err
	}
	defer cleanupTemp(tmp)
	if err := h.renderServerPack(ctx, content, tmp); err != nil {
		return err
	}
	size, err := tmp.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return prov.PutStream(ctx, key, tmp, size)
}
