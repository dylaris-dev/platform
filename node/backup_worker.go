package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"dylaris-pkg/queue"
)

// BackupRunCommand is the payload the Core scheduler / handler pushes onto
// the node's Redis queue to start a single backup-run.
type BackupRunCommand struct {
	RunID           int             `json:"runId"`
	JobID           int             `json:"jobId"`
	ServerUUID      string          `json:"serverUuid"`
	SubServer       string          `json:"subServer"`
	IncludePatterns []string        `json:"includePatterns"`
	ExcludePatterns []string        `json:"excludePatterns"`
	StorageKey      string          `json:"storageKey"`
	Storage         json.RawMessage `json:"storage"`
	// Upload "multipart" means the storage is object storage and Core drives the
	// upload: Storage carries no credentials and there is no URL, the node asks
	// Core for part URLs as it goes. See backup_transfer.go. Required for every
	// provider this node does not handle itself (requireCoreTransfer).
	Upload string `json:"upload"`
	// GuardedTransfer marks a tenant's own bucket: its URLs may name any host,
	// so they are fetched through the client that refuses non-public addresses.
	GuardedTransfer bool `json:"guardedTransfer"`
	// AllowPrivate lists the private networks (CIDRs) the operator allowed
	// for users' own storage (STORAGE_PRIVATE_ALLOW on Core). It only widens a
	// guarded transfer; loopback and link-local stay refused regardless.
	AllowPrivate []string `json:"allowPrivate"`
	// Manifest is Core's description of what this archive contains: loader,
	// versions, installer origin, the installed-mod rows. It is written into the
	// archive VERBATIM and never parsed here.
	//
	// Opaque on purpose. The node writing bytes it does not understand is what
	// keeps the schema in one place: Core can add a field to the manifest
	// without a node release, and a node can never disagree with Core about what
	// a manifest means. Empty (an older Core) simply produces an archive with no
	// manifest, which every reader must already handle - archives written before
	// this existed have none either.
	Manifest json.RawMessage `json:"manifest"`
	// UploadCapMbit is the storage's own cap on what this run sends it, players
	// or not. 0 (an older Core, or no cap) is none.
	UploadCapMbit int64 `json:"uploadCapMbit"`
}

// manifestEntryName is where the archive description lives inside the archive.
//
// Under a reserved directory rather than at the root so it cannot collide with
// a file a server legitimately has, and so ONE prefix covers whatever else the
// format grows later. It is written FIRST, so a reader can stream-parse it
// without unpacking a multi-gigabyte world to reach it.
const manifestEntryName = ".dylaris/backup.json"

// manifestDirName is the reserved prefix manifestEntryName lives under. It is
// skipped by the archive walk and by extraction, for the same reason
// .dylaris-backups is: an entry the platform writes into the tree would
// otherwise be archived into the NEXT backup and restored into a live server
// directory.
const manifestDirName = ".dylaris"

// isManifestEntry reports whether a tar entry name addresses the reserved
// manifest directory. It cleans the name first, because a tar header is written
// by whoever produced the archive and "./.dylaris/backup.json" addresses the
// same place as ".dylaris/backup.json".
func isManifestEntry(name string) bool {
	rel := strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(name, "\\", "/")), "/")
	return rel == manifestDirName || strings.HasPrefix(rel, manifestDirName+"/")
}

// writeManifestEntry writes the manifest as the first entry of the tar.
func writeManifestEntry(tw *tar.Writer, manifest []byte) error {
	// "null" is what a JSON encoder produces for an absent RawMessage, and this
	// side never parses the value - so without this the archive would carry a
	// description file whose whole content is the word null.
	if len(manifest) == 0 || string(bytes.TrimSpace(manifest)) == "null" {
		return nil
	}
	hdr := &tar.Header{
		Name:     manifestEntryName,
		Mode:     0o644,
		Size:     int64(len(manifest)),
		Typeflag: tar.TypeReg,
		ModTime:  time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(manifest)
	return err
}

type storageInfo struct {
	ID       int             `json:"id"`
	Name     string          `json:"name"`
	Provider string          `json:"provider"`
	Config   json.RawMessage `json:"config"`
}

type localCfg struct {
	BasePath string `json:"basePath"`
}

// isBackupStoreEntry reports whether a walk-relative path is the archive store
// or something inside it. Such an entry never belongs in an archive.
//
// A whole-server job (sub_server NULL, which Core's validSubServer accepts as
// "the whole container") walks the server ROOT, and .dylaris-backups sits
// inside it: without this, every run archives every earlier run, so backup N
// carries backups 1..N-1 on top of the world. Worse, the node-local upload
// target is a file inside the very tree being walked, so a run can stream its
// own half-written archive into itself.
func isBackupStoreEntry(rel string) bool {
	return rel == backupDirName || strings.HasPrefix(rel, backupDirName+"/") ||
		rel == manifestDirName || strings.HasPrefix(rel, manifestDirName+"/")
}

// archiveSourceBytes estimates what an archive of dir holds for the spool
// decision: the tree less the backup and manifest stores the walk skips. Those
// hold every node-local archive of the server, so a server with a few of them
// measured several times its world and was refused the spool.
// ponytail: exclude patterns still count; over-reserving errs safe, mirror the
// walk's filter here if they ever push eligible servers off the spool.
func archiveSourceBytes(dir string) int64 {
	n := dirSize(dir) - dirSize(filepath.Join(dir, backupDirName)) - dirSize(filepath.Join(dir, manifestDirName))
	return max(n, 0)
}

// archiveHiddenRootFiles are the node-owned files in a server root that no
// archive carries. .active_server is not among them: the active sub-server is
// no secret, and a restore onto an empty disk still learns it from the archive.
var archiveHiddenRootFiles = []string{".node_config.json", ".dylaris.json", netGateFile}

// RunBackup builds the archive and streams it directly to storage via an
// io.Pipe — no buffering in RAM. The tar+gzip writer runs in a goroutine
// that pushes bytes into the pipe; the storage uploader reads from the
// other end and pushes them out. For multi-GB worlds this keeps the
// node's working set under a few megabytes regardless of archive size.
// copyAndCommit writes the archive and reports a failure that only shows on
// sync or close: on a network filesystem a full disk or a quota is typically
// reported there, and with the close error dropped a truncated archive was
// recorded as a good backup - which retention then let push out older, intact
// ones.
func copyAndCommit(f *os.File, r io.Reader) error {
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("flush the archive to disk: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close the archive: %w", err)
	}
	return nil
}

