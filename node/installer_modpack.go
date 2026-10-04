package main

// Install a Modrinth modpack (.mrpack format) as a fresh
// sub-server.
//
// .mrpack format reference (https://docs.modrinth.com/docs/modpacks/format):
//   - zip file
//   - modrinth.index.json at root: { formatVersion, game:"minecraft",
//       versionId, name, summary, files[], dependencies{minecraft, forge,
//       fabric-loader, quilt-loader, neoforge, ...} }
//   - overrides/ : tree of files copied verbatim into the server dir
//   - server-overrides/ : applied on top of overrides for server installs
//
// Each entry in files[] has { path, hashes:{sha1,sha512}, env:{client,server},
// downloads:[url, ...], fileSize }. We accept only downloads from a small
// allowlist (cdn.modrinth.com + the Modrinth-approved CDN hosts they
// publish) and skip files whose env.server == "unsupported".

import (
	"archive/zip"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

type mrpackIndex struct {
	FormatVersion int               `json:"formatVersion"`
	Game          string            `json:"game"`
	VersionID     string            `json:"versionId"`
	Name          string            `json:"name"`
	Summary       string            `json:"summary"`
	Files         []mrpackFile      `json:"files"`
	Dependencies  map[string]string `json:"dependencies"`
}

type mrpackFile struct {
	Path      string            `json:"path"`
	Hashes    map[string]string `json:"hashes"`
	Env       map[string]string `json:"env,omitempty"`
	Downloads []string          `json:"downloads"`
	FileSize  int64             `json:"fileSize"`
}

// modpackAllowedHosts mirrors Modrinth's published list of approved download
// hosts. Any URL outside this set is a sign of a tampered or self-hosted
// pack — V1 we just refuse those rather than try to whitelist user content.
//
// The two forgecdn hosts are gone: since 2026-07-16 every download from that
// CDN needs a CurseForge API key, so such a URL could only end in a 401. A
// refusal here names the host and is the clearer failure of the two.
var modpackAllowedHosts = map[string]bool{
	"cdn.modrinth.com":          true,
	"github.com":                true,
	"raw.githubusercontent.com": true,
	"gitlab.com":                true,
	"maven.fabricmc.net":        true,
	"maven.minecraftforge.net":  true,
}

const (
	maxMrpackSize       = 200 << 20 // 200 MB total mrpack archive
	maxModpackTotalSize = 4 << 30   // 4 GB cap on summed file sizes
	// maxModpackFile bounds ONE file from the manifest. The manifest's own
	// fileSize is a claim by whoever wrote the pack, not a fact, so it may only
	// tighten this ceiling - never raise it, and never remove it. See
	// modpackFileCap.
	maxModpackFile = 512 << 20 // 512 MB; no single mod jar comes close
	// modpackSizeSlack is the headroom allowed over a declared size before a
	// download is called oversized. A manifest that is a few KB out is normal.
	modpackSizeSlack = 64 << 10
	mrpackTempDir    = ".dylaris-mrpack"
)

// modpackFileCap converts a manifest-declared file size into the byte ceiling
// for that download.
//
// Every value that is not a usable size - zero, absent, negative, or larger
// than any real mod file - falls back to maxModpackFile. Negative is the one
// that mattered: the cap used to be computed as fileSize+slack and handed
// straight to downloadFileBounded, where a value <= 0 selected the 4 GB
// WHOLE-PACK cap as the limit for a SINGLE file. A pack declaring
// "fileSize": -100000 for each of its entries therefore got 4 GB per file, and
// because the running total added those negative numbers it went DOWN with
// every entry, so the 4 GB aggregate cap could never trip either. Both caps
// were defeated by the same field, and the field belongs to the attacker: the
// host allowlist admits github.com and raw.githubusercontent.com, where anyone
// can publish an .mrpack.
func modpackFileCap(declared int64) int64 {
	if declared > 0 && declared+modpackSizeSlack <= maxModpackFile {
		return declared + modpackSizeSlack
	}
	return maxModpackFile
}

// loadExtraModpackHosts merges operator-trusted hosts from MODPACK_MIRROR_HOSTS
// (comma-separated, e.g. a mirror of the operator's own) into
// modpackAllowedHosts. Core's public host does NOT need to be listed here any
// more - the node is told it on every login (coreMirrorHost). Call once at startup,
// before any command processing — modpackAllowedHosts is a package-level map
// with no synchronization, so mutating it concurrently with validateMrpackURL
// reads would race.
func loadExtraModpackHosts() {
	raw := os.Getenv("MODPACK_MIRROR_HOSTS")
	if raw == "" {
		return
	}
	for _, h := range strings.Split(raw, ",") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			modpackAllowedHosts[h] = true
			log.Printf("modpack: allowlisted extra mirror host %q via MODPACK_MIRROR_HOSTS", h)
		}
	}
}

