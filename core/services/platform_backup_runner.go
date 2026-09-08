package services

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"dylaris-core/models"
	"dylaris-core/services/bundle"
)

// Running one platform backup.
//
// Everything the run needs from the outside is a function, not a type. Not
// ceremony: this package is imported BY handlers and by storage, so taking the
// concrete destination or the concrete core storage would be a cycle - and the
// decisions worth testing here (what to include, what to skip, what to record)
// are exactly the ones a fake makes visible.

// BundleFile is one object in a core storage area.
type BundleFile struct {
	Key  string
	Size int64
}

// CoreStorageArea is one area of the shared Core file storage, already scoped.
type CoreStorageArea interface {
	Walk(ctx context.Context) ([]BundleFile, error)
	Open(ctx context.Context, key string) (io.ReadCloser, error)
}

// BundleDestination is where the finished bundle goes. Deliberately just the
// write: a run never reads or lists.
type BundleDestination interface {
	Put(ctx context.Context, key string, r io.Reader, size int64) error
}

type platformRunnerStore interface {
	GetPlatformBackupJob(id int) (*models.PlatformBackupJob, error)
	GetSetting(key string) (string, error)
	GetBackupStorage(id int) (*models.BackupStorage, error)
	GetDefaultBackupStorage() (*models.BackupStorage, error)
	GetUserDefaultBackupStorage(ownerID string) (*models.BackupStorage, error)
	ListBackupTargetServers() ([]models.BackupTargetServer, error)
	CreatePlatformBackupRun(jobID int, storageID *int) (int, error)
	FinishPlatformBackupRun(id int, status string, sizeBytes int64,
		storageKey, errMessage string, components []models.PlatformBackupComponent) error
}

// PlatformBackupPassphraseSetting is where the operator's backup passphrase
// lives. A secret setting, so it is encrypted at rest under the same key as the
// DNS token and the Resend key - and therefore moved by ResealAtRest, which a
// purpose of its own would not have been.
const PlatformBackupPassphraseSetting = "platform_backup.passphrase"

// ErrNoBackupPassphrase is a platform backup attempted before one was set.
// Refused rather than defaulted: a bundle with a guessable passphrase is worse
// than no bundle, because it is trusted.
var ErrNoBackupPassphrase = fmt.Errorf("no platform backup passphrase is set; set one in Settings before running a platform backup")

// PlatformBackupRunner assembles and stores one bundle.
type PlatformBackupRunner struct {
	Store   platformRunnerStore
	Release string

	// ClusterSecret travels INSIDE the encrypted payload. The database in this
	// bundle holds values encrypted under it, and the warp leader's WireGuard
	// identity is derived from it and stored nowhere at all.
	ClusterSecret string

	// WorkDir is where components are spooled. tar writes a member's size
	// before its bytes, so anything of unknown length - pg_dump, a walk - has
	// to land on disk first. The ceiling is the largest single component, not
	// the bundle.
	WorkDir string

	DB          PGConn
	ServerMajor int

	OpenDest            func(ctx context.Context, bs *models.BackupStorage) (BundleDestination, error)
	OpenCoreStorage     func(area string) (CoreStorageArea, error)
	TriggerServerBackup func(ctx context.Context, serverID int) (ref string, err error)
	MetricsConfigured   func() bool

	// now is injectable so a test can name the key it expects.
	now func() time.Time
}

func (r *PlatformBackupRunner) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now().UTC()
}