// saveFlushTimeout bounds the wait for the server to confirm `save-all flush`.
//
// It used to be a fixed five-second wait, on the grounds that the console was
// not readable from here. It is: the log-shipper streams it into Redis. A large
// modded world takes far longer than five seconds to flush, and a tar that
// starts reading region files mid-write produces an archive that is damaged
// without anyone noticing until a restore - by which time retention has
// removed the older, good ones. So the archive waits for "Saved the game".
// Past the bound it goes ahead as before and says so in the log.
var saveFlushTimeout = 3 * time.Minute

// saveConfirmed reports whether a console line confirms a finished save:
// "Saved the game" from 1.13 on, "Saved the world" before it (Forge 1.12.2,
// 1.7.10), and the dimension summary some loaders print last.
func saveConfirmed(line string) bool {
	msg := afterLogPrefix(line)
	for _, p := range []string{"Saved the game", "Saved the world", "All dimensions are saved", "ThreadedAnvilChunkStorage: All dimensions are saved"} {
		if strings.HasPrefix(msg, p) {
			return true
		}
	}
	return false
}

// afterLogPrefix is a console line without its log prefix. The message is
// matched from its start: a player's chat ("<Steve> Saved the game?") carries
// the same phrases, and a wait ended by one archived a world mid-flush. The
// prefix ends at the FIRST "]: " (vanilla, Paper, Forge), else at the first
// "] " (BungeeCord); nothing a player types can move either one earlier.
// Unlike consoleMessage it keeps a "[Steve] " tag, which is how /say prints.
func afterLogPrefix(line string) string {
	msg := strings.TrimSpace(ansiEscape.ReplaceAllString(line, ""))
	if i := strings.Index(msg, "]: "); i >= 0 {
		return msg[i+3:]
	}
	if i := strings.Index(msg, "] "); i >= 0 {
		return msg[i+2:]
	}
	return msg
}

// saveUnsupported reports a console that answered the save commands with "I do
// not know that command" (a proxy, or a custom server): no confirmation is
// coming, so there is nothing to wait for.
func saveUnsupported(line string) bool {
	msg := afterLogPrefix(line)
	if l := strings.ToLower(msg); strings.HasPrefix(l, "unknown command") || strings.HasPrefix(l, "unknown or incomplete command") {
		return true
	}
	// The proxies answer in words of their own, matched as the whole message:
	// a backup that stops waiting early on a game server archives a world
	// mid-save, so nothing looser than their exact reply.
	return slices.Contains(proxyUnknownCommandReplies, msg)
}

// proxyUnknownCommandReplies are the proxies' answers to a command they do not
// have: Velocity's "This command does not exist." (its messages.properties),
// BungeeCord's and Waterfall's "Command not found" (BungeeCordLauncher). Their
// log prefixes differ ("[12:00:00 INFO]: " and "12:00:00 [INFO] "), so the
// reply is matched after either.
var proxyUnknownCommandReplies = []string{"This command does not exist.", "Command not found"}

// ansiEscape is a terminal colour sequence. BungeeCord sends its reply in red,
// and the console stream carries whatever the process wrote.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// waitForSaveConfirmation reads the server's console stream from after
// fromID until a save is confirmed, the timeout passes, or ctx ends.
func waitForSaveConfirmation(ctx context.Context, rdb *redis.Client, key, fromID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	last := fromID
	for time.Now().Before(deadline) {
		block := time.Until(deadline)
		if block > 5*time.Second {
			block = 5 * time.Second
		}
		res, err := rdb.XRead(ctx, &redis.XReadArgs{Streams: []string{key, last}, Count: 200, Block: block}).Result()
		if ctx.Err() != nil {
			return false
		}
		if err != nil && err != redis.Nil {
			time.Sleep(time.Second)
			continue
		}
		for _, st := range res {
			for _, m := range st.Messages {
				last = m.ID
				line, _ := m.Values["line"].(string)
				if saveConfirmed(line) {
					return true
				}
				if saveUnsupported(line) {
					return false
				}
			}
		}
	}
	return false
}

