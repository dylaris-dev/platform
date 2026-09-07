package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dylaris-core/models"
	"dylaris-core/pkg/crypto"
	"dylaris-core/storage/modpack"

	"github.com/gorilla/mux"
)

type mrpackIndexOut struct {
	FormatVersion int               `json:"formatVersion"`
	Game          string            `json:"game"`
	VersionID     string            `json:"versionId"`
	Name          string            `json:"name"`
	Summary       string            `json:"summary,omitempty"`
	Files         []mrpackFileOut   `json:"files"`
	Dependencies  map[string]string `json:"dependencies"`
}

type mrpackFileOut struct {
	Path      string            `json:"path"`
	Hashes    map[string]string `json:"hashes"`
	Env       map[string]string `json:"env,omitempty"`
	Downloads []string          `json:"downloads"`
	FileSize  int64             `json:"fileSize"`
}

// envForSide maps a build-content side to the mrpack per-file env block.
func envForSide(side string) map[string]string {
	switch side {
	case models.SideClient:
		return map[string]string{"client": "required", "server": "unsupported"}
	case models.SideServer:
		return map[string]string{"client": "unsupported", "server": "required"}
	default:
		return map[string]string{"client": "required", "server": "required"}
	}
}

// isMrpackFilesEntry reports whether an entry can be a clean files[] reference:
// Modrinth-linked, carrying both hashes, with a Modrinth CDN download URL.
// Everything else is embedded under overrides/ instead.
//
// One predicate rather than the same four-term condition written out in both
// buildMrpackIndex and writeMrpackZip: the two decide the SAME question about
// the same entry, and an entry that both skip is an entry that silently leaves
// the pack.
func isMrpackFilesEntry(e models.BuildContentEntry) bool {
	return e.Linked && e.SHA1 != "" && e.SHA512 != "" && modrinthCDNURL(e) != ""
}

// buildMrpackIndex builds modrinth.index.json. Modrinth-linked content goes in
// files[]; everything else is embedded under overrides/ (written separately by
// writeMrpackZip).
//
// files[].path is a path the LAUNCHER writes to, so a traversal-bearing
// TargetPath here is the manifest's version of a zip slip - the same defect
// renderServerPack's streamModrinthContent and the Solder render both already
// refuse on this exact field. This was the third reader of it and the one
// without the check. It reaches the DB unsanitized from exactly one place:
// addModrinthVersion builds it from the filename the MODRINTH API reports,
// which is third-party text. Our own node resolves every mrpack entry through
// resolveExtractPath and would refuse it; a third-party launcher is not ours to
// assume anything about, and an exported .mrpack is a file that leaves here.
func buildMrpackIndex(pack *models.Pack, build *models.PackBuild, content []models.BuildContentEntry) (mrpackIndexOut, error) {
	files := make([]mrpackFileOut, 0, len(content))
	for _, e := range content {
		if !isMrpackFilesEntry(e) {
			// Non-linked content is embedded via overrides/ in writeMrpackZip.
			continue
		}
		if e.TargetPath == "" || modpack.IsUnsafeEntryPath(e.TargetPath) {
			return mrpackIndexOut{}, fmt.Errorf("modrinth content %q has an invalid target path", e.ModSlug)
		}
		files = append(files, mrpackFileOut{
			Path:      e.TargetPath,
			Hashes:    map[string]string{"sha1": e.SHA1, "sha512": e.SHA512},
			Env:       envForSide(e.Side),
			Downloads: []string{modrinthCDNURL(e)},
			FileSize:  e.Filesize,
		})
	}

	deps := map[string]string{"minecraft": build.Minecraft}
	switch strings.ToLower(build.Loader) {
	case "fabric":
		deps["fabric-loader"] = build.LoaderVersion
	case "quilt":
		deps["quilt-loader"] = build.LoaderVersion
	case "forge":
		deps["forge"] = build.LoaderVersion
	case "neoforge":
		deps["neoforge"] = build.LoaderVersion
	}

	summary := pack.Summary
	if build.Changelog != "" {
		summary = strings.TrimSpace(pack.Summary + "\n\n" + build.Changelog)
	}
	name := pack.SolderDisplayName
	if name == "" {
		name = pack.InternalName
	}
	return mrpackIndexOut{
		FormatVersion: 1,
		Game:          "minecraft",
		VersionID:     build.VersionString,
		Name:          name,
		Summary:       summary,
		Files:         files,
		Dependencies:  deps,
	}, nil
}

// modrinthCDNURL returns the cdn.modrinth.com download URL for a linked entry,
// or "". It reads modrinth_download_url (set at add/replace time); url_override
// is reserved for the Solder mirror URL and is NOT used here. An entry with no
// cdn URL falls to overrides/ (still installs, just embedded not referenced).
func modrinthCDNURL(e models.BuildContentEntry) string {
	if strings.HasPrefix(e.ModrinthDownloadURL, "https://cdn.modrinth.com/") {
		return e.ModrinthDownloadURL
	}
	return ""
}

