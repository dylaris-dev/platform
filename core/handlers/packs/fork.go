package packs

import (
	"context"
	"crypto/md5"
	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/storage/modpack"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const maxForkObjectBytes int64 = 1 << 30
const maxForkUploadBytes int64 = 2 << 30

var forkSlots = make(chan struct{}, 2)

// ForkBuild copies an authorized build into a new or empty writable pack.
// No source publication binding, access grant or bearer share link is inherited.
func (h *PacksHandler) ForkBuild(w http.ResponseWriter, r *http.Request) {
	source, _, ok := h.loadBuild(r, packRead)
	if !ok {
		handlers.SendJSONError(w, "Forbidden", 403)
		return
	}
	user, _ := r.Context().Value("userID").(string)
	var req struct {
		TargetPackID  int    `json:"targetPackId"`
		Name          string `json:"name"`
		Slug          string `json:"slug"`
		VersionString string `json:"versionString"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil || req.TargetPackID < 0 {
		handlers.SendJSONError(w, "Invalid fork request", 400)
		return
	}
	original, ok := h.packAccess(r, source.PackID, packRead)
	if !ok {
		handlers.SendJSONError(w, "Forbidden", 403)
		return
	}
	target := &models.Pack{OwnerID: user, InternalName: strings.TrimSpace(req.Name), Summary: original.Summary, ModrinthVisibility: "unlisted"}
	if req.TargetPackID > 0 {
		target, ok = h.packAccess(r, req.TargetPackID, packEdit)
		if !ok {
			handlers.SendJSONError(w, "Target edit access is required", 403)
			return
		}
		builds, err := h.state.Store.ListPackBuilds(target.ID)
		if err != nil {
			handlers.SendJSONError(w, "Could not inspect the target", 500)
			return
		}
		if len(builds) > 0 {
			handlers.SendJSONError(w, "The target must be empty: it may not contain any builds", 409)
			return
		}
	} else {
		if target.InternalName == "" || len([]rune(target.InternalName)) > 128 {
			handlers.SendJSONError(w, "A pack name of 1-128 characters is required", 400)
			return
		}
		target.InternalSlug = strings.TrimSpace(req.Slug)
		if target.InternalSlug == "" {
			target.InternalSlug = handlers.Slugify(target.InternalName)
			if len(target.InternalSlug) > 64 {
				target.InternalSlug = strings.TrimRight(target.InternalSlug[:64], "-_")
			}
		}
		if !packSlugRe.MatchString(target.InternalSlug) {
			handlers.SendJSONError(w, "Invalid target slug", 400)
			return
		}
		own, err := h.state.Store.ListPacksByOwner(user)
		if err != nil {
			handlers.SendJSONError(w, "Could not inspect existing packs", 500)
			return
		}
		for _, p := range own {
			if p.InternalSlug == target.InternalSlug {
				handlers.SendJSONError(w, "A pack with this slug already exists", 409)
				return
			}
		}
	}
	version := strings.TrimSpace(req.VersionString)
	if version == "" {
		version = source.VersionString
	}
	if len([]rune(version)) > 64 || !safeKeyComponent(version) {
		handlers.SendJSONError(w, "Invalid target build version", 400)
		return
	}
	select {
	case forkSlots <- struct{}{}:
		defer func() { <-forkSlots }()
	default:
		handlers.SendJSONError(w, "Other pack copies are in progress. Try again shortly.", 503)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	rows, err := h.state.Store.ListBuildContent(source.ID)
	if err != nil {
		handlers.SendJSONError(w, "Could not read source content", 500)
		return
	}
	if len(rows) > 2000 {
		handlers.SendJSONError(w, "A fork supports up to 2000 content entries", 422)
		return
	}
	folders, err := h.state.Store.ListPackFolders(ctx, source.ID)
	if err != nil {
		handlers.SendJSONError(w, "Could not read source folders", 500)
		return
	}
	forkID := strings.ReplaceAll(uuid.NewString(), "-", "")
	var provider modpack.ModpackStorageProvider
	staged := []string{}
	committed := false
	defer func() {
		if committed || provider == nil {
			return
		}
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		unused, err := h.state.Store.UnreferencedPackForkUploads(cleanup, forkID, staged)
		if err != nil {
			log.Print("pack fork: retained staged uploads because the database outcome could not be verified")
			return
		}
		for _, key := range unused {
			if cleanup.Err() != nil {
				break
			}
			if err := provider.Delete(cleanup, key); err != nil {
				log.Print("pack fork: could not clean up a staged upload")
			}
		}
	}()
	copied := map[string]string{}
	var total int64
	for i := range rows {
		entry := &rows[i]
		if entry.StorageKey == "" {
			continue
		}
		if key, exists := copied[entry.StorageKey]; exists {
			entry.StorageKey = key
			continue
		}
		if entry.Filesize < 0 || entry.Filesize > maxForkObjectBytes || total+entry.Filesize > maxForkUploadBytes {
			handlers.SendJSONError(w, "Uploaded fork content exceeds the 1 GB/file or 2 GB total limit", 422)
			return
		}
		if provider == nil {
			provider, err = h.state.BuildModpackStorageProvider()
			if err != nil || provider == nil {
				handlers.SendJSONError(w, "Modpack storage is unavailable", 503)
				return
			}
		}
		key := "packs/" + target.OwnerID + "/forks/" + forkID + "/" + strconv.Itoa(entry.ID) + ".zip"
		// Track before writing: even a partially failed provider write is ours to remove.
		staged = append(staged, key)
		if err = copyForkArtifact(ctx, provider, entry.StorageKey, key, *entry); err != nil {
			handlers.SendJSONError(w, "Could not copy and verify an uploaded file; the source was kept unchanged", 502)
			return
		}
		total += entry.Filesize
		copied[entry.StorageKey] = key
		entry.StorageKey = key
	}
	// Recheck grants after storage work; a revoked invitation cannot finish a fork.
	if _, ok = h.packAccess(r, source.PackID, packRead); !ok {
		handlers.SendJSONError(w, "Source access was revoked", 403)
		return
	}
	if target.ID > 0 {
		if _, ok = h.packAccess(r, target.ID, packEdit); !ok {
			handlers.SendJSONError(w, "Target access was revoked", 403)
			return
		}
	}
	b := &models.PackBuild{VersionString: version, Minecraft: source.Minecraft, Loader: source.Loader, LoaderVersion: source.LoaderVersion, MinJava: source.MinJava, MinMemory: source.MinMemory, Changelog: source.Changelog, Channel: models.ChannelDraft}
	f := &models.PackFork{SourcePackID: source.PackID, SourceBuildID: source.ID, Target: target, Build: b, CatalogSuffix: forkID, Content: rows, Folders: folders}
	packID, _, err := h.state.Store.ForkPackBuild(ctx, f)
	if err != nil {
		handlers.SendJSONError(w, "Could not finish the fork: target must still be empty and its slug unique", 409)
		return
	}
	committed = true
	h.audit(r, target.OwnerID, packID, "build.forked", map[string]any{"sourcePackId": source.PackID, "sourceBuildId": source.ID, "buildId": b.ID})
	publishPackEvent(r, h.state, "pack_builds.changed", packID, map[string]interface{}{"packId": packID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "pack": target, "build": b})
}

func copyForkArtifact(ctx context.Context, p modpack.ModpackStorageProvider, source, target string, e models.BuildContentEntry) error {
	rc, size, err := p.Stream(ctx, source)
	if err != nil {
		return err
	}
	defer rc.Close()
	if size >= 0 && size != e.Filesize {
		return fmt.Errorf("source artifact size changed")
	}
	tmp, err := os.CreateTemp("", "dylaris-fork-*")
	if err != nil {
		return err
	}
	defer cleanupTemp(tmp)
	digest := md5.New()
	n, err := io.Copy(io.MultiWriter(tmp, digest), io.LimitReader(forkReader{ctx: ctx, r: rc}, maxForkObjectBytes+1))
	if err != nil {
		return err
	}
	if n != e.Filesize || n > maxForkObjectBytes {
		return fmt.Errorf("source artifact size mismatch")
	}
	if e.MD5 != "" && !strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), e.MD5) {
		return fmt.Errorf("source artifact checksum mismatch")
	}
	if err = validateStoredModZip(tmp, n); err != nil {
		return err
	}
	if _, err = tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return p.PutStream(ctx, target, tmp, n)
}

type forkReader struct {
	ctx context.Context
	r   io.Reader
}

func (r forkReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
