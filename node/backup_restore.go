package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/redis/go-redis/v9"

	"dylaris-pkg/queue"
)

// BackupRestoreCommand is the payload Core pushes onto the node queue to
// roll a sub-server (or the whole container) back to a previous backup.
type BackupRestoreCommand struct {
	RunID      int             `json:"runId"`
	RestoreID  int             `json:"restoreId"`
	JobID      int             `json:"jobId"`
	ServerUUID string          `json:"serverUuid"`
	SubServer  string          `json:"subServer"`
	StorageKey string          `json:"storageKey"`
	Storage    json.RawMessage `json:"storage"`
	// PresignedGetURL, when set, is a pre-signed S3/R2 GET URL the node downloads
	// from instead of using bucket credentials (BYON tenant nodes). Empty = use
	// Storage creds (operator nodes).
	PresignedGetURL string `json:"presignedGetUrl"`
}

// createStageDir makes the directory a restore extracts into, beside targetDir
// so the swap that follows is a same-filesystem rename.
//
// The name is RANDOM, not a timestamp, and it is CREATED rather than adopted.
// For a sub-server restore this path sits inside the server root - which is
// bind-mounted into the tenant's own MC container at /data, so anything running
// there can plant a symlink under any name it can predict. os.MkdirAll would
// have taken that link (Stat follows links, sees a directory, returns nil), the
// whole extraction would have written through it, and the swap renames the LINK
// into place, because rename does not follow one. A second-resolution timestamp
// is predictable enough to plant a minute of candidates for. MkdirTemp picks the
// name itself and fails on anything that already exists, so neither half works.
func createStageDir(targetDir string) (string, error) {
	// The "*" is where MkdirTemp puts its random; stageDirSuffix rides after
	// it so restore_cleanup.go can recognise the name without depending on how
	// many digits that random happened to have.
	dir, err := os.MkdirTemp(filepath.Dir(targetDir), filepath.Base(targetDir)+stageDirInfix+"*"+stageDirSuffix)
	if err != nil {
		return "", err
	}
	// MkdirTemp creates 0700; the restored tree is handed to the MC container,
	// which may not run as this process's user.
	if err := os.Chmod(dir, 0o755); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("stage dir perms: %w", err)
	}
	return dir, nil
}