// worldSaveGuard pauses a running server's world saving for the duration of an
// archive, and turns it back on afterwards.
//
// Without it the archive holds whatever the JVM happened to have flushed. A
// player's position and inventory live in memory until an autosave, which is
// why restoring a backup taken while someone was online put them back where
// they last saved rather than where they were. The less visible half is worse:
// a tar over a live world can capture a region file mid-write, and that archive
// fails only at restore time.
//
// The cost, accepted deliberately: while saving is off the server keeps running
// and keeps not persisting. resume() is deferred, so every ordinary return and
// every panic re-enables it. A SIGKILL of the node process is the case that is
// not covered - saving then stays off until that server restarts.
type worldSaveGuard struct {
	uuid string
	// send is nil when saving was never paused, which is both the "server was
	// not running" case and the guard's own record that there is nothing to undo.
	send func(command string) error
}

// pauseWorldSaves stops the server writing its world, flushes what it holds,
// and returns the guard that puts it back. subServer is the one being archived,
// "" for the whole server.
func pauseWorldSaves(ctx context.Context, rdb *redis.Client, dm *DockerManager, uuid, subServer string) *worldSaveGuard {
	g := &worldSaveGuard{uuid: uuid}
	if rdb == nil || dm == nil {
		return g
	}
	// A stopped server has everything on disk already, and its console queue is
	// drained by the log-shipper INSIDE the container - so a command pushed now
	// would not be read now, it would be read by the next start. Turning saving
	// off on a server that is only just booting is the one outcome worse than an
	// inconsistent backup.
	info, err := dm.cli.ContainerInspect(ctx, "mc_"+uuid)
	if err != nil || info.State == nil || !info.State.Running {
		return g
	}
	sub := ""
	if info.Config != nil {
		if s := strings.TrimPrefix(info.Config.WorkingDir, "/data/"); s != info.Config.WorkingDir {
			sub = s
		}
	}
	// The container runs one sub-server. A backup of another one has all its
	// files on disk already, and save-off would land on the running one, which
	// the archive never touches.
	if subServer != "" && sub != "" && sub != subServer {
		return g
	}
	inputKey := fmt.Sprintf("dylaris:server:%s:input", uuid)
	g.send = func(command string) error { return rdb.RPush(context.Background(), inputKey, command).Err() }
	logKey := serverLogStreamKey(uuid, sub)
	// From the newest line now, so an earlier save's confirmation is not read
	// as this one's.
	// An empty stream reads from its start; an unreadable one from "now" ($),
	// so an old confirmation is never taken for this one.
	from := "$"
	if last, err := rdb.XRevRangeN(ctx, logKey, "+", "-", 1).Result(); err == nil {
		from = "0"
		if len(last) > 0 {
			from = last[0].ID
		}
	}

	g.send("save-off")
	g.send("save-all flush")
	if waitForSaveConfirmation(ctx, rdb, logKey, from, saveFlushTimeout) {
		log.Printf("backup: server %s: saving paused, the server confirmed the flush", uuid)
	} else {
		log.Printf("backup: server %s: saving paused, but the server did not confirm the flush within %s; archiving anyway", uuid, saveFlushTimeout)
	}
	return g
}

// pauseSaves is pauseWorldSaves, a variable so a test can stand in for a
// running server.
var pauseSaves = pauseWorldSaves

// save-on is retried for about half a minute: a Redis failover outlasts
// go-redis's own retries, and a server left on save-off loses everything since
// the backup on its next crash.
var (
	saveOnAttempts   = 5
	saveOnRetryDelay = 2 * time.Second
)

// resume re-enables saving, once, however many times it is called.
//
// The command goes out on a background context on purpose: the caller's may
// already be cancelled, and a cancelled backup is exactly the moment when
// leaving a server unable to save would be worst.
func (g *worldSaveGuard) resume() {
	if g == nil || g.send == nil {
		return
	}
	send := g.send
	g.send = nil
	delay := saveOnRetryDelay
	var err error
	for i := 0; i < saveOnAttempts; i++ {
		if i > 0 {
			time.Sleep(delay)
			delay *= 2
		}
		if err = send("save-on"); err == nil {
			log.Printf("backup: server %s: saving resumed", g.uuid)
			return
		}
	}
	log.Printf("backup: server %s: ERROR: saving is still OFF, save-on could not be sent: %v; run save-on in the console or restart the server", g.uuid, err)
}