// coreMirrorHost is the host Core named for itself on this node's last
// successful login (AuthResult.modpack_mirror_host). A pack BUILT in the panel
// is served from there, and nothing else on the node knows that address: the
// node is configured with Core's gRPC address, which is a different name in
// every real deploy.
//
// Before this, the only way to allow it was MODPACK_MIRROR_HOSTS, an env var an
// operator had to know about. Nobody set it - not on our own nodes, not in the
// deploy files the panel writes for customers - so a built pack could not be
// installed anywhere.
//
// An atomic rather than an entry in modpackAllowedHosts: a login runs
// concurrently with an install, and that map is written once at startup and
// read without synchronization from then on.
var coreMirrorHost atomic.Value // string

func coreMirrorHostValue() string {
	h, _ := coreMirrorHost.Load().(string)
	return h
}

// setCoreMirrorHost records what Core named. Empty means "Core names none",
// never "forget the one you have" - the same rule as the Redis address on the
// same message, so an older Core (or a replica that has no public URL
// configured) cannot revoke a host the node was told by another.
func setCoreMirrorHost(host string) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return
	}
	if prev, _ := coreMirrorHost.Load().(string); prev != host {
		log.Printf("modpack: pack downloads from Core host %q are allowed", host)
	}
	coreMirrorHost.Store(host)
}

// validateMrpackArchiveURL checks the URL of the pack ARCHIVE, which Core named
// itself. It is the one place Core's own host is accepted; the per-file
// downloads inside the manifest stay on the published allowlist, because those
// URLs come from whoever wrote the pack.
func validateMrpackArchiveURL(u string) error {
	err := validateMrpackURL(u)
	if err == nil {
		return nil
	}
	host, herr := mrpackURLHost(u)
	if herr != nil {
		return herr
	}
	mirror, _ := coreMirrorHost.Load().(string)
	if mirror != "" && host == mirror {
		return nil
	}
	if mirror == "" {
		// The likeliest reason by far, and one an operator can act on: the
		// address was set in the panel after this node connected.
		return fmt.Errorf("%w (Core has not named a public address to this node; it is told one when it connects)", err)
	}
	return err
}

// validateCoreURL accepts only a URL on the host Core named for itself, for
// files Core serves from its own storage (the library).
func validateCoreURL(u string) error {
	host, err := mrpackURLHost(u)
	if err != nil {
		return err
	}
	mirror, _ := coreMirrorHost.Load().(string)
	if mirror == "" {
		return fmt.Errorf("Core has not named a public address to this node; it is told one when it connects")
	}
	if host != mirror {
		return fmt.Errorf("url host %q is not Core's address", host)
	}
	return nil
}