// RunRestore streams the archive from storage straight into a tar reader
// and extracts on the fly into a staging directory. Once the extraction is
// complete the staging dir is swapped in atomically (rename) so a partial
// restore can never corrupt a running world. The container is stopped
// before the swap and started again afterwards.
func RunRestore(ctx context.Context, rdb *redis.Client, sm *StorageManager, dm *DockerManager, cmd BackupRestoreCommand) {
	// At-least-once delivery: a redelivery while this restore is still running
	// would extract into the same directory a second time and stop/restart the
	// container under the first. See backup_inflight.go.
	key := fmt.Sprintf("%d", cmd.RestoreID)
	if !restoresInFlight.enter(key) {
		log.Printf("backup_restore: restore %s is already running on this node, ignoring the redelivery", key)
		return
	}
	defer restoresInFlight.leave(key)

	started := time.Now()
	storage := storageInfo{}
	if err := json.Unmarshal(cmd.Storage, &storage); err != nil {
		reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "invalid storage payload: "+err.Error())
		return
	}

	rootDir := resolveServerRoot(sm, cmd.ServerUUID)
	targetDir := rootDir
	if cmd.SubServer != "" {
		targetDir = filepath.Join(rootDir, cmd.SubServer)
		// Same containment guard as the backup path. Here the consequence is a
		// WRITE: the staging directory and the rename that follows would land
		// outside the server root.
		if !withinRoot(rootDir, targetDir) {
			reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "sub-server escapes the server directory")
			return
		}
	}

	// Stage the new content in a sibling directory so we can swap it in
	// atomically. The sibling lives in the same filesystem as targetDir,
	// which keeps the rename truly atomic.
	stageDir, err := createStageDir(targetDir)
	if err != nil {
		reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "stage dir: "+err.Error())
		return
	}
	// On any error we'll remove the stage dir; success path will remove the
	// previous targetDir instead.
	stageCleanup := func() { os.RemoveAll(stageDir) }

	// Stop the container before touching disk. The CL is mainly for
	// sub-server restores too — a single container hosts every sub-server,
	// so a Minecraft server with an open world file is racing us either
	// way. Best to suspend and resume.
	if dm != nil {
		log.Printf("Restore %d: stopping container %s", cmd.RunID, cmd.ServerUUID)
		gracefulStop(rdb, cmd.ServerUUID, dm)
	}

	var body io.ReadCloser
	if cmd.PresignedGetURL != "" {
		body, err = downloadPresigned(ctx, cmd.PresignedGetURL)
	} else {
		body, err = downloadBackup(ctx, sm, cmd.ServerUUID, storage, cmd.StorageKey)
	}
	if err != nil {
		stageCleanup()
		reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "download failed: "+err.Error())
		return
	}
	defer body.Close()

	gr, err := gzip.NewReader(body)
	if err != nil {
		stageCleanup()
		reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "gzip open: "+err.Error())
		return
	}
	defer gr.Close()
	tr := tar.NewReader(gr)

	extracted := 0
	for {
		hdr, terr := tr.Next()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			stageCleanup()
			reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "tar read: "+terr.Error())
			return
		}
		// The archive's own description is not part of the server. Restoring it
		// would drop a .dylaris directory into a live server tree, where the
		// NEXT backup would archive it - a manifest nested inside a manifest,
		// describing the wrong backup. Core reads this entry from the archive
		// itself, never from a restored copy.
		if isManifestEntry(hdr.Name) {
			continue
		}
		// Path-traversal guard — entries can name "..", absolute paths,
		// etc. The clean+prefix check rejects anything that escapes stageDir.
		cleanPath := filepath.Join(stageDir, filepath.Clean("/"+hdr.Name))
		if !strings.HasPrefix(cleanPath, stageDir+string(os.PathSeparator)) && cleanPath != stageDir {
			log.Printf("Restore %d: skipping unsafe entry %q", cmd.RunID, hdr.Name)
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(cleanPath, os.FileMode(hdr.Mode)); err != nil {
				stageCleanup()
				reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "mkdir: "+err.Error())
				return
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(cleanPath), 0o755); err != nil {
				stageCleanup()
				reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "mkdir parent: "+err.Error())
				return
			}
			f, ferr := os.OpenFile(cleanPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode))
			if ferr != nil {
				stageCleanup()
				reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "open file: "+ferr.Error())
				return
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				stageCleanup()
				reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "copy: "+err.Error())
				return
			}
			f.Close()
			extracted++
		case tar.TypeSymlink, tar.TypeLink:
			// Skip links: an unvalidated link target could point outside the
			// staging dir and escape on later access. MC server backups don't
			// rely on links.
			log.Printf("restore: skipping link entry %q -> %q", hdr.Name, hdr.Linkname)
		default:
			// Skip block/char devices etc.
		}
	}

	if extracted == 0 {
		stageCleanup()
		reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "archive contained no regular files")
		return
	}

	// Atomic-swap: move current targetDir aside, move stageDir into place,
	// then remove the old contents in the background. The two renames are
	// the only on-disk window where the world looks weird, and that's
	// measured in milliseconds.
	backupDir := targetDir + ".pre-restore-" + time.Now().UTC().Format("20060102-150405")
	if _, statErr := os.Stat(targetDir); statErr == nil {
		if err := os.Rename(targetDir, backupDir); err != nil {
			stageCleanup()
			reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "stash original: "+err.Error())
			return
		}
	}
	if err := os.Rename(stageDir, targetDir); err != nil {
		// Roll back the previous stash so the world isn't left missing.
		os.Rename(backupDir, targetDir)
		stageCleanup()
		reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "swap stage: "+err.Error())
		return
	}
	if cmd.SubServer == "" {
		if err := carryArchivesAcrossSwap(backupDir, targetDir); err != nil {
			log.Printf("Restore %d: [warn] could not carry %s across the swap: %v", cmd.RunID, backupDirName, err)
		}
	}
	go os.RemoveAll(backupDir)

	// Bring the container back up. For sub-server restores we also flip
	// the active sub-server file if needed so the next start picks up
	// whatever the archive contained.
	if dm != nil {
		log.Printf("Restore %d: restarting container %s", cmd.RunID, cmd.ServerUUID)
		if err := dm.RestartContainer(cmd.ServerUUID); err != nil {
			log.Printf("Restore %d: container restart failed: %v", cmd.RunID, err)
		}
	}

	reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "success", "")
	log.Printf("Restore %d completed: %d files in %v", cmd.RunID, extracted, time.Since(started))
}