func RunBackup(ctx context.Context, rdb *redis.Client, sm *StorageManager, dm *DockerManager, cmd BackupRunCommand) {
	// At-least-once delivery: a redelivery while this run is still going would
	// archive the same tree twice into the same key. See backup_inflight.go.
	key := fmt.Sprintf("%d", cmd.RunID)
	if !backupsInFlight.enter(key) {
		log.Printf("backup_run: run %s is already running on this node, ignoring the redelivery", key)
		return
	}
	defer backupsInFlight.leave(key)
	// A different run of the same server: the first to finish would switch
	// world saving back on under this one's archive. Core starts one per
	// server; this is a node that queued several while it was away.
	serverKey := "server:" + cmd.ServerUUID
	// Not while a restore swaps the server's directory either: the archive
	// would hold a half-swapped tree. The restore refuses the other way round.
	if reason := enterBackupOfServer(cmd.ServerUUID); reason != "" {
		log.Printf("backup_run: run %s refused for server %s: %s", key, cmd.ServerUUID, reason)
		reportBackup(ctx, rdb, cmd.RunID, "failed", reason, 0)
		return
	}
	defer backupsInFlight.leave(serverKey)

	started := time.Now()
	storage := storageInfo{}
	if err := json.Unmarshal(cmd.Storage, &storage); err != nil {
		reportBackup(ctx, rdb, cmd.RunID, "failed", "invalid storage payload: "+err.Error(), 0)
		return
	}
	if err := requireCoreTransfer(storage.Provider, cmd.Upload == modeUploadMultipart); err != nil {
		reportBackup(ctx, rdb, cmd.RunID, "failed", err.Error(), 0)
		return
	}
	// The same run once more after a node restart: killed after its success
	// report and before the queue's ACK, it comes back with its own key. The
	// archive there is the one the success row points at; writing it again
	// truncated it first, and a rerun that then failed deleted it.
	if cmd.Upload != modeUploadMultipart {
		if size, ok := completeArchive(sm, cmd.ServerUUID, storage, cmd.StorageKey); ok {
			log.Printf("backup_run: run %s already has its complete archive, reporting it instead of writing it again", key)
			reportBackup(ctx, rdb, cmd.RunID, "success", "", size)
			return
		}
	}

	serverRoot := resolveServerRoot(sm, cmd.ServerUUID)
	rootDir := serverRoot
	if cmd.SubServer != "" {
		rootDir = filepath.Join(serverRoot, cmd.SubServer)
		// Core validates the name, but this is where it becomes a path: Join
		// cleans "..", it does not confine, so an unconfined name here would
		// archive whatever the node process can read - other tenants' servers
		// and the cached .node_secret included.
		if !withinRoot(serverRoot, rootDir) {
			reportBackup(ctx, rdb, cmd.RunID, "failed", "sub-server escapes the server directory", 0)
			return
		}
	}
	if _, err := os.Stat(rootDir); err != nil {
		reportBackup(ctx, rdb, cmd.RunID, "failed", "source directory not found: "+rootDir, 0)
		return
	}

	// Spool or not is decided before saving pauses: measuring the server's
	// size is a walk of its own and must not lengthen the window.
	pacer := newBackupPacer(cmd.UploadCapMbit)
	defer pacer.done()
	spoolPath, spoolMax := "", int64(0)
	releaseSpool := func(int64) {}
	// Deferred before the spool file's removal, so it runs after it.
	defer func() { releaseSpool(0) }()
	if cmd.Upload == modeUploadMultipart {
		if p := backupPolicyNow.Load(); p != nil && p.SpoolEnabled && globalSpoolDir != "" {
			_, free := getDiskSpace(globalSpoolDir)
			var why string
			spoolMax, releaseSpool, why = reserveSpool(p, archiveSourceBytes(rootDir), free)
			if spoolMax > 0 {
				spoolPath = filepath.Join(globalSpoolDir, spoolFileName(cmd.RunID))
			} else {
				log.Printf("backup %d: uploading directly, not spooling: %s", cmd.RunID, why)
			}
		}
	}

	// Everything that could refuse this run has now refused it, so this is the
	// last point at which pausing saves would be wasted. From here the guard
	// covers every exit, including the panic paths.
	saves := pauseSaves(ctx, rdb, dm, cmd.ServerUUID, cmd.SubServer)
	defer saves.resume()
	if saves.send != nil {
		pacer.savesPaused(time.Now())
	} else {
		// A stopped server has no save-off window to shorten.
		spoolPath = ""
		releaseSpool(0)
	}
	logBackupPace(cmd.RunID, pacer)

	// For node-local storage the destination is on the same disk we're
	// reading from — no pipe/uploader is needed. We still want to keep
	// the streaming tar+gzip path so RAM stays bounded for large worlds,
	// so we use the same io.Pipe + uploader plumbing and let
	// uploadBackup() switch on storage.Provider to do the right thing
	// (io.Copy to a hidden .dylaris-backups/<id>.tar.gz on the Node).
	// counting writer wraps the archive's output so we can report the
	// archived size without waiting for the upload to complete.
	counter := &countingWriter{}

	stopProgress := reportBackupProgress(ctx, rdb, cmd.RunID, counter)
	defer stopProgress()

	addedAny := false
	paceRead := func(r io.Reader) io.Reader { return pacer.reader(ctx, r) }

	// src is what the upload reads; abortSrc unblocks its writer on failure.
	var src io.Reader
	abortSrc := func(error) {}
	if spoolPath != "" {
		f, added, err := spoolArchive(spoolPath, spoolMax, counter, func(w io.Writer) (bool, error) {
			return writeServerArchivePaced(w, serverRoot, rootDir, cmd.IncludePatterns, cmd.ExcludePatterns, cmd.Manifest, paceRead)
		})
		if f != nil {
			path := spoolPath
			defer func() {
				f.Close()
				os.Remove(path)
			}()
		}
		switch {
		case errors.Is(err, errSpoolFull):
			// Larger than its estimate (links inside the server are archived as
			// their targets): the direct upload below still takes it.
			f.Close()
			os.Remove(spoolPath)
			releaseSpool(0)
			counter.n.Store(0)
			log.Printf("backup %d: the archive outgrew its spool reservation, uploading directly", cmd.RunID)
		case err != nil:
			reportBackup(ctx, rdb, cmd.RunID, "failed", "spool the archive: "+err.Error(), 0)
			return
		default:
			// The file's bytes now count in the disk's free space; holding the
			// reservation through the paced upload counted them twice.
			releaseSpool(counter.Total())
			addedAny = added
			// The archive is complete on disk: the world may save again while the
			// upload takes as long as the players need it to.
			saves.resume()
			pacer.savesResumed()
			log.Printf("backup %d: archive spooled (%.2f MB), saving resumed, uploading", cmd.RunID, float64(counter.Total())/1024/1024)
			src = f
		}
	}
	if src == nil {
		pr, pw := io.Pipe()
		mw := io.MultiWriter(pw, counter)
		go func() {
			// The goroutine is the source side of the pipe. Any error we hit
			// has to propagate to the reader by closing the pipe with that
			// error so the uploader sees it and aborts cleanly.
			added, err := writeServerArchivePaced(mw, serverRoot, rootDir, cmd.IncludePatterns, cmd.ExcludePatterns, cmd.Manifest, paceRead)
			addedAny = added
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			pw.Close()
		}()
		src = pr
		abortSrc = func(err error) { pr.CloseWithError(err) }
	}

	// Upload reads from the pipe. Returns once EOF (or pipe error) reached.
	// Object storage goes through Core's multipart upload; the filesystem
	// providers are written by this node directly.
	var upErr error
	multipartSize := int64(-1)
	switch {
	case cmd.Upload == modeUploadMultipart:
		// Checked before Complete, which is the point: an archive that matched
		// nothing is never completed, so there is no object to clean up and
		// Core aborts the parts when the run is reported failed.
		multipartSize, upErr = uploadMultipartPaced(ctx, coreMultipartAPI(cmd.RunID), transferClient(cmd.GuardedTransfer, cmd.AllowPrivate), src, func() error {
			if !addedAny {
				return errNothingArchived
			}
			return nil
		}, pacer)
	default:
		upErr = uploadBackup(ctx, sm, cmd.ServerUUID, storage, cmd.StorageKey, pacer.capReader(ctx, src))
	}
	if errors.Is(upErr, errNothingArchived) {
		reportBackup(ctx, rdb, cmd.RunID, "failed", upErr.Error(), 0)
		return
	}
	if upErr != nil {
		// Drain any remaining bytes so the writer goroutine doesn't block on
		// a full pipe; CloseWithError unblocks the writer immediately.
		abortSrc(upErr)
		// Remove what was half-written, which for the file-backed providers is
		// a real archive-shaped file: local, shared and node-local all io.Copy
		// straight into the destination, so an upload that dies partway leaves
		// its bytes there and returns the error.
		//
		// Nothing else would ever remove it. Retention prunes SUCCESSFUL runs,
		// and reapAbandonedRuns deliberately deletes nothing (it cannot tell a
		// complete archive from a partial one). Meanwhile the node reports
		// node-local usage by summing every regular file in .dylaris-backups/,
		// so the debris counts against backup.quota_per_server_gb and shows on
		// the Overview usage bar while appearing in no backup list.
		//
		// The trigger that matters makes that circular: a disk filling up
		// produces the largest leftover, which pushes the server over its quota,
		// which is what refuses the NEXT run. The failure would keep itself
		// alive.
		//
		// Safe to call unconditionally - the key belongs to this run alone, so
		// there is no other archive it could remove, and deleteBackup is
		// best-effort on a key that was never written.
		//
		// Not for a multipart upload: this node has nothing to delete there, and
		// Core aborts the parts when it reads the failed report.
		if cmd.Upload != modeUploadMultipart {
			deleteBackup(ctx, sm, cmd.ServerUUID, storage, cmd.StorageKey)
		}
		reportBackup(ctx, rdb, cmd.RunID, "failed", "upload failed: "+upErr.Error(), 0)
		return
	}

	size := counter.Total()
	if multipartSize >= 0 {
		// The completed object's size as Core read it, which is the number the
		// quota and the run list should carry.
		size = multipartSize
	}
	if !addedAny {
		// We still uploaded a 0-file archive; clean up storage and surface
		// the friendlier error so the UI doesn't show a zero-byte success.
		deleteBackup(ctx, sm, cmd.ServerUUID, storage, cmd.StorageKey)
		reportBackup(ctx, rdb, cmd.RunID, "failed", "no files matched include/exclude patterns", 0)
		return
	}

	reportBackup(ctx, rdb, cmd.RunID, "success", "", size)
	log.Printf("Backup %d streamed to %s/%s — %.2f MB in %v", cmd.RunID, storage.Provider, cmd.StorageKey, float64(size)/1024/1024, time.Since(started))
}