func installModpack(destDir string, cfg InstallerConfig) error {
	if cfg.URL == "" {
		return fmt.Errorf("modpack installer requires URL")
	}
	if err := validateMrpackArchiveURL(cfg.URL); err != nil {
		return err
	}

	log.Printf("modpack: downloading %s for %s", cfg.URL, destDir)
	// A fixed name in a directory the tenant writes: a link planted under it
	// was adopted by MkdirAll and followed by OpenRoot, and the node created
	// pack.mrpack wherever it pointed. Opened without following a link.
	defer os.RemoveAll(filepath.Join(destDir, mrpackTempDir))
	tmp, releaseTmp, err := pinDir(destDir, mrpackTempDir, true)
	if err != nil {
		return fmt.Errorf("prepare %s: %w", mrpackTempDir, err)
	}
	defer releaseTmp()

	tmpRoot, err := os.OpenRoot(tmp)
	if err != nil {
		return err
	}
	defer tmpRoot.Close()
	mrpackPath := filepath.Join(tmp, "pack.mrpack")
	// Only Core's own archive may come from a non-public address: a self-hosted
	// Core often has one. Everything a pack author chose goes through the guard.
	fetch := downloadBoundedGuarded
	if host, _ := mrpackURLHost(cfg.URL); host != "" && host == coreMirrorHostValue() {
		fetch = downloadBoundedInto
	}
	if _, err := fetch(tmpRoot, "pack.mrpack", cfg.URL, maxMrpackSize); err != nil {
		return fmt.Errorf("download .mrpack: %w", err)
	}

	idx, err := readMrpackIndex(mrpackPath)
	if err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if idx.Game != "" && idx.Game != "minecraft" {
		return fmt.Errorf("unsupported game %q in mrpack", idx.Game)
	}

	if err := extractOverrides(mrpackPath, destDir); err != nil {
		return fmt.Errorf("extract overrides: %w", err)
	}

	// Fan out every listed file. Skip client-only.
	//
	// The running total counts bytes actually WRITTEN, not the sizes the
	// manifest declares. Declared sizes cannot bound anything on their own: a
	// pack that lies about them low still writes real bytes to the node's disk,
	// and one that declares them negative used to walk the total backwards.
	var totalBytes int64
	for _, f := range idx.Files {
		if env := f.Env["server"]; env == "unsupported" {
			continue
		}
		n, err := fetchModpackFile(destDir, f)
		totalBytes += n
		if err != nil {
			log.Printf("modpack: file %s failed: %v", f.Path, err)
			return fmt.Errorf("file %s: %w", f.Path, err)
		}
		if totalBytes > maxModpackTotalSize {
			return fmt.Errorf("modpack exceeds %d byte cap", int64(maxModpackTotalSize))
		}
	}

	// Chain into the loader install. Dependencies map gives us the MC
	// version + loader version. Loader choice derives from which key is set.
	mcVersion := idx.Dependencies["minecraft"]
	if mcVersion == "" {
		return fmt.Errorf("modpack manifest missing minecraft dependency")
	}
	if fl := idx.Dependencies["fabric-loader"]; fl != "" {
		log.Printf("modpack: chaining fabric mc=%s loader=%s", mcVersion, fl)
		return installFabric(destDir, mcVersion, fl)
	}
	if ql := idx.Dependencies["quilt-loader"]; ql != "" {
		log.Printf("modpack: chaining quilt mc=%s loader=%s (using fabric installer as fallback)", mcVersion, ql)
		return installFabric(destDir, mcVersion, ql)
	}
	if fb := idx.Dependencies["forge"]; fb != "" {
		log.Printf("modpack: chaining forge mc=%s loader=%s", mcVersion, fb)
		return installForge(destDir, mcVersion, fb, cfg.JavaImage, cfg.ServerUUID)
	}
	if nf := idx.Dependencies["neoforge"]; nf != "" {
		log.Printf("modpack: chaining neoforge loader=%s", nf)
		return installNeoForge(destDir, nf, cfg.JavaImage, cfg.ServerUUID)
	}
	// No loader listed → vanilla.
	return installVanilla(destDir, mcVersion)
}