// Run executes one platform backup and returns the run id.
//
// The run row is opened BEFORE any work, so a run that dies mid-way is visible
// as one that never finished rather than as nothing at all. Everything after
// that reports through the row.
func (r *PlatformBackupRunner) Run(ctx context.Context, jobID int) (int, error) {
	job, err := r.Store.GetPlatformBackupJob(jobID)
	if err != nil || job == nil {
		return 0, fmt.Errorf("platform backup: job %d: %w", jobID, err)
	}
	if err := job.Selection.Validate(); err != nil {
		return 0, fmt.Errorf("platform backup: %w", err)
	}
	passphrase, err := r.Store.GetSetting(PlatformBackupPassphraseSetting)
	if err != nil {
		return 0, fmt.Errorf("platform backup: reading the passphrase: %w", err)
	}
	if passphrase == "" {
		return 0, ErrNoBackupPassphrase
	}
	// Platform-owned, so the owner step of the storage chain is skipped: an
	// empty owner never matches a tenant's storage, which is what keeps a
	// platform bundle out of a customer's bucket.
	dest, err := ResolveJobStorage(r.Store, job.StorageID, "")
	if err != nil {
		return 0, fmt.Errorf("platform backup: destination: %w", err)
	}

	runID, err := r.Store.CreatePlatformBackupRun(jobID, &dest.ID)
	if err != nil {
		return 0, fmt.Errorf("platform backup: opening the run: %w", err)
	}

	key, size, components, err := r.build(ctx, job, passphrase, dest)
	if err != nil {
		// The components gathered so far are kept: a run that died on the
		// third component should say which two it had.
		if ferr := r.Store.FinishPlatformBackupRun(runID, "failed", 0, "", err.Error(), components); ferr != nil {
			logErrf("platform-backup", "run %d: recording the failure: %v", runID, ferr)
		}
		return runID, err
	}
	if err := r.Store.FinishPlatformBackupRun(runID, "success", size, key, "", components); err != nil {
		logErrf("platform-backup", "run %d: recording the result: %v", runID, err)
	}
	return runID, nil
}

// build writes the bundle to a spool file and uploads it.
//
// Spooled rather than streamed to the destination, because backup.Storage.Put
// takes a size: the destination has to be told how many bytes are coming, and
// that is only known once the bundle is finished.
func (r *PlatformBackupRunner) build(ctx context.Context, job *models.PlatformBackupJob,
	passphrase string, dest *models.BackupStorage) (string, int64, []models.PlatformBackupComponent, error) {

	var components []models.PlatformBackupComponent

	if err := os.MkdirAll(r.WorkDir, 0o700); err != nil {
		return "", 0, components, fmt.Errorf("work directory: %w", err)
	}
	spool, err := os.CreateTemp(r.WorkDir, "platform-bundle-*.tmp")
	if err != nil {
		return "", 0, components, fmt.Errorf("spool file: %w", err)
	}
	spoolPath := spool.Name()
	defer func() {
		spool.Close()
		if rerr := os.Remove(spoolPath); rerr != nil && !os.IsNotExist(rerr) {
			log.Printf("platform backup: could not remove %s: %v", spoolPath, rerr)
		}
	}()

	w, err := bundle.NewWriter(spool, passphrase, r.Release)
	if err != nil {
		return "", 0, components, err
	}

	// The manifest is the FIRST entry, so it describes what the run RESOLVED to
	// do. What each part actually did is the run row's business and is recorded
	// there; a reader that only has the file learns the plan, which is what a
	// restore needs.
	servers, missing := r.resolveServers(job.Selection.Servers)
	if err := w.WriteManifest(&bundle.Manifest{
		Schema:        bundle.Schema,
		CreatedAt:     r.clock(),
		Source:        r.Release,
		ClusterSecret: r.ClusterSecret,
		Selection:     job.Selection,
		Components:    r.plannedComponents(job.Selection, servers, missing),
	}); err != nil {
		return "", 0, components, fmt.Errorf("manifest: %w", err)
	}

	// A selected server that has since been deleted is recorded and skipped.
	// A saved selection outlives what it names, so this is an ordinary outcome
	// of a healthy run and must never fail the run around it.
	components = append(components, SkippedServerComponents(missing)...)

	if job.Selection.Database {
		c, err := r.addDatabase(ctx, w)
		components = append(components, c)
		if err != nil {
			return "", 0, components, err
		}
	}
	if job.Selection.MetricsDB {
		components = append(components, r.metricsComponent())
	}
	for _, area := range coreStorageAreas(job.Selection) {
		cs, err := r.addCoreStorage(ctx, w, area)
		components = append(components, cs...)
		if err != nil {
			return "", 0, components, err
		}
	}
	for _, srv := range servers {
		components = append(components, r.triggerServer(ctx, srv))
	}

	if err := w.Close(); err != nil {
		return "", 0, components, fmt.Errorf("closing the bundle: %w", err)
	}

	size, err := spool.Seek(0, io.SeekEnd)
	if err != nil {
		return "", 0, components, err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return "", 0, components, err
	}

	store, err := r.OpenDest(ctx, dest)
	if err != nil {
		return "", 0, components, fmt.Errorf("opening the destination: %w", err)
	}
	key := fmt.Sprintf("platform-backups/%d/%s-%s.dylaris-bundle",
		job.ID, r.clock().Format("20060102-150405"), filepath.Base(spoolPath))
	if err := store.Put(ctx, key, spool, size); err != nil {
		return "", 0, components, fmt.Errorf("uploading the bundle: %w", err)
	}
	return key, size, components, nil
}

