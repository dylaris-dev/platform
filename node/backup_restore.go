package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

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
	// Download "presigned" means the storage is object storage: no credentials
	// and no URL in the command, the node asks Core for the URL. See
	// backup_transfer.go. Required for every provider this node does not handle
	// itself (requireCoreTransfer).
	Download string `json:"download"`
	// GuardedTransfer, AllowPrivate: see BackupRunCommand.
	GuardedTransfer bool     `json:"guardedTransfer"`
	AllowPrivate    []string `json:"allowPrivate"`
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
	ownStages.Store(filepath.Base(dir), true)
	// MkdirTemp creates 0700; the restored tree is handed to the MC container,
	// which may not run as this process's user.
	if err := os.Chmod(dir, 0o755); err != nil {
		os.RemoveAll(dir)
		releaseStageDir(dir)
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
	// Not while a backup of the server is archiving it: the backup walks the
	// tree this renames and then deletes, so it failed - and the backup's
	// save-on at the end could land on the restored world mid-swap.
	if reason := enterRestoreOfServer(cmd.ServerUUID); reason != "" {
		reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", reason)
		return
	}
	defer restoreServersInFlight.leave(cmd.ServerUUID)

	started := time.Now()
	storage := storageInfo{}
	if err := json.Unmarshal(cmd.Storage, &storage); err != nil {
		reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "invalid storage payload: "+err.Error())
		return
	}
	// Refused before the server is touched: a command this node cannot carry out
	// must not stop the server on its way to failing.
	if err := requireCoreTransfer(storage.Provider, cmd.Download == modeDownloadPresigned); err != nil {
		reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", err.Error())
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

	// Same interlock as setup/reinstall/migrate_storage, and for the same
	// reason: the restore stops a container whose desired_state is still
	// "online" and then renames its whole directory away. Without this the
	// reconciler starts the server back up mid-restore, against the very
	// directory the swap is about to replace. Taken only once this restore
	// holds the server: a refused duplicate that held it too deleted the
	// running restore's node_busy on its way out.
	releaseBusy := holdBusyStatus(rdb, cmd.ServerUUID, "restarting", busyStatusTTL)
	defer func() {
		releaseBusy()
		// The hold reported "restarting" throughout, and nothing else reports a
		// container that is not running: a restore of a stopped server (or one
		// whose restart failed) left that status for good, and Core refuses every
		// power action on a restarting server. A running one reports itself.
		// The checks above refuse before this: they touch neither the server
		// nor its status.
		if !serverDeleted(cmd.ServerUUID) && (dm == nil || !containerRunning(dm, cmd.ServerUUID)) {
			rdb.Set(context.Background(), fmt.Sprintf("dylaris:server:%s:status", cmd.ServerUUID), "stopped", 30*time.Second)
		}
	}()

	// Stage the new content in a sibling directory so we can swap it in
	// atomically. The sibling lives in the same filesystem as targetDir,
	// which keeps the rename truly atomic.
	stageDir, err := createStageDir(targetDir)
	if err != nil {
		reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "stage dir: "+err.Error())
		return
	}
	defer releaseStageDir(stageDir)
	// On any error we'll remove the stage dir; success path will remove the
	// previous targetDir instead.
	stageCleanup := func() { os.RemoveAll(stageDir) }

	// Asked for BEFORE the container stops: a restore Core refuses, or a Core
	// this node cannot reach, must not take the server down on its way to
	// failing. The URL outlives the stop (30 minutes against a stop of well
	// under one), and a retry below asks for a fresh one anyway.
	var restoreURL string
	if cmd.Download == modeDownloadPresigned {
		restoreURL, err = coreRestoreURL(cmd.RestoreID)(ctx)
		if err != nil {
			stageCleanup()
			reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "download failed: "+err.Error())
			return
		}
	}

	// Stop the container before touching disk. The CL is mainly for
	// sub-server restores too — a single container hosts every sub-server,
	// so a Minecraft server with an open world file is racing us either
	// way. Best to suspend and resume.
	// The server comes back up only if it was running. Every failure after the
	// stop returned without starting it again, and a success started it even
	// when its owner had it stopped.
	wasRunning := dm != nil && containerRunning(dm, cmd.ServerUUID)
	if dm != nil {
		log.Printf("Restore %d: stopping container %s", cmd.RunID, cmd.ServerUUID)
		if err := gracefulStop(rdb, cmd.ServerUUID, dm); err != nil {
			// Nothing on disk is touched yet: the swap below must not run under
			// a server that is still writing its world.
			stageCleanup()
			reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "stop the server: "+err.Error())
			return
		}
	}
	defer func() {
		if wasRunning {
			log.Printf("Restore %d: starting container %s again", cmd.RunID, cmd.ServerUUID)
			if err := dm.RestartContainer(cmd.ServerUUID); err != nil {
				log.Printf("Restore %d: container restart failed: %v", cmd.RunID, err)
			}
		}
	}()

	// The body has no deadline (a restore may stream for hours), so a source
	// that stops sending ends it instead: a tenant's own bucket could trickle a
	// byte a minute and hold this command slot for good. A window must bring
	// restoreStallMinBytes, not just any byte, or that trickle re-arms it.
	dlCtx, dlCancel := context.WithCancel(ctx)
	defer dlCancel()
	var body io.ReadCloser
	switch {
	case cmd.Download == modeDownloadPresigned:
		body, err = openPresignedRestore(dlCtx, transferClient(cmd.GuardedTransfer, cmd.AllowPrivate), restoreURL, coreRestoreURL(cmd.RestoreID))
	default:
		body, err = downloadBackup(ctx, sm, cmd.ServerUUID, storage, cmd.StorageKey)
	}
	if err != nil {
		stageCleanup()
		reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "download failed: "+err.Error())
		return
	}
	defer body.Close()
	stall := time.AfterFunc(restoreStallTimeout, dlCancel)
	defer stall.Stop()
	// The stall guard is a minimum rate, and bytes that are never written to
	// disk (a manifest entry with a huge size, gzip members after the tar end)
	// pass no budget either: an endless stream at that rate held the slot for
	// good. This bounds the whole restore, however fast the source sends.
	deadline := time.AfterFunc(restoreDeadline(archiveSize(body)), dlCancel)
	defer deadline.Stop()

	gr, err := gzip.NewReader(&stallReader{r: body, t: stall, d: restoreStallTimeout, min: restoreStallMinBytes})
	if err != nil {
		stageCleanup()
		reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "gzip open: "+err.Error())
		return
	}
	defer gr.Close()
	stageRoot, err := openRootMk(stageDir)
	if err != nil {
		stageCleanup()
		reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "open stage: "+err.Error())
		return
	}
	stageClosed := false
	closeStage := func() {
		if !stageClosed {
			stageRoot.Close()
			stageClosed = true
		}
	}
	defer closeStage()
	tr := tar.NewReader(gr)

	// The stage sits beside the server, outside its disk limit, and the archive
	// can come from a bucket the tenant writes to. Extraction stops before it
	// would leave the disk below its reserve, which every other server here
	// needs as much as this one.
	//
	// Shared with every other write running on this node (copies, imports,
	// unpacks, other restores): each used to reckon with the whole free space,
	// and two at once filled the disk every server here runs on. The server's
	// own disk limit is NOT applied: a restore holds the old world beside the
	// new one until the swap, so it would refuse any server past half its limit.
	budget := restoreDiskBudget(filepath.Dir(stageDir)) - inflightWrites.Load()
	var written int64
	defer func() { inflightWrites.Add(-written) }()
	extracted, entries := 0, 0
	// The directories creating an entry makes, its own and the parents
	// MkdirAll adds: an archive of deep paths built thousands of directories
	// per counted entry.
	madeDirs := map[string]bool{}
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
		// Entries are created THROUGH a Root at stageDir, so any residual link
		// or traversal is refused there; extractRel folds "..", absolute names
		// and the root itself.
		name, skip := extractSkip(stageDir, hdr.Name)
		if skip {
			log.Printf("Restore %d: skipping unsafe entry %q", cmd.RunID, hdr.Name)
			continue
		}
		// Every entry costs an inode and at least a block, which a byte budget
		// over file contents never sees: an archive of millions of empty
		// directories is small in the bucket and exhausts the node's inodes.
		cost := 1
		for d := path.Dir(name); d != "." && d != "/" && !madeDirs[d]; d = path.Dir(d) {
			madeDirs[d] = true
			cost++
		}
		if hdr.Typeflag == tar.TypeDir {
			madeDirs[name] = true
		}
		entries += cost
		written += int64(cost) * restoreEntryCost
		inflightWrites.Add(int64(cost) * restoreEntryCost)
		if entries > maxRestoreEntries || written > budget {
			stageCleanup()
			reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", errRestoreDiskBudget.Error())
			return
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := stageRoot.MkdirAll(name, os.FileMode(hdr.Mode)); err != nil {
				stageCleanup()
				reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "mkdir: "+err.Error())
				return
			}
		case tar.TypeReg, tar.TypeRegA:
			f, ferr := createIn(stageRoot, name, os.FileMode(hdr.Mode))
			if ferr != nil {
				stageCleanup()
				reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "open file: "+ferr.Error())
				return
			}
			n, err := io.Copy(f, io.LimitReader(tr, budget-written+1))
			written += n
			inflightWrites.Add(n)
			if err == nil && written > budget {
				err = errRestoreDiskBudget
			}
			if err != nil {
				f.Close()
				stageCleanup()
				reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "copy: "+err.Error())
				return
			}
			f.Close()
			// Handed over now, while the name is the file's only one: a
			// TypeLink entry may add a second, and the start-time ownership
			// walk skips every file with two names.
			if !nodeOwnedRestoreName(cmd.SubServer, name) {
				chownForMCIn(stageRoot, name)
			}
			extracted++
		case tar.TypeLink:
			// The second name of a file archived once (see writeServerArchive).
			// Both names go through the stage Root, so the link cannot reach
			// out of it.
			old, skipOld := extractSkip(stageDir, hdr.Linkname)
			if skipOld || nodeOwnedRestoreName(cmd.SubServer, name) || mkdirParentIn(stageRoot, name) != nil || stageRoot.Link(old, name) != nil {
				log.Printf("restore: skipping link entry %q -> %q", hdr.Name, hdr.Linkname)
			}
		case tar.TypeSymlink:
			// Skip links: an unvalidated link target could point outside the
			// staging dir and escape on later access. MC server backups don't
			// rely on links.
			log.Printf("restore: skipping link entry %q -> %q", hdr.Name, hdr.Linkname)
		default:
			// Skip block/char devices etc.
		}
	}

	// tar stops at its end-of-archive blocks; the gzip CRC is checked only when
	// the stream is read to its own end. Without this, an archive that rotted
	// at rest was swapped in as success and the good world deleted.
	if _, err := io.Copy(io.Discard, gr); err != nil {
		stageCleanup()
		reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "archive checksum: "+err.Error())
		return
	}

	// Close the stage Root BEFORE the swap: the swap renames stageDir itself,
	// and on Windows an open handle on a directory blocks renaming it. The Root
	// was only needed to write the entries, which is done. Idempotent, so the
	// deferred close on an error path does not double-close.
	closeStage()

	if extracted == 0 {
		stageCleanup()
		reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "archive contained no regular files")
		return
	}

	// A delete of the server is not ordered against a restore and ran beside
	// it: the stage sits outside the removed directory, and the swap below put
	// the deleted server's world back on disk, quota project and all.
	if serverDeleted(cmd.ServerUUID) {
		stageCleanup()
		wasRunning = false
		reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", errRestoreServerDeleted)
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
		if rerr := os.Rename(backupDir, targetDir); rerr != nil {
			// The stash is now the only copy of the world.
			keepStash(cmd.RunID, backupDir)
		}
		stageCleanup()
		reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", "swap stage: "+err.Error())
		return
	}
	// The delete can also land between the check above and the swap, after its
	// RemoveAll already ran.
	if serverDeleted(cmd.ServerUUID) {
		os.RemoveAll(targetDir)
		os.RemoveAll(backupDir)
		wasRunning = false
		reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "failed", errRestoreServerDeleted)
		return
	}
	carried := true
	if cmd.SubServer == "" {
		if err := carryArchivesAcrossSwap(backupDir, targetDir); err != nil {
			// The stash still holds what could not be carried - the other
			// backups among it. Deleting it would delete them.
			carried = false
			log.Printf("Restore %d: [warn] could not carry %s across the swap, keeping %s: %v", cmd.RunID, backupDirName, backupDir, err)
		}
	}
	if carried {
		go os.RemoveAll(backupDir)
	} else {
		keepStash(cmd.RunID, backupDir)
	}
	if cmd.SubServer == "" {
		// The stage was made in the storage root, outside any project, and is
		// now the server root: re-tag it or the server counts ~0 and its limit
		// caps nothing. A sub-server stage lives inside the root and inherits.
		retagAfterRestore(ctx, rdb, cmd.ServerUUID)
	}

	reportRestore(ctx, rdb, cmd.RestoreID, cmd.RunID, "success", "")
	log.Printf("Restore %d completed: %d files in %v", cmd.RunID, extracted, time.Since(started))
}