// mrpackURLHost is the host of a download URL, https only. A download that is
// not https is refused before its host is looked at at all: a pack archive
// carries no hash of its own, so plain HTTP would be a pack anyone on the path
// can replace.
func mrpackURLHost(u string) (string, error) {
	if !strings.HasPrefix(u, "https://") {
		return "", fmt.Errorf(".mrpack url must be https")
	}
	rest := strings.TrimPrefix(u, "https://")
	slash := strings.IndexByte(rest, '/')
	if slash < 1 {
		return "", fmt.Errorf("bad .mrpack url")
	}
	return strings.ToLower(rest[:slash]), nil
}

func validateMrpackURL(u string) error {
	host, err := mrpackURLHost(u)
	if err != nil {
		return err
	}
	if !modpackAllowedHosts[host] {
		return fmt.Errorf(".mrpack url host %q not allowed", host)
	}
	return nil
}

func readMrpackIndex(path string) (*mrpackIndex, error) {
	rd, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	for _, f := range rd.File {
		if f.Name != "modrinth.index.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		var idx mrpackIndex
		if err := json.NewDecoder(rc).Decode(&idx); err != nil {
			return nil, err
		}
		return &idx, nil
	}
	return nil, fmt.Errorf("modrinth.index.json not found in archive")
}

// extractOverrides walks overrides/ and server-overrides/ inside the
// .mrpack zip and copies entries into destDir. server-overrides applies
// on top so server-only tweaks override client values for the same path.
func extractOverrides(mrpackPath, destDir string) error {
	zf, err := openNoFollow(mrpackPath)
	if err != nil {
		return err
	}
	defer zf.Close()
	st, err := zf.Stat()
	if err != nil {
		return err
	}
	rd, err := zip.NewReader(zf, st.Size())
	if err != nil {
		return err
	}
	budget := restoreDiskBudget(destDir)

	root, err := openRootMk(destDir)
	if err != nil {
		return err
	}
	defer root.Close()

	prefixes := []string{"overrides/", "server-overrides/"}
	for _, prefix := range prefixes {
		for _, f := range rd.File {
			if !strings.HasPrefix(f.Name, prefix) || f.Name == prefix {
				continue
			}
			rel := strings.TrimPrefix(f.Name, prefix)
			// The Root is the runtime boundary; extractSkip drops an entry whose
			// name escapes destDir lexically or through a link already planted
			// there (destDir is tenant-writable before the install runs).
			// Overrides fail the install rather than silently drop an entry -
			// a modpack that cannot lay down a file is a broken pack, not a
			// warning. (The upload-zip extractor skips instead; that is a
			// user-assembled archive, not a resolved pack.)
			name, skip := extractSkip(destDir, rel)
			if skip {
				return fmt.Errorf("unsafe path in mrpack: %s", f.Name)
			}
			if f.FileInfo().IsDir() {
				if err := root.MkdirAll(name, 0o755); err != nil {
					return err
				}
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return err
			}
			werr := writeFileInto(root, name, 0o644, &budgetReader{r: rc, left: &budget}, 0)
			rc.Close()
			if werr != nil {
				return werr
			}
		}
	}
	return nil
}

