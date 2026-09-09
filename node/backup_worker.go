package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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
	// PresignedPutURL, when set, is a pre-signed S3/R2 PUT URL the node uploads
	// to instead of using bucket credentials (BYON tenant nodes never receive
	// the operator's creds). Empty = use Storage creds (operator nodes).
	PresignedPutURL string `json:"presignedPutUrl"`
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

type s3Cfg struct {
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	ForcePathStyle  bool   `json:"forcePathStyle"`
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

// RunBackup builds the archive and streams it directly to storage via an
// io.Pipe — no buffering in RAM. The tar+gzip writer runs in a goroutine
// that pushes bytes into the pipe; the storage uploader reads from the
// other end and pushes them out. For multi-GB worlds this keeps the
// node's working set under a few megabytes regardless of archive size.
// saveFlushWait is how long `save-all flush` is given before the files are
// read.
//
// It is a WAIT, not a confirmation. A server acknowledges a save on its
// console, which this process does not read, so there is nothing to observe.
// gracefulStop makes the same trade with the same command and three seconds;
// this one is longer on purpose - a stop that reads a moment early loses that
// moment, while a backup that reads a moment early produces a damaged archive
// that nobody finds out about until a restore.
const saveFlushWait = 5 * time.Second

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
	send func(command string)
}

