package main

// Install/remove a single mod or plugin file inside a server's
// sub-server tree. Source URL is always cdn.modrinth.com (enforced core-side
// in handlers/server_mods.go before the queue dispatch, but we re-check here
// since a stale queued payload is technically a stale trust boundary).
//
// install_mod payload (from core):
//   uuid, activeSubServer, targetDir ("mods"|"plugins"), fileName, downloadUrl, sha512
//
// remove_mod payload (from core):
//   uuid, activeSubServer, targetDir, fileName
//
// Every field below that becomes part of a path is re-checked here for that
// reason, activeSubServer included - see validActiveSubServer.

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dylaris-pkg/queue"
	"dylaris-pkg/validate"

	"github.com/redis/go-redis/v9"
)

type installModPayload struct {
	UUID            string `json:"uuid"`
	ActiveSubServer string `json:"activeSubServer"`
	TargetDir       string `json:"targetDir"`
	FileName        string `json:"fileName"`
	DownloadURL     string `json:"downloadUrl"`
	SHA512          string `json:"sha512"`
	// PreviousFileName is the jar this install REPLACES, deleted only once the
	// new one is verified and in place. Empty for a first install, and equal to
	// FileName when a version keeps the same name - in which case the rename
	// has already replaced it and there is nothing left to remove.
	PreviousFileName string `json:"previousFileName"`
	// Identity for the report. Core cannot match a result to a row without it,
	// and InstallID additionally tells a late answer about a superseded attempt
	// apart from the answer to the current one.
	InstallID string `json:"installId"`
	ServerID  int    `json:"serverId"`
	ProjectID string `json:"projectId"`
}