// carryArchivesAcrossSwap moves the live archive directory from the stashed
// old server root into the freshly restored one.
//
// A whole-server restore replaces the server ROOT, and the archives live
// inside it - so without this the atomic swap hands every OTHER backup to the
// background delete that follows. Restoring one backup must never destroy the
// rest, which is the difference between one bad restore and no way back at all.
//
// Anything the archive itself carried under that name is dropped first: only
// archives written before the backup side stopped nesting them contain a copy,
// and it is stale by definition. Sub-server restores never call this - their
// target is one level below the archives.
func carryArchivesAcrossSwap(stashedRoot, restoredRoot string) error {
	live := filepath.Join(stashedRoot, backupDirName)
	if _, err := os.Stat(live); err != nil {
		return nil // nothing to carry
	}
	target := filepath.Join(restoredRoot, backupDirName)
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	return os.Rename(live, target)
}

// downloadBackup returns an io.ReadCloser streaming the archive from
// whichever storage provider holds it. The caller is responsible for
// closing. For the node-local mode the source is on the same disk we'll
// extract into, so this is just a plain file open — no network hop.
func downloadBackup(ctx context.Context, sm *StorageManager, serverUUID string, info storageInfo, key string) (io.ReadCloser, error) {
	switch info.Provider {
	case "local", "shared":
		var cfg localCfg
		if err := json.Unmarshal(info.Config, &cfg); err != nil {
			return nil, fmt.Errorf("invalid local cfg: %w", err)
		}
		if cfg.BasePath == "" {
			return nil, fmt.Errorf("local storage requires basePath")
		}
		full := filepath.Join(cfg.BasePath, filepath.Clean("/"+key))
		return os.Open(full)

	case "node-local":
		archive := nodeLocalArchiveName(key)
		full := filepath.Join(resolveServerRoot(sm, serverUUID), backupDirName, archive)
		return os.Open(full)

	case "s3":
		client, bucket, err := buildS3Client(ctx, info.Config)
		if err != nil {
			return nil, err
		}
		out, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			return nil, err
		}
		return out.Body, nil

	default:
		return nil, fmt.Errorf("unknown provider %s", info.Provider)
	}
}

// downloadPresigned streams the archive from a pre-signed GET URL (BYON tenant
// nodes, which never receive bucket credentials). Caller closes the body.
func downloadPresigned(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("presigned get: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("presigned get status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp.Body, nil
}

// reportRestore publishes on this node's OWN restore channel so the Core's
// scheduler can update the backup_restores row matching this attempt, and can
// tell which node reported it. Same reasoning as reportBackup; see
// queue.BackupRestoresChannel.
func reportRestore(ctx context.Context, rdb *redis.Client, restoreID, runID int, status, errMsg string) {
	payload := map[string]interface{}{
		"restoreId": restoreID,
		"runId":     runID,
		"status":    status,
		"error":     errMsg,
		"timestamp": time.Now().Unix(),
	}
	data, _ := json.Marshal(payload)
	if err := rdb.Publish(ctx, queue.BackupRestoresChannel(nodeID), data).Err(); err != nil {
		log.Printf("restore result publish failed: %v", err)
	}
}