// fetchModpackFile downloads one manifest entry and returns the bytes it left
// on disk, so the caller can bound the pack by what was really written.
func fetchModpackFile(destDir string, f mrpackFile) (int64, error) {
	if len(f.Downloads) == 0 {
		return 0, fmt.Errorf("no download URLs")
	}
	// The manifest names the path, the tenant owns the directory it lands in, so
	// the file is created THROUGH a Root at destDir; extractRel folds traversal.
	name, err := extractRel(f.Path)
	if err != nil {
		return 0, fmt.Errorf("unsafe path %q: %w", f.Path, err)
	}
	root, err := openRootMk(destDir)
	if err != nil {
		return 0, err
	}
	defer root.Close()

	var lastErr error
	for _, u := range f.Downloads {
		if err := validateMrpackURL(u); err != nil {
			lastErr = err
			continue
		}
		tmp := name + ".part"
		n, err := downloadBoundedGuarded(root, tmp, u, modpackFileCap(f.FileSize))
		if err != nil {
			lastErr = err
			continue
		}
		// Verify sha512 if provided.
		if want := f.Hashes["sha512"]; want != "" {
			got, err := hashInRoot(root, tmp)
			if err != nil {
				root.Remove(tmp)
				lastErr = err
				continue
			}
			if !strings.EqualFold(got, want) {
				root.Remove(tmp)
				lastErr = fmt.Errorf("sha512 mismatch for %s", f.Path)
				continue
			}
		}
		if err := root.Rename(tmp, name); err != nil {
			root.Remove(tmp)
			return 0, err
		}
		chownForMCIn(root, name)
		return n, nil
	}
	return 0, lastErr
}

// downloadFileBounded streams a URL to disk with a sane timeout + size cap and
// returns the bytes written. Named distinctly from the installer.go
// downloadFile (which is unbounded and used by single-jar installers) so we
// don't shadow that helper.
//
// The cap is ENFORCED, not merely requested. It reads one byte past it and
// fails if that byte arrives - the standard probe. That probe was already here
// and its result was discarded, so an oversized response was silently truncated
// to the cap and reported as a successful download: for a mod file with no
// sha512 in the manifest, a corrupt jar was then renamed into place as if it
// were the real one.
//
// It is unguarded because Core's own mirror may sit on a private address, so it
// must not follow a redirect off that host: the tenant picks the PATH on Core's
// host, and one open redirect there would otherwise point this node at Redis,
// Postgres or the metadata service with the answer written into their server.
func downloadBoundedInto(root *os.Root, name, url string, maxBytes int64) (int64, error) {
	return downloadBoundedWith(&http.Client{
		Timeout:       5 * time.Minute,
		Transport:     uaTransport{base: http.DefaultTransport},
		CheckRedirect: sameHostRedirect,
	}, root, name, url, maxBytes)
}

// sameHostRedirect follows a redirect only to the host the request started on.
func sameHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	if req.URL.Host != via[0].URL.Host {
		return fmt.Errorf("refused redirect off %s", via[0].URL.Host)
	}
	return nil
}

// downloadBoundedGuarded is downloadBoundedInto through the client that refuses
// every non-public address. The host allowlist only covers the FIRST URL: a
// redirect from an allowlisted host could otherwise point the node at its
// private network, and the response lands in the tenant's server.
func downloadBoundedGuarded(root *os.Root, name, url string, maxBytes int64) (int64, error) {
	return downloadBoundedWith(&http.Client{Timeout: 5 * time.Minute, Transport: guardedDownloadClient.Transport}, root, name, url, maxBytes)
}

func downloadBoundedWith(client *http.Client, root *os.Root, name, url string, maxBytes int64) (int64, error) {
	resp, err := client.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("status %d", resp.StatusCode)
	}
	out, err := createIn(root, name, 0o644)
	if err != nil {
		return 0, err
	}
	if maxBytes <= 0 {
		maxBytes = maxModpackFile
	}
	n, copyErr := io.Copy(out, io.LimitReader(resp.Body, maxBytes+1))
	closeErr := out.Close()
	switch {
	case copyErr != nil:
		root.Remove(name)
		return 0, copyErr
	case closeErr != nil:
		root.Remove(name)
		return 0, closeErr
	case n > maxBytes:
		root.Remove(name)
		return 0, fmt.Errorf("download exceeds its %d byte limit", maxBytes)
	}
	return n, nil
}

// hashInRoot returns the sha512 hex of name inside root.
func hashInRoot(root *os.Root, name string) (string, error) {
	f, err := root.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha512.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