// writeServerArchive tar+gzips the tree at rootDir into w and reports whether
// anything was written. serverRoot is the tenant boundary a symlink may not
// point outside of; it is the SERVER root, not rootDir, so a link from one
// sub-server to another keeps working.
//
// Split out of RunBackup so the walk can be driven without Redis, a storage
// manager or a live provider.
func writeServerArchive(w io.Writer, serverRoot, rootDir string, include, exclude []string, manifest []byte) (bool, error) {
	return writeServerArchivePaced(w, serverRoot, rootDir, include, exclude, manifest, nil)
}

// writeServerArchivePaced is writeServerArchive with every file's bytes read
// through pace, which paces the disk read and gzip together. nil is unpaced.
func writeServerArchivePaced(w io.Writer, serverRoot, rootDir string, include, exclude []string, manifest []byte, pace func(io.Reader) io.Reader) (bool, error) {
	if pace == nil {
		pace = func(r io.Reader) io.Reader { return r }
	}
	root, err := os.OpenRoot(serverRoot)
	if err != nil {
		return false, err
	}
	defer root.Close()
	startName, err := rootName(serverRoot, rootDir)
	if err != nil {
		return false, err
	}
	gw := gzip.NewWriter(w)
	tw := tar.NewWriter(gw)
	addedAny := false

	// Before the walk, so it is the first entry no matter what the walk finds.
	// It deliberately does NOT set addedAny: a run whose include/exclude
	// patterns match no file must still fail as "nothing matched" rather than
	// upload an archive containing only its own description.
	if err := writeManifestEntry(tw, manifest); err != nil {
		return false, err
	}

	// walkRoot judges links through the Root: one that leaves the server, dangles
	// or names a directory is skipped, one that stays inside is archived as its
	// target. Archiving a link as a header-only entry and then copying its
	// target's bytes into it used to abort every backup of that server.
	// The first archived name of each file, by identity. A file reached again -
	// a hard link, or a symlink inside the server onto it - is written as a tar
	// link to that name instead of a second copy. Without it a tenant's one
	// gigabyte linked five thousand times, which costs them no quota, became
	// five terabytes of upload, and a restore of it filled the node's disk.
	seen := map[fileIdentity]string{}
	// The archive store is skipped by name, but a symlink elsewhere in the tree
	// onto one of its archives is archived as its target: a tenant linking every
	// earlier archive made each run carry all of them, doubling per run. So
	// the store's files are known by identity and never archived under any name.
	// Links INSIDE the store are not followed: one there onto level.dat would
	// otherwise take the world out of every backup.
	stored := map[fileIdentity]bool{}
	_ = fs.WalkDir(root.FS(), backupDirName, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.Mode().IsRegular() {
			if id, ok := identityOf(info); ok {
				stored[id] = true
			}
		}
		return nil
	})
	// The node's own files in the server root are hidden from the tenant (mode
	// 0600, isProtectedFile): the start command and JVM flags among them are
	// redacted for members without server.settings.write, and the archive was
	// the one way to read them. Kept out by name at the root and by identity
	// under any other name. A restore keeps the live copies (and drops what an
	// older archive carried), so the archive never needed them.
	for _, n := range archiveHiddenRootFiles {
		if info, err := root.Lstat(n); err == nil && info.Mode().IsRegular() {
			if id, ok := identityOf(info); ok {
				stored[id] = true
			}
		}
	}
	walkErr := walkRoot(root, startName, func(name string, info fs.FileInfo) error {
		rel := relTo(startName, name)
		if rel == "." {
			return nil
		}
		if startName == "." && slices.Contains(archiveHiddenRootFiles, rel) {
			return nil
		}
		if isBackupStoreEntry(rel) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if matchAny(rel, exclude) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if len(include) > 0 && !matchAny(rel, include) {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		id, hasID := identityOf(info)
		hasID = hasID && !info.IsDir()
		if hasID {
			if stored[id] {
				return nil
			}
			if first, dup := seen[id]; dup {
				hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeLink, first, 0
				if err := tw.WriteHeader(hdr); err != nil {
					return err
				}
				addedAny = true
				return nil
			}
		}
		// Opened before its header is written: that write can block on the
		// upload for minutes, and a plugin's short-lived file (a journal, a
		// tile written then renamed) removed in that window failed the whole
		// backup. Gone before the open, it is left out like any file removed
		// before the walk reached it; once open, its removal no longer matters.
		var f *os.File
		if !info.IsDir() {
			var err error
			f, err = openRegularIn(root, name)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			defer f.Close()
		}
		if hasID {
			seen[id] = rel
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if f != nil {
			if err := copyExactly(tw, pace(f), hdr.Size); err != nil {
				return err
			}
		}
		addedAny = true
		return nil
	})

	// Order matters: close tar (flush remaining records) then gzip (flush
	// trailer) so the reader sees a complete stream.
	tarErr := tw.Close()
	gzErr := gw.Close()

	switch {
	case walkErr != nil:
		return addedAny, walkErr
	case tarErr != nil:
		return addedAny, tarErr
	case gzErr != nil:
		return addedAny, gzErr
	}
	return addedAny, nil
}

// countingWriter sums the byte count of an in-flight stream without
// buffering. Used so we can report the compressed archive size even though
// we never hold the full archive in memory.
type countingWriter struct{ n atomic.Int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return len(p), nil
}

func (c *countingWriter) Total() int64 { return c.n.Load() }

// uploadBackup streams the body to a filesystem provider. For local/shared
// that's a simple io.Copy to disk. For node-local the archive lands inside the
// server's own .dylaris-backups/ folder on this Node's disk - the storage key's
// tail filename is taken as the archive name and the directory is created on
// demand. Object storage never reaches here: it is uploaded through Core
// (uploadMultipart), and this node holds no credentials for it.
func uploadBackup(ctx context.Context, sm *StorageManager, serverUUID string, info storageInfo, key string, r io.Reader) error {
	switch info.Provider {
	case "local", "shared":
		// "shared" is the new UI label for the legacy "local" provider —
		// same Core-side filesystem path, just relabeled in the panel.
		var cfg localCfg
		if err := json.Unmarshal(info.Config, &cfg); err != nil {
			return fmt.Errorf("invalid local cfg: %w", err)
		}
		if cfg.BasePath == "" {
			return fmt.Errorf("local storage requires basePath")
		}
		full := filepath.Join(cfg.BasePath, filepath.Clean("/"+key))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		return copyAndCommit(f, r)

	case "node-local":
		// Resolve <server-dir>/.dylaris-backups/<archive>. The Core-side
		// storage key includes a "backups/<serverUUID>/job-<jobID>/..."
		// prefix for display, but on-disk we collapse it to the leaf
		// filename — the directory is server-scoped already so the prefix
		// adds no information.
		dir := filepath.Join(resolveServerRoot(sm, serverUUID), backupDirName)
		// Not MkdirAll: the server may have been deleted while the backup
		// waited for its save flush, and MkdirAll brought its directory back
		// for good. A missing server root fails the run instead.
		root, err := openBackupRoot(dir, true)
		if err != nil {
			return fmt.Errorf("create node-local backup dir: %w", err)
		}
		defer root.Close()
		full := nodeLocalArchiveName(key)
		// The write side of the same problem openArchive describes: this
		// directory is inside the tenant's own bind mount, so O_CREATE|O_TRUNC
		// would follow a symlink planted under the name the next run is about to
		// use and write the archive through it. Remove first (that unlinks a
		// link, it does not follow one) and then insist on creating the file
		// ourselves - O_EXCL fails on anything that reappeared in between, so
		// the race ends in a failed backup rather than a write somewhere else.
		if rerr := root.Remove(full); rerr != nil && !os.IsNotExist(rerr) {
			return fmt.Errorf("clear previous archive: %w", rerr)
		}
		// Written under a temporary name and renamed when complete. Written
		// in place, a run killed by a node update left a truncated archive
		// under the real name, which counted against the backup quota and no
		// list ever showed. The boot sweep removes an old .partial.
		partial := full + partialBackupSuffix
		if rerr := root.Remove(partial); rerr != nil && !os.IsNotExist(rerr) {
			return fmt.Errorf("clear previous partial archive: %w", rerr)
		}
		f, err := root.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		// Bounded by the disk, shared with every other write running here: an
		// archive inside the server directory used to be written with no check
		// at all, and a full disk stops every server on the node. Not by the
		// server's own disk limit - a backup of a server near it would never fit.
		budget := nodeWriteBudget(dir)
		// A failed run removes its partial: booked as landed, those bytes kept
		// counting against every other write running on the node.
		landed := false
		defer func() {
			if landed {
				budget.release()
			} else {
				budget.releaseWritten(0)
			}
		}()
		if err := copyAndCommit(f, budget.reader(r)); err != nil {
			root.Remove(partial)
			return err
		}
		// Rename replaces the directory entry; it does not follow a link
		// planted under the final name meanwhile.
		if err := root.Rename(partial, full); err != nil {
			root.Remove(partial)
			return fmt.Errorf("finish archive: %w", err)
		}
		landed = true
		return nil

	default:
		return fmt.Errorf("unknown provider %s", info.Provider)
	}
}

// completeArchive reports the size of a complete archive already under key on
// a filesystem provider. The key carries a random suffix per run, so a file
// there is this run's; it counts only when its gzip stream reads to the end
// with a good checksum, which a write cut short (or an older node's in-place
// write) never has.
func completeArchive(sm *StorageManager, serverUUID string, info storageInfo, key string) (int64, bool) {
	var f *os.File
	var err error
	switch info.Provider {
	case "local", "shared":
		var cfg localCfg
		if json.Unmarshal(info.Config, &cfg) != nil || cfg.BasePath == "" {
			return 0, false
		}
		f, err = os.Open(filepath.Join(cfg.BasePath, filepath.Clean("/"+key)))
	case "node-local":
		// Inside the tenant's bind mount: openArchive refuses a planted link.
		f, err = openArchive(filepath.Join(resolveServerRoot(sm, serverUUID), backupDirName, nodeLocalArchiveName(key)))
	default:
		return 0, false
	}
	if err != nil {
		return 0, false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return 0, false
	}
	gr, err := gzip.NewReader(f)
	if err != nil {
		return 0, false
	}
	if _, err := io.Copy(io.Discard, gr); err != nil {
		return 0, false
	}
	return st.Size(), true
}

// nodeLocalArchiveName collapses a storage key into its leaf filename so
// archives land directly under .dylaris-backups/ regardless of how Core
// chose to encode the key (job-N/, server-uuid/, etc.).
func nodeLocalArchiveName(key string) string {
	parts := strings.Split(filepath.ToSlash(key), "/")
	return parts[len(parts)-1]
}

// deleteBackup removes an archive that we already started writing but then
// decided to abort (e.g. empty include/exclude result). Best-effort —
// surfaces nothing back to the caller because the original error already
// covers the user-visible outcome.
func deleteBackup(ctx context.Context, sm *StorageManager, serverUUID string, info storageInfo, key string) {
	switch info.Provider {
	case "local", "shared":
		var cfg localCfg
		if json.Unmarshal(info.Config, &cfg) != nil || cfg.BasePath == "" {
			return
		}
		os.Remove(filepath.Join(cfg.BasePath, filepath.Clean("/"+key)))
	case "node-local":
		if root, err := openBackupRoot(filepath.Join(resolveServerRoot(sm, serverUUID), backupDirName), false); err == nil {
			root.Remove(nodeLocalArchiveName(key))
			root.Close()
		}
	}
}

// matchAny returns true when `path` (slash-separated, relative to the
// archive root) matches any of the glob patterns. Patterns ending in /**
// match everything below a directory; otherwise filepath.Match semantics.
func matchAny(path string, patterns []string) bool {
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "/**") {
			if strings.HasPrefix(path, strings.TrimSuffix(p, "/**")+"/") || path == strings.TrimSuffix(p, "/**") {
				return true
			}
			continue
		}
		if ok, _ := filepath.Match(p, path); ok {
			return true
		}
		// Also match against any path component so "*.log" hits nested files.
		base := filepath.Base(path)
		if ok, _ := filepath.Match(p, base); ok {
			return true
		}
	}
	return false
}