// overrideEntriesFromStoredZip reads a content entry's stored Solder zip and
// writes its inner files directly into zw under overrides/. A Solder zip
// already holds the file at its .minecraft-relative path (e.g. mods/x.jar),
// so we prefix "overrides/". Skips content that has no storage_key (pure
// Modrinth reference).
//
// Reachable from the PUBLIC UNAUTHENTICATED ServeShare mrpack endpoint (BC2).
// total is a running total shared across every stored zip of the render;
// it is incremented and checked INSIDE this per-file loop, immediately after
// each file's io.LimitReader read, mirroring renderServerPack's accumulator
// placement. Writing straight into zw instead of returning a
// map[string][]byte means a bomb split across many small files within one
// stored zip fails the moment the aggregate crosses the cap, instead of
// only being caught after the whole entry was already buffered in RAM.
func (h *PacksHandler) overrideEntriesFromStoredZip(ctx context.Context, zw *zip.Writer, prov modpack.ModpackStorageProvider, e models.BuildContentEntry, total *int64) error {
	if e.StorageKey == "" {
		return nil
	}
	raw, err := prov.Get(ctx, e.StorageKey)
	if err != nil {
		return fmt.Errorf("override read %s: %w", e.StorageKey, err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return fmt.Errorf("override unzip %s: %w", e.StorageKey, err)
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if f.UncompressedSize64 > maxServerPackEntryBytes {
			return fmt.Errorf("override %q entry %q exceeds the size cap", e.StorageKey, f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		b, err := io.ReadAll(io.LimitReader(rc, maxServerPackEntryBytes+1))
		rc.Close()
		if err != nil {
			return err
		}
		if int64(len(b)) > maxServerPackEntryBytes {
			return fmt.Errorf("override %q entry %q exceeds the size cap", e.StorageKey, f.Name)
		}
		*total += int64(len(b))
		if *total > maxServerPackTotalBytes {
			return fmt.Errorf("mrpack overrides exceed the total size cap")
		}
		ew, err := zw.CreateHeader(&zip.FileHeader{Name: "overrides/" + f.Name, Method: zip.Deflate, Modified: time.Now()})
		if err != nil {
			return err
		}
		if _, err := ew.Write(b); err != nil {
			return err
		}
	}
	return nil
}

// writeMrpackZip writes modrinth.index.json + all overrides into zw.
func (h *PacksHandler) writeMrpackZip(ctx context.Context, zw *zip.Writer, pack *models.Pack, build *models.PackBuild, content []models.BuildContentEntry, prov modpack.ModpackStorageProvider) error {
	idx, err := buildMrpackIndex(pack, build, content)
	if err != nil {
		return err
	}
	indexBytes, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	iw, err := zw.CreateHeader(&zip.FileHeader{Name: "modrinth.index.json", Method: zip.Deflate, Modified: time.Now()})
	if err != nil {
		return err
	}
	if _, err := iw.Write(indexBytes); err != nil {
		return err
	}

	// Embed non-linked content under overrides/. total bounds the assembled
	// pack across all entries and all stored zips (BC2), mirroring
	// renderServerPack's running cap; overrideEntriesFromStoredZip checks it
	// incrementally per file instead of after fully materializing an entry.
	var total int64
	for _, e := range content {
		if isMrpackFilesEntry(e) {
			continue // already a files[] reference
		}
		if prov == nil {
			continue // no storage configured; skip embedding
		}
		if err := h.overrideEntriesFromStoredZip(ctx, zw, prov, e, &total); err != nil {
			return err
		}
	}
	return nil
}

// renderMrpack returns the full .mrpack bytes for a build.
func (h *PacksHandler) renderMrpack(ctx context.Context, pack *models.Pack, build *models.PackBuild, content []models.BuildContentEntry) ([]byte, error) {
	prov, _ := h.state.buildModpackStorageProvider()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if err := h.writeMrpackZip(ctx, zw, pack, build, content, prov); err != nil {
		zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// mrpackStorageKey is the storage key for a build's rendered .mrpack.
//
// The directory segment is opaque on purpose: the Node fetches this object over
// /solder/mirror/, which is anonymous by necessity (see SolderMirror), so the
// PATH is the credential here, the same model as /api/share/{token}. That only
// holds while the path cannot be derived from something the platform publishes.
//
// It used to be modpacks/<ownerID>/<internalSlug>/<version>/, and the owner id
// is printed in plain sight in every mods[].url the public Solder API serves -
// in all three delivery modes, since the storage key is what a launcher
// downloads by. One public pack therefore handed out a segment the layout
// treated as secret, leaving only the internal slug between an anonymous caller
// and every OTHER pack of that account. services.SystemEvents already refuses to
// broadcast a pack owner id for exactly this reason; the Solder API published it
// anyway.
//
// Derived rather than random so the key stays stable per (owner, pack, build):
// re-rendering a draft install overwrites its object instead of leaving a new
// one behind on every install. CLUSTER_SECRET cannot be empty (LoadConfig
// refuses to boot on the default or an unset value), so the segment always has a
// real key behind it. Rotating that secret moves the derived path: a PUBLISHED
// build is unaffected because its key is stored on the build row, and an
// unpublished one is re-rendered under the new path, orphaning the old object.
func (h *PacksHandler) mrpackStorageKey(pack *models.Pack, build *models.PackBuild) string {
	mac := hmac.New(sha256.New, crypto.DeriveKey(h.state.ClusterSecret, "mrpack-path"))
	fmt.Fprintf(mac, "%s\x1f%s\x1f%s", pack.OwnerID, pack.InternalSlug, build.VersionString)
	return "modpacks/" + hex.EncodeToString(mac.Sum(nil)) + "/pack.mrpack"
}

// persistMrpackForBuild renders + (for beta/release) persists the mrpack to
// storage and freezes the build. Drafts are rendered fresh, never persisted.
func (h *PacksHandler) persistMrpackForBuild(ctx context.Context, pack *models.Pack, build *models.PackBuild, content []models.BuildContentEntry) ([]byte, error) {
	data, err := h.renderMrpack(ctx, pack, build, content)
	if err != nil {
		return nil, err
	}
	if build.Channel == models.ChannelDraft {
		return data, nil
	}
	prov, err := h.state.buildModpackStorageProvider()
	if err != nil {
		return nil, fmt.Errorf("modpack storage misconfigured: %w", err)
	}
	if prov == nil {
		return nil, fmt.Errorf("no modpack storage configured (Settings -> Modpacks)")
	}
	key := h.mrpackStorageKey(pack, build)
	if err := prov.Put(ctx, key, data); err != nil {
		return nil, fmt.Errorf("mrpack storage put: %w", err)
	}
	sum := sha256.Sum256(data)
	build.MrpackStorageKey = key
	build.MrpackSHA256 = hex.EncodeToString(sum[:])
	build.Frozen = true
	if err := h.state.Store.UpdatePackBuild(build); err != nil {
		return nil, fmt.Errorf("mrpack persisted but stamp failed: %w", err)
	}
	return data, nil
}

// ensureInstallMrpack returns the storage key of a mrpack the Node can download
// for a server install. If the build is already published (MrpackStorageKey set)
// it reuses that. Otherwise it renders the build's mrpack on the fly and stores it
// under the deterministic key WITHOUT mutating the build record or freezing it —
// so draft builds stay installable and editable (owner decision, Phase 5).
func (h *PacksHandler) ensureInstallMrpack(ctx context.Context, pack *models.Pack, build *models.PackBuild) (string, error) {
	if build.MrpackStorageKey != "" {
		return build.MrpackStorageKey, nil
	}
	content, err := h.state.Store.ListBuildContent(build.ID)
	if err != nil {
		return "", err
	}
	data, err := h.renderMrpack(ctx, pack, build, content)
	if err != nil {
		return "", err
	}
	prov, err := h.state.buildModpackStorageProvider()
	if err != nil {
		return "", err
	}
	if prov == nil {
		return "", fmt.Errorf("modpack storage not configured")
	}
	key := h.mrpackStorageKey(pack, build)
	if err := prov.Put(ctx, key, data); err != nil {
		return "", err
	}
	return key, nil
}

// ExportMrpack streams the .mrpack for a build (self-distributed download).
func (h *PacksHandler) ExportMrpack(w http.ResponseWriter, r *http.Request) {
	packID, _ := strconv.Atoi(mux.Vars(r)["id"])
	p, ok := h.ownsPack(r, packID)
	if !ok {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	buildID, _ := strconv.Atoi(mux.Vars(r)["buildId"])
	build, err := h.state.Store.GetPackBuild(buildID)
	if err != nil || build == nil || build.PackID != packID {
		sendJSONError(w, "Build not found", http.StatusNotFound)
		return
	}
	content, err := h.state.Store.ListBuildContent(buildID)
	if err != nil {
		sendJSONError(w, "Failed to load content", http.StatusInternalServerError)
		return
	}
	data, err := h.renderMrpack(r.Context(), p, build, content)
	if err != nil {
		sendJSONError(w, "Failed to build .mrpack: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-modrinth-modpack+zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", p.InternalSlug+"-"+build.VersionString+".mrpack"))
	_, _ = w.Write(data)
}