func (r *PlatformBackupRunner) resolveServers(sel models.PlatformBackupServers) ([]models.BackupTargetServer, []int) {
	if sel.Mode == models.PlatformBackupServersNone {
		return nil, nil
	}
	all, err := r.Store.ListBackupTargetServers()
	if err != nil {
		logErrf("platform-backup", "listing servers: %v", err)
		return nil, nil
	}
	return SelectBackupServers(all, sel)
}

// plannedComponents is what the run set out to cover, for the manifest inside
// the bundle. Sizes and outcomes are not known yet and belong to the run row.
func (r *PlatformBackupRunner) plannedComponents(sel models.PlatformBackupSelection,
	servers []models.BackupTargetServer, missing []int) []models.PlatformBackupComponent {

	out := []models.PlatformBackupComponent{}
	if sel.Database {
		out = append(out, models.PlatformBackupComponent{Kind: "database", Status: models.PlatformBackupIncluded})
	}
	if sel.MetricsDB {
		out = append(out, r.metricsComponent())
	}
	for _, area := range coreStorageAreas(sel) {
		out = append(out, models.PlatformBackupComponent{Kind: area, Status: models.PlatformBackupIncluded})
	}
	for _, s := range servers {
		out = append(out, models.PlatformBackupComponent{Kind: "server", Ref: s.UUID, Status: models.PlatformBackupIncluded})
	}
	return append(out, SkippedServerComponents(missing)...)
}

// coreStorageAreas is the selected areas of the shared Core file storage, in a
// fixed order so two runs of one selection produce the same bundle layout.
func coreStorageAreas(sel models.PlatformBackupSelection) []string {
	var out []string
	if sel.Library {
		out = append(out, "library")
	}
	if sel.Modpacks {
		out = append(out, "modpacks")
	}
	return out
}

func (r *PlatformBackupRunner) addDatabase(ctx context.Context, w *bundle.Writer) (models.PlatformBackupComponent, error) {
	c := models.PlatformBackupComponent{Kind: "database"}

	f, err := os.CreateTemp(r.WorkDir, "pgdump-*.tmp")
	if err != nil {
		c.Status = models.PlatformBackupFailed
		c.Message = err.Error()
		return c, err
	}
	path := f.Name()
	defer func() {
		f.Close()
		os.Remove(path)
	}()

	if err := DumpDatabase(ctx, r.DB, r.ServerMajor, f); err != nil {
		c.Status = models.PlatformBackupFailed
		c.Message = err.Error()
		return c, err
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		c.Status = models.PlatformBackupFailed
		c.Message = err.Error()
		return c, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		c.Status = models.PlatformBackupFailed
		c.Message = err.Error()
		return c, err
	}
	if err := w.AddReader("database.dump", size, f); err != nil {
		c.Status = models.PlatformBackupFailed
		c.Message = err.Error()
		return c, err
	}
	c.Status = models.PlatformBackupIncluded
	c.SizeBytes = size
	return c, nil
}