// modInstallResult is what the node publishes back. Status is "installed" or
// "failed"; a failure carries the reason, because the operator reading the
// panel has no access to this node's log.
type modInstallResult struct {
	InstallID string `json:"installId"`
	ServerID  int    `json:"serverId"`
	SubServer string `json:"subServer"`
	ProjectID string `json:"projectId"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	Timestamp int64  `json:"timestamp"`
}

type removeModPayload struct {
	UUID            string `json:"uuid"`
	ActiveSubServer string `json:"activeSubServer"`
	TargetDir       string `json:"targetDir"`
	FileName        string `json:"fileName"`
}

// downloadModFile is the fetch step, named so a test can replace it.
//
// Only this step, and deliberately not validateModrinthURL: the host pin is a
// trust boundary, and a boundary that a variable can move is not one. A test
// therefore still passes a real cdn.modrinth.com URL and still goes through the
// same check - it just does not go to the network to decide what happens next.
var downloadModFile = downloadAndVerify

func runInstallMod(ctx context.Context, rdb *redis.Client, storage *StorageManager, payload string) {
	var pl installModPayload
	if err := json.Unmarshal([]byte(payload), &struct {
		Config *installModPayload `json:"config"`
	}{Config: &pl}); err != nil {
		// Nothing to report against: the identity Core matches a result to
		// lives inside the payload that would not decode.
		log.Printf("install_mod: decode failed: %v", err)
		return
	}
	// From here on every way out reports, the validation refusals included.
	fail := func(format string, args ...interface{}) {
		msg := fmt.Sprintf(format, args...)
		log.Printf("install_mod: %s", msg)
		reportModInstall(ctx, rdb, pl, "failed", msg)
	}
	if err := validateModrinthURL(pl.DownloadURL); err != nil {
		fail("%v", err)
		return
	}
	serverPath := storage.GetServerDir(pl.UUID)
	if serverPath == "" {
		fail("storage lookup for %s returned empty path", pl.UUID)
		return
	}
	if !validSubDir(pl.TargetDir) {
		fail("invalid target dir %q", pl.TargetDir)
		return
	}
	if !validActiveSubServer(pl.ActiveSubServer) {
		fail("invalid active sub-server %q", pl.ActiveSubServer)
		return
	}
	// Same rule Core now applies before queueing (validate.IsPlainFileName), so
	// a name that reaches here at all is one it already agreed to. Kept as a
	// check rather than trusted: this command arrives over a queue, and the node
	// is the last thing between it and the filesystem.
	cleanName := pl.FileName
	if !validate.IsPlainFileName(cleanName) {
		fail("invalid file name %q", pl.FileName)
		return
	}
	// GetServerDir returns the path up to <uuid>; the active sub-server and
	// targetDir are joined on through resolveWithinDir - the same traversal AND
	// symlink boundary the file browser, the beam server and the zip walkers go
	// through.
	//
	// This file joined the path by hand instead. Every check it did make
	// (validSubDir, validActiveSubServer, filepath.Base) guards the STRING; none
	// of them asks the filesystem, and the directory being joined onto is
	// bind-mounted into the tenant's OWN Minecraft container as /data. "ln -s
	// /app/dylaris_data mods" is one line in there. After it, os.MkdirAll adopts
	// the link and os.Create follows it, so installing a mod wrote a
	// Modrinth-hosted file - content the tenant picks, by publishing it - to any
	// path the node process can write, and removing one deleted any file it can
	// delete. resolveWithinDir refuses a component that resolves outside the
	// server root, and refuses a DANGLING link too: that one is a trap for
	// whatever creates the file next.
	//
	// Checked before MkdirAll on purpose - MkdirAll through a dangling link
	// creates the target.
	destDir, err := resolveWithinDir(serverPath, filepath.Join(pl.ActiveSubServer, pl.TargetDir))
	if err != nil {
		fail("%v", err)
		return
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		fail("mkdir %s: %v", destDir, err)
		return
	}
	// The .part file is the one the download actually opens, so it is the one
	// that has to be clean; destFile is only ever reached by os.Rename, which
	// does not follow a link.
	tmpFile, err := resolveWithinDir(destDir, cleanName+".part")
	if err != nil {
		fail("%v", err)
		return
	}
	destFile := filepath.Join(destDir, cleanName)

	if err := downloadModFile(pl.DownloadURL, tmpFile, pl.SHA512); err != nil {
		os.Remove(tmpFile)
		fail("download failed for %s: %v", cleanName, err)
		return
	}
	if err := os.Rename(tmpFile, destFile); err != nil {
		os.Remove(tmpFile)
		fail("rename %s to %s: %v", tmpFile, destFile, err)
		return
	}
	// ONLY now, and this order is the whole fix.
	//
	// The jar an update replaced was never removed, so an updated server carried
	// both builds in mods/ and loaded both. Deleting it FIRST - the obvious
	// reading of "replace" - is worse than the bug it fixes: a download that 404s
	// or fails its hash would leave the server with neither build. Download,
	// verify, swap in, and only then drop the old one.
	//
	// Deleting a jar the running server has open is safe and needs no stop. The
	// JVM holds the inode, not the name, so it keeps reading the old file until
	// it exits and picks the new one up on the next start - which is also why an
	// install has always only taken effect after a restart.
	if pl.PreviousFileName != "" && pl.PreviousFileName != cleanName {
		switch old, rerr := resolveWithinDir(destDir, pl.PreviousFileName); {
		case !validate.IsPlainFileName(pl.PreviousFileName):
			log.Printf("install_mod: not deleting previous %q: not a plain file name", pl.PreviousFileName)
		case rerr != nil:
			log.Printf("install_mod: previous %q: %v", pl.PreviousFileName, rerr)
		default:
			if err := os.Remove(old); err != nil && !os.IsNotExist(err) {
				// Still reported as installed below, because it IS: the new jar is
				// in place and only the cleanup failed. Calling that "failed" would
				// invite a retry that cannot help.
				log.Printf("install_mod: removing previous %s: %v", old, err)
			} else {
				log.Printf("install_mod: removed previous %s", pl.PreviousFileName)
			}
		}
	}
	log.Printf("install_mod: installed %s into %s", cleanName, destDir)
	reportModInstall(ctx, rdb, pl, "installed", "")
}

// reportModInstall publishes the outcome on THIS node's own channel.
//
// The channel name is the only thing Core can attribute a Pub/Sub message by -
// see pkg/queue/modchannels.go - so it is per node, and Core re-derives the
// server's node and compares. A publish that fails is logged and nothing more:
// the row then stays "installing", which is wrong in the safe direction, unlike
// a row claiming a success nobody confirmed.
func reportModInstall(ctx context.Context, rdb *redis.Client, pl installModPayload, status, message string) {
	if rdb == nil || pl.InstallID == "" {
		return
	}
	data, _ := json.Marshal(modInstallResult{
		InstallID: pl.InstallID,
		ServerID:  pl.ServerID,
		SubServer: pl.ActiveSubServer,
		ProjectID: pl.ProjectID,
		Status:    status,
		Message:   message,
		Timestamp: time.Now().Unix(),
	})
	if err := rdb.Publish(ctx, queue.ModResultsChannel(nodeID), data).Err(); err != nil {
		log.Printf("install_mod: result publish failed: %v", err)
	}
}

func runRemoveMod(storage *StorageManager, payload string) {
	var pl removeModPayload
	if err := json.Unmarshal([]byte(payload), &struct {
		Config *removeModPayload `json:"config"`
	}{Config: &pl}); err != nil {
		log.Printf("remove_mod: decode failed: %v", err)
		return
	}
	serverPath := storage.GetServerDir(pl.UUID)
	if serverPath == "" {
		log.Printf("remove_mod: storage lookup for %s returned empty path", pl.UUID)
		return
	}
	if !validSubDir(pl.TargetDir) {
		log.Printf("remove_mod: invalid target dir %q", pl.TargetDir)
		return
	}
	if !validActiveSubServer(pl.ActiveSubServer) {
		log.Printf("remove_mod: invalid active sub-server %q", pl.ActiveSubServer)
		return
	}
	cleanName := filepath.Base(pl.FileName)
	if cleanName == "" || cleanName == "." || cleanName == ".." || strings.ContainsAny(cleanName, "/\\") {
		log.Printf("remove_mod: invalid file name %q", pl.FileName)
		return
	}
	// Same boundary as the install side: os.Remove does not follow the leaf, but
	// a symlinked "mods" directory would still put the deletion outside the
	// server root.
	dir, err := resolveWithinDir(serverPath, filepath.Join(pl.ActiveSubServer, pl.TargetDir))
	if err != nil {
		log.Printf("remove_mod: %v", err)
		return
	}
	path := filepath.Join(dir, cleanName)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("remove_mod: rm %s: %v", path, err)
		return
	}
	log.Printf("remove_mod: removed %s", path)
}

func validateModrinthURL(u string) error {
	if !strings.HasPrefix(u, "https://cdn.modrinth.com/") {
		return fmt.Errorf("downloadUrl must point to cdn.modrinth.com (got %q)", truncate(u, 60))
	}
	return nil
}

func validSubDir(d string) bool {
	return d == "mods" || d == "plugins"
}

// validActiveSubServer is the check the header comment above already promised
// and this file did not make. Both functions filepath.Join the field onto the
// server's data directory, and Join CLEANS rather than confines: a name carrying
// ".." walks out of the server root, which for install_mod means writing a
// downloaded jar outside it and for remove_mod means deleting a file outside it.
// targetDir and fileName were both re-checked here; this one was not, and it is
// the field that arrives from a .active_server file on a node's own disk.
//
// Same rule Core applies (validate.IsSubServerName): one plain directory name.
// Empty is allowed - a server with no sub-server keeps its files at the root and
// Join skips an empty element.
func validActiveSubServer(s string) bool {
	return s == "" || validate.IsSubServerName(s)
}

func downloadAndVerify(url, dest, expectedSHA512 string) error {
	client := &http.Client{
		Timeout:   5 * time.Minute,
		Transport: uaTransport{base: http.DefaultTransport},
		// Core pins the download host to a Modrinth CDN before this command is
		// queued, and with the default policy that pin covered the first hop
		// only: Go follows up to ten redirects, so one hop off cdn.modrinth.com
		// would have fetched from anywhere, with the sha512 checked only when
		// the caller supplied one. Measured against the real CDN: a version
		// file answers 200 with zero redirects, so refusing a hop to a
		// different host costs nothing today and keeps Core's pin meaning what
		// it says. Same-host redirects still work, so a path rewrite would not
		// break installs.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) == 0 {
				return nil
			}
			if req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("refusing redirect to a different host: %s -> %s", via[0].URL.Host, req.URL.Host)
			}
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	// Remove-then-O_EXCL rather than os.Create: the caller resolved this path a
	// moment ago, but the tenant can re-plant a link at it from inside their
	// container between that check and this open. os.Remove takes the link
	// itself, O_EXCL refuses anything that reappears. Same pair backup_worker.go
	// uses for node-local archives.
	if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear previous partial download: %w", err)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	defer f.Close()

	h := sha512.New()
	// Cap downloads at 256 MB so a runaway / malicious URL can't fill the disk.
	//
	// The extra byte is the overflow probe, and it has to be ACTED on: without
	// the length check below, an oversized response was cut off at the cap and
	// reported as a successful download, so with no sha512 in the payload a
	// truncated jar got renamed into place and the server crash-looped on a
	// corrupt archive with nothing pointing at the size. Same shape as the bug in
	// downloadFileBounded (installer_modpack.go).
	const maxSize = 256 << 20
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	if n > maxSize {
		return fmt.Errorf("download exceeds the %d byte limit", int64(maxSize))
	}
	if expectedSHA512 != "" {
		got := hex.EncodeToString(h.Sum(nil))
		if !strings.EqualFold(got, expectedSHA512) {
			return fmt.Errorf("sha512 mismatch: want %s got %s", expectedSHA512, got)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