// pauseWorldSaves stops the server writing its world, flushes what it holds,
// and returns the guard that puts it back.
func pauseWorldSaves(ctx context.Context, rdb *redis.Client, dm *DockerManager, uuid string) *worldSaveGuard {
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
	inputKey := fmt.Sprintf("dylaris:server:%s:input", uuid)
	g.send = func(command string) { rdb.RPush(context.Background(), inputKey, command) }

	g.send("save-off")
	g.send("save-all flush")
	log.Printf("backup: server %s: saving paused and flushed before the archive", uuid)
	time.Sleep(saveFlushWait)
	return g
}

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
	send("save-on")
	log.Printf("backup: server %s: saving resumed", g.uuid)
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

	started := time.Now()
	storage := storageInfo{}
	if err := json.Unmarshal(cmd.Storage, &storage); err != nil {
		reportBackup(ctx, rdb, cmd.RunID, "failed", "invalid storage payload: "+err.Error(), 0)
		return
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

	// Everything that could refuse this run has now refused it, so this is the
	// last point at which pausing saves would be wasted. From here the guard
	// covers every exit, including the panic paths.
	saves := pauseWorldSaves(ctx, rdb, dm, cmd.ServerUUID)
	defer saves.resume()

	// For node-local storage the destination is on the same disk we're
	// reading from — no pipe/uploader is needed. We still want to keep
	// the streaming tar+gzip path so RAM stays bounded for large worlds,
	// so we use the same io.Pipe + uploader plumbing and let
	// uploadBackup() switch on storage.Provider to do the right thing
	// (io.Copy to a hidden .dylaris-backups/<id>.tar.gz on the Node).
	pr, pw := io.Pipe()
	// counting writer wraps the pipe writer so we can report the archived
	// size without waiting for the upload to complete.
	counter := &countingWriter{}
	mw := io.MultiWriter(pw, counter)

	stopProgress := reportBackupProgress(ctx, rdb, cmd.RunID, counter)
	defer stopProgress()

	addedAny := false

	go func() {
		// The goroutine is the source side of the pipe. Any error we hit
		// has to propagate to the reader by closing the pipe with that
		// error so the uploader sees it and aborts cleanly.
		added, err := writeServerArchive(mw, serverRoot, rootDir, cmd.IncludePatterns, cmd.ExcludePatterns, cmd.Manifest)
		addedAny = added
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.Close()
	}()

	// Upload reads from the pipe. Returns once EOF (or pipe error) reached.
	// A BYON tenant node gets a pre-signed PUT URL and never sees bucket creds;
	// it stages the archive to a temp file first (PUT needs Content-Length).
	var upErr error
	if cmd.PresignedPutURL != "" {
		upErr = uploadBackupPresigned(ctx, cmd.PresignedPutURL, pr)
	} else {
		upErr = uploadBackup(ctx, sm, cmd.ServerUUID, storage, cmd.StorageKey, pr)
	}
	if upErr != nil {
		// Drain any remaining bytes so the writer goroutine doesn't block on
		// a full pipe; CloseWithError unblocks the writer immediately.
		pr.CloseWithError(upErr)
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
		// best-effort on a key that was never written (a BYON node holds no
		// bucket credentials, so its S3 branch returns without doing anything;
		// its own staged temp file is already removed by uploadBackupPresigned).
		deleteBackup(ctx, sm, cmd.ServerUUID, storage, cmd.StorageKey)
		reportBackup(ctx, rdb, cmd.RunID, "failed", "upload failed: "+upErr.Error(), 0)
		return
	}

	size := counter.Total()
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
	resolvedRoot := resolveZipRoot(serverRoot)
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

	walkErr := filepath.Walk(rootDir, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		rel, _ := filepath.Rel(rootDir, path)
		rel = filepath.ToSlash(rel)
		if rel == "." {
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
		// The same symlink guard the zip walkers take. Skipping it here was
		// not a leak but a hard stop: Walk reports a link via Lstat, so
		// FileInfoHeader emits a header-only symlink entry of size 0, and the
		// os.Open below then FOLLOWS the link and copies the target's bytes
		// into it. The tar writer answers that with ErrWriteTooLong, which
		// aborts the walk - so one link anywhere under a server, planted over
		// SFTP or from inside its own container, failed every backup of that
		// server from then on.
		info, ok := zipEntryInfo(resolvedRoot, path, info)
		if !ok {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.IsDir() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(tw, f)
			f.Close()
			if copyErr != nil {
				return copyErr
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

// uploadBackup streams the body to whatever provider the storage config
// names. For local/shared that's a simple io.Copy to disk; for S3 we use
// the SDK manager.Uploader which transparently switches to multipart
// upload when the body exceeds the part-size threshold, so we never have
// to know the final archive size up front. For node-local the archive
// lands inside the server's own .dylaris-backups/ folder on this Node's
// disk — the storage key's tail filename is taken as the archive name and
// the directory is created on demand.
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
		defer f.Close()
		_, err = io.Copy(f, r)
		return err

	case "node-local":
		// Resolve <server-dir>/.dylaris-backups/<archive>. The Core-side
		// storage key includes a "backups/<serverUUID>/job-<jobID>/..."
		// prefix for display, but on-disk we collapse it to the leaf
		// filename — the directory is server-scoped already so the prefix
		// adds no information.
		dir := filepath.Join(resolveServerRoot(sm, serverUUID), backupDirName)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create node-local backup dir: %w", err)
		}
		archive := nodeLocalArchiveName(key)
		full := filepath.Join(dir, archive)
		// The write side of the same problem archiveInfo describes: this
		// directory is inside the tenant's own bind mount, so O_CREATE|O_TRUNC
		// would follow a symlink planted under the name the next run is about to
		// use and write the archive through it. Remove first (that unlinks a
		// link, it does not follow one) and then insist on creating the file
		// ourselves - O_EXCL fails on anything that reappeared in between, so
		// the race ends in a failed backup rather than a write somewhere else.
		if rerr := os.Remove(full); rerr != nil && !os.IsNotExist(rerr) {
			return fmt.Errorf("clear previous archive: %w", rerr)
		}
		f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(f, r)
		return err

	case "s3":
		client, bucket, err := buildS3Client(ctx, info.Config)
		if err != nil {
			return err
		}
		uploader := manager.NewUploader(client, func(u *manager.Uploader) {
			u.PartSize = 16 * 1024 * 1024 // 16 MiB per part — balances RAM vs. PUT count
			u.Concurrency = 3             // 3 in-flight parts, ~48 MiB peak window
		})
		_, err = uploader.Upload(ctx, &s3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
			Body:   r,
		})
		return err

	default:
		return fmt.Errorf("unknown provider %s", info.Provider)
	}
}

// presignedPutMaxSize is the S3/R2 single-PUT object limit. Larger BYON backups
// would need multipart-presigned (a follow-up); we fail clearly rather than
// silently truncate.
const presignedPutMaxSize = 5 * 1024 * 1024 * 1024 // 5 GiB

// uploadBackupPresigned uploads the archive to a pre-signed PUT URL. The PUT
// needs a Content-Length, but the archive is produced as an unbounded stream, so
// it is staged to a temp file first to learn the size. Used only for BYON tenant
// nodes (which must never receive bucket credentials).
func uploadBackupPresigned(ctx context.Context, url string, r io.Reader) error {
	tmp, err := os.CreateTemp("", "dylaris-backup-*.tar.gz")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	size, err := io.Copy(tmp, r)
	if err != nil {
		return fmt.Errorf("stage archive: %w", err)
	}
	if size > presignedPutMaxSize {
		return fmt.Errorf("archive %d bytes exceeds the 5 GiB single-upload limit for tenant nodes", size)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, tmp)
	if err != nil {
		return err
	}
	req.ContentLength = size
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("presigned put: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("presigned put status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
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
		archive := nodeLocalArchiveName(key)
		os.Remove(filepath.Join(resolveServerRoot(sm, serverUUID), backupDirName, archive))
	case "s3":
		client, bucket, err := buildS3Client(ctx, info.Config)
		if err != nil {
			return
		}
		client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
	}
}

// buildS3Client centralises the SDK setup so the streaming upload and the
// best-effort delete share the same credential / endpoint resolution.
func buildS3Client(ctx context.Context, raw json.RawMessage) (*s3.Client, string, error) {
	var cfg s3Cfg
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, "", fmt.Errorf("invalid s3 cfg: %w", err)
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")),
	)
	if err != nil {
		return nil, "", err
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.ForcePathStyle
	})
	return client, cfg.Bucket, nil
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