// metricsComponent records the statistics database as not covered.
//
// Recorded rather than silently omitted, and skipped rather than half-taken:
// the metrics database is TimescaleDB, whose hypertables do not survive a plain
// dump and restore. A dump that only fails at restore time is worse than none,
// because it is relied on until the hour it is needed.
func (r *PlatformBackupRunner) metricsComponent() models.PlatformBackupComponent {
	msg := "the statistics database is TimescaleDB and is not covered by a platform bundle yet"
	if r.MetricsConfigured != nil && !r.MetricsConfigured() {
		msg = "no statistics database is configured"
	}
	return models.PlatformBackupComponent{Kind: "metrics", Status: models.PlatformBackupSkipped, Message: msg}
}

func (r *PlatformBackupRunner) addCoreStorage(ctx context.Context, w *bundle.Writer, area string) ([]models.PlatformBackupComponent, error) {
	c := models.PlatformBackupComponent{Kind: area}
	if r.OpenCoreStorage == nil {
		c.Status = models.PlatformBackupSkipped
		c.Message = "no Core file storage is configured"
		return []models.PlatformBackupComponent{c}, nil
	}
	src, err := r.OpenCoreStorage(area)
	if err != nil {
		c.Status = models.PlatformBackupFailed
		c.Message = err.Error()
		return []models.PlatformBackupComponent{c}, fmt.Errorf("%s: %w", area, err)
	}
	files, err := src.Walk(ctx)
	if err != nil {
		c.Status = models.PlatformBackupFailed
		c.Message = err.Error()
		return []models.PlatformBackupComponent{c}, fmt.Errorf("%s: %w", area, err)
	}

	var total int64
	for _, f := range files {
		rc, err := src.Open(ctx, f.Key)
		if err != nil {
			c.Status = models.PlatformBackupFailed
			c.Message = fmt.Sprintf("%s: %v", f.Key, err)
			return []models.PlatformBackupComponent{c}, fmt.Errorf("%s/%s: %w", area, f.Key, err)
		}
		err = w.AddReader(area+"/"+f.Key, f.Size, rc)
		rc.Close()
		if err != nil {
			c.Status = models.PlatformBackupFailed
			c.Message = fmt.Sprintf("%s: %v", f.Key, err)
			return []models.PlatformBackupComponent{c}, fmt.Errorf("%s/%s: %w", area, f.Key, err)
		}
		total += f.Size
	}
	c.Status = models.PlatformBackupIncluded
	c.SizeBytes = total
	return []models.PlatformBackupComponent{c}, nil
}

// triggerServer asks a server's OWN backup job to run.
//
// The worlds do not travel through Core, and that is the design rather than a
// shortcut. A server backup already has a schedule, a quota, a retention policy
// and a destination belonging to its owner; archiving it a second time inside a
// platform run would either ignore all of that or implement it twice, and every
// run would carry the full size of every world. The bundle records the
// reference instead.
//
// A failure here does NOT fail the run. One server whose node is offline must
// not cost the operator the database backup that was the point of the run.
func (r *PlatformBackupRunner) triggerServer(ctx context.Context, srv models.BackupTargetServer) models.PlatformBackupComponent {
	c := models.PlatformBackupComponent{Kind: "server", Ref: srv.UUID}
	if r.TriggerServerBackup == nil {
		c.Status = models.PlatformBackupSkipped
		c.Message = "server backups cannot be started from here"
		return c
	}
	ref, err := r.TriggerServerBackup(ctx, srv.ID)
	if err != nil {
		c.Status = models.PlatformBackupFailed
		c.Message = err.Error()
		return c
	}
	c.Status = models.PlatformBackupIncluded
	c.Message = ref
	return c
}
