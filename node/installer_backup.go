package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"dylaris-pkg/queue"
)

// backupImportArchiveName is the file the panel uploads into the sub-server
// directory before a setup of type "backup", the same way an upload-zip install
// finds ".upload.zip" already sitting there.
//
// A dotfile inside the directory being installed into rather than a temp path
// elsewhere: the upload path a panel and the Beam client already share writes
// into the sub-server directory, and giving this its own destination would mean
// a second upload route to secure.
//
// The name deliberately does NOT begin with ".dylaris". That prefix is the
// platform-reserved namespace and isPlatformReservedName refuses every WRITE to
// it, so the first name this had - ".dylaris-import.tar.gz" - could not be
// uploaded through the Beam client at all, while the HTTP path (which checks
// the narrower isProtectedFile set) allowed it. Reserved means "the user may
// not write this"; an archive the user supplies is the opposite of that.
// TestImportArchiveNameIsWritable holds the two together.
const backupImportArchiveName = ".upload-backup.tar.gz"

// maxImportManifestBytes bounds the description read out of an untrusted
// archive. The manifest is a few kilobytes of JSON for a server with hundreds of
// mods; anything approaching this is not a manifest, and reading it into memory
// on the strength of a header written by whoever produced the archive is exactly
// the thing to bound.
const maxImportManifestBytes = 4 << 20 // 4 MiB

// installFromBackupArchive restores a Dylaris backup archive into a fresh
// sub-server directory and returns the archive's own description, when it has
// one.
//
// The manifest is RETURNED rather than written to disk. It describes the
// sub-server, it is not part of it - extracting it would leave a stale
// description inside a live server directory, which the next backup would then
// archive: a manifest nested inside a manifest, describing a different backup.
//
// A nil manifest is an ordinary answer, not a failure. Archives written before
// manifests existed have none, and so does anything a user assembled by hand.
// Core installs the files and lets the operator set the loader, versions and
// mods themselves, which is what an import without a description can honestly
// offer.
func installFromBackupArchive(destDir string) ([]byte, error) {
	archivePath := filepath.Join(destDir, backupImportArchiveName)
	manifest, err := unpackBackupArchive(archivePath, destDir)
	if err != nil {
		// Left in place on failure, so a retry does not have to be re-uploaded.
		return nil, err
	}
	// AFTER unpacking has closed its handles, never inside it. A deferred Close
	// runs when the function returns, which is after a remove written next to it
	// - and on Windows that remove fails outright rather than quietly deleting a
	// still-open file the way Linux does. The archive would then have been left
	// behind on the very path that is supposed to clean it up.
	if err := os.Remove(archivePath); err != nil && !os.IsNotExist(err) {
		log.Printf("backup import: could not remove %s: %v", archivePath, err)
	}
	return manifest, nil
}

// unpackBackupArchive extracts archivePath into destDir and returns the
// manifest it carried, closing every handle it opened before it returns.
func unpackBackupArchive(archivePath, destDir string) ([]byte, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("backup archive not found at %s: %w", backupImportArchiveName, err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gr.Close()

	var manifest []byte
	tr := tar.NewReader(gr)
	for {
		hdr, terr := tr.Next()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			return nil, fmt.Errorf("tar read: %w", terr)
		}

		// The archive's own description: read it, do not extract it.
		if isManifestEntry(hdr.Name) {
			if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 && hdr.Size <= maxImportManifestBytes {
				b, rerr := io.ReadAll(io.LimitReader(tr, maxImportManifestBytes))
				if rerr == nil && json.Valid(b) {
					manifest = b
				}
			}
			continue
		}

		// Path-traversal guard. An archive is a file a user supplied, so an
		// entry may name "..", an absolute path, or a symlink pointing out of
		// the tree. Same clean+prefix check the restore path applies, for the
		// same reason: this writes into a directory that is bind-mounted into a
		// tenant's container.
		cleanPath := filepath.Join(destDir, filepath.Clean("/"+hdr.Name))
		if !strings.HasPrefix(cleanPath, destDir+string(os.PathSeparator)) && cleanPath != destDir {
			log.Printf("backup import: skipping unsafe entry %q", hdr.Name)
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(cleanPath, os.FileMode(hdr.Mode)); err != nil {
				return nil, fmt.Errorf("mkdir: %w", err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(cleanPath), 0o755); err != nil {
				return nil, fmt.Errorf("mkdir: %w", err)
			}
			out, oerr := os.OpenFile(cleanPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode))
			if oerr != nil {
				return nil, fmt.Errorf("create %s: %w", hdr.Name, oerr)
			}
			// Bounded by the header's own size rather than copied to EOF, so a
			// lying header cannot fill the disk from a small archive.
			if _, cerr := io.Copy(out, io.LimitReader(tr, hdr.Size)); cerr != nil {
				out.Close()
				return nil, fmt.Errorf("write %s: %w", hdr.Name, cerr)
			}
			out.Close()
		default:
			// Symlinks and hard links are dropped rather than recreated. A link
			// in a user-supplied archive is a write primitive pointed wherever
			// its target says, and nothing a Minecraft server needs to run
			// depends on one.
			log.Printf("backup import: skipping non-regular entry %q", hdr.Name)
		}
	}

	return manifest, nil
}

// reportSetup publishes what an install turned out to describe on this node's
// OWN setup channel, so Core can attribute it to the node that hosts the server.
// Same reasoning as reportBackup and reportRestore; see queue.SetupResultsChannel.
//
// Silent no-op when there is nothing to say. Every installer except a backup
// import returns no manifest, and publishing an empty report for each of them
// would put a message on the wire for every setup in the fleet to say nothing.
func reportSetup(ctx context.Context, rdb *redis.Client, serverUUID, subServer string, manifest []byte) {
	if len(manifest) == 0 {
		return
	}
	payload := map[string]interface{}{
		"serverUuid": serverUUID,
		"subServer":  subServer,
		"manifest":   json.RawMessage(manifest),
		"timestamp":  time.Now().Unix(),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("setup report: marshal failed: %v", err)
		return
	}
	if err := rdb.Publish(ctx, queue.SetupResultsChannel(nodeID), data).Err(); err != nil {
		// Logged, never fatal. The files are installed either way; what is lost
		// is the automatic loader/mod configuration, which the operator can
		// still set by hand - the same position an archive with no manifest
		// leaves them in.
		log.Printf("setup report publish failed: %v", err)
	}
}