// reportBackup publishes the run result so Core can update the DB row.
//
// On THIS node's own channel, never a fleet-wide one: Pub/Sub carries no sender
// identity, so the channel name is the only thing Core can attribute a result
// by. See queue.BackupResultsChannel for what the shared channel allowed. nodeID
// is the Core-assigned node identity - the same value the ACL user and every
// other node-scoped Redis name are built from.
func reportBackup(ctx context.Context, rdb *redis.Client, runID int, status, errMsg string, size int64) {
	payload := map[string]interface{}{
		"runId":     runID,
		"status":    status,
		"error":     errMsg,
		"sizeBytes": size,
		"timestamp": time.Now().Unix(),
	}
	data, _ := json.Marshal(payload)
	if err := rdb.Publish(ctx, queue.BackupResultsChannel(nodeID), data).Err(); err != nil {
		log.Printf("backup result publish failed: %v", err)
	}
}

// backupProgressInterval is how often a running backup says how much it has
// archived so far.
const backupProgressInterval = 5 * time.Second

// reportBackupProgress publishes the archived byte count until the returned
// stop function is called.
//
// BYTES, not a phase. The tar and the upload run CONCURRENTLY over a single
// pipe - the packer writes while the uploader reads - so "copying files" and
// then "uploading to storage" are not two stages that could be reported; they
// are the same stage seen from two ends. The byte count is the one number that
// is true at every moment of a run.
//
// A progress message can still lose a race with the terminal one, because
// Pub/Sub orders nothing. That is handled where it has to be anyway - Core
// ignores a "running" report for a run that has already finished - rather than
// by trying to stop this ticker before every one of RunBackup's exits.
func reportBackupProgress(ctx context.Context, rdb *redis.Client, runID int, counter *countingWriter) func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(backupProgressInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				reportBackup(ctx, rdb, runID, "running", "", counter.Total())
			}
		}
	}()
	return func() { close(done) }
}

// resolveServerRoot resolves the sub-server root via the node's
// StorageManager. Falls back to the legacy default when sm is nil so
// development/testing can still run without multi-storage.
func resolveServerRoot(sm *StorageManager, uuid string) string {
	if sm == nil {
		return filepath.Join("./dylaris_data/servers", uuid)
	}
	return sm.GetServerDir(uuid)
}

// copyExactly writes exactly size bytes of f: the size the tar header already
// promised. A server keeps writing while it is backed up (save-off does not
// stop the log), so a file that grew failed the whole backup with "write too
// long" and one that shrank with "missed writing". A grown file is cut at the
// size it had when it was listed; a shrunk one is padded with zeros.
func copyExactly(w io.Writer, f io.Reader, size int64) error {
	n, err := io.Copy(w, io.LimitReader(f, size))
	if err != nil {
		return err
	}
	if n < size {
		_, err = io.CopyN(w, zeroReader{}, size-n)
	}
	return err
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