const errRestoreServerDeleted = "the server was deleted during the restore"

// retagAfterRestore is a var so a test can see a full restore reach it. The
// limit sits on the project id and did not move; it is lifted for the tag and
// put back.
var retagAfterRestore = func(ctx context.Context, rdb *redis.Client, uuid string) {
	if globalQuotaSet == nil {
		return
	}
	if err := countDiskUsage(ctx, rdb, globalQuotaSet, uuid, knownDiskLimitMB(ctx, rdb, globalQuotaSet, uuid), false); err != nil {
		log.Printf("quota: re-tag %s after restore: %v", uuid, err)
	}
}

// keepStash renames a stash that must survive out of the restore cleanup's
// reach: that cleanup removes every "<dir>.pre-restore-<time>" after a day,
// and a stash kept because it holds the other backups, or the only copy of the
// world, went with it.
//
// The kept name starts with a dot, so the sub-server listing, the server count
// and the disk-quota sweep - which all skip dot-directories - do not take it for
// a sub-server or a server of its own.
func keepStash(runID int, stash string) {
	if _, err := os.Lstat(stash); err != nil {
		return // nothing was stashed (no server dir before the restore)
	}
	dir, base := filepath.Split(stash)
	kept := filepath.Join(dir, ".restore-kept-"+strings.Replace(base, ".pre-restore-", "-", 1))
	if err := os.Rename(stash, kept); err != nil {
		log.Printf("Restore %d: [warn] could not move %s out of the cleanup's reach: %v", runID, stash, err)
		return
	}
	log.Printf("Restore %d: [warn] kept %s for an operator to look at", runID, kept)
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
//
// The node's own files in the server root go the same way: the resource
// limits, the active sub-server and the server's install record are the
// node's state, not the tenant's, and the copy in an archive is as old as the
// archive - a restore would otherwise bring back the RAM and CPU of before a
// downgrade, for the reconciler to recreate the container with.
func carryArchivesAcrossSwap(stashedRoot, restoredRoot string) error {
	// No server root before the restore (a recovery onto an empty disk): the
	// archive is the only copy of the node's state there is, so it stays.
	if _, err := os.Lstat(stashedRoot); err != nil {
		return nil
	}
	for _, name := range nodeOwnedRootEntries {
		// The archive's copy goes even when the live root has none to carry:
		// leaving it let a restore bring node state (limits, install record)
		// out of an archive the tenant may have written.
		target := filepath.Join(restoredRoot, name)
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		live := filepath.Join(stashedRoot, name)
		if _, err := os.Lstat(live); err != nil {
			continue // nothing to carry
		}
		if err := os.Rename(live, target); err != nil {
			return err
		}
	}
	return nil
}

// nodeOwnedRestoreName reports whether name, in a whole-server restore, is one
// of the node's own root files. A restore onto a missing root keeps the
// archive's copy (carryArchivesAcrossSwap), and handed to the MC uid - or
// linked to a file that is - the tenant's container could rewrite the config
// the reconciler recreates the container from. Created by the node, it stays
// the node's.
func nodeOwnedRestoreName(subServer, name string) bool {
	return subServer == "" && (slices.Contains(nodeOwnedRootEntries, name) || slices.Contains(archiveHiddenRootFiles, name))
}

// nodeOwnedRootEntries are the names in a server root the node writes and the
// tenant cannot (isProtectedFile).
var nodeOwnedRootEntries = []string{backupDirName, ".node_config.json", ".active_server", ".dylaris.json"}

// restoreStallTimeout ends a restore download that delivers no byte for this
// long. A variable for tests.
var restoreStallTimeout = 2 * time.Minute

// restoreStallMinBytes is the least a restore download must deliver per
// restoreStallTimeout (about 8.7 KB/s). A variable for tests.
var restoreStallMinBytes int64 = 1 << 20

// restoreDeadlineFloor, restoreDeadlineMinRate, restoreDeadlineUnknownSize and
// restoreDeadlineMax set restoreDeadline. The rate (2 Mbit/s) is far below any
// line a restore runs over; S3 and R2 always state a length, so an unknown one
// is an unusual source and gets a fixed day. The size is the Content-Length the
// source declares, and a tenant's own endpoint declares what it likes: without
// the ceiling, 1 TiB at the stall guard's minimum rate held a command slot for
// seven weeks. Variables for tests.
var (
	restoreDeadlineFloor             = 2 * time.Hour
	restoreDeadlineMinRate     int64 = 256 << 10
	restoreDeadlineUnknownSize       = 24 * time.Hour
	restoreDeadlineMax               = 24 * time.Hour
)

// restoreDeadline is how long a restore of an archive of size bytes (-1:
// unknown) may take in all.
func restoreDeadline(size int64) time.Duration {
	if size < 0 {
		return restoreDeadlineUnknownSize
	}
	// Compared in seconds before converting: size/rate times a second
	// overflows int64 for a declared length above about 2.4e15.
	if secs := size / restoreDeadlineMinRate; secs < int64((restoreDeadlineMax-restoreDeadlineFloor)/time.Second) {
		return restoreDeadlineFloor + time.Duration(secs)*time.Second
	}
	return restoreDeadlineMax
}

// stallReader re-arms the stall timer for d on every read that returned data,
// or, with min set, once min bytes arrived since it was last armed.
// It is NOT stopped at EOF: an HTTP/2 upload reads its last buffer to EOF before
// flow control lets it go out, so a peer that withholds the window from there
// would otherwise hold the request with no timer running. The owner stops it.
type stallReader struct {
	r   io.Reader
	t   *time.Timer
	d   time.Duration
	min int64
	got int64
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.got += int64(n)
	if n > 0 && s.got >= s.min {
		s.got = 0
		s.t.Reset(s.d)
	}
	return n, err
}

// maxRestoreEntries and restoreEntryCost bound what an archive's entries cost
// beyond their bytes. A large modded server holds a few hundred thousand files.
var maxRestoreEntries = 2_000_000

const restoreEntryCost = 4096

// errRestoreDiskBudget ends an extraction that would fill the disk.
var errRestoreDiskBudget = errors.New("the archive is larger than the free space this node can give it")

// restoreDiskBudget is how many bytes an extraction into dir may write: the
// free space, less a reserve of a twentieth of the disk. A variable for tests.
var restoreDiskBudget = func(dir string) int64 {
	total, free := getDiskSpace(dir)
	if total == 0 {
		return math.MaxInt64 - 1 // unknown: no bound rather than a refusal
	}
	if reserve := total / 20; free > reserve {
		return int64(free - reserve)
	}
	return 0
}

// containerRunning reports whether the server's container is running now.
func containerRunning(dm *DockerManager, uuid string) bool {
	info, err := dm.cli.ContainerInspect(dm.ctx, "mc_"+uuid)
	return err == nil && info.State != nil && info.State.Running
}

// downloadBackup opens the archive on a filesystem provider. The caller is
// responsible for closing. For the node-local mode the source is on the same
// disk we'll extract into, so this is just a plain file open. Object storage
// never reaches here: its archive is fetched from a URL Core signs
// (openPresignedRestore).
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
		// The same check list and download take: an archive is a regular file
		// the node wrote, never a link to be followed.
		if _, err := archiveInfo(full); err != nil {
			return nil, err
		}
		return os.Open(full)

	default:
		return nil, fmt.Errorf("unknown provider %s", info.Provider)
	}
}

// downloadPresigned streams the archive from a pre-signed GET URL. Caller closes
// the body.
func downloadPresigned(ctx context.Context, client *http.Client, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, withoutURL(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("presigned get: %w", withoutURL(err))
	}
	if resp.StatusCode/100 != 2 {
		err := storageStatusError("presigned get", resp)
		resp.Body.Close()
		return nil, err
	}
	return sizedBody{resp.Body, resp.ContentLength}, nil
}

// sizedBody carries the length a response declared (-1 when it declared none)
// to restoreDeadline.
type sizedBody struct {
	io.ReadCloser
	size int64
}

// archiveSize is the length of a restore source where it states one, else -1.
func archiveSize(body io.Reader) int64 {
	switch b := body.(type) {
	case sizedBody:
		return b.size
	case *os.File:
		if fi, err := b.Stat(); err == nil {
			return fi.Size()
		}
	}
	return -1
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
