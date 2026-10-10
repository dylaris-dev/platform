package services

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	backupstorage "dylaris-core/storage/backup"
	"dylaris-core/store"

	"dylaris-pkg/validate"

	"github.com/redis/go-redis/v9"
)

// The storage orphan scan: objects in one of the platform's own buckets that no
// row names any more.
//
// It only ever OFFERS objects whose key has a shape Core itself writes, which is
// the line between "ours and forgotten" and "somebody else's". Library files,
// modpacks, ticket attachments and anything unrecognised are counted, never
// listed for deletion: an object store shared with another subsystem is normal
// here, and not knowing what a file is must never read as permission to delete it.

// OrphanMinAge is how old an object must be before it can be an orphan. A run
// row names its key before the upload starts, but a platform bundle's key is
// recorded only when its upload has finished, and a migration transfer is in
// flight for minutes - so a young unreferenced object is usually one being
// written right now.
const OrphanMinAge = 24 * time.Hour

// Orphan kinds.
const (
	OrphanServerBackup      = "server-backup"
	OrphanPlatformBackup    = "platform-backup"
	OrphanMigrationTransfer = "migration-transfer"
	OrphanProbe             = "probe"
)

// orphanServerIDPattern is validate.ServerUUID without its anchors. Real ids are
// "<uuid>_<random>", so a bare 36-char UUID pattern classified every real
// backup as unclassified and could never find an orphan of one.
var orphanServerIDPattern = strings.TrimSuffix(strings.TrimPrefix(validate.ServerUUID.String(), "^"), "$")

var (
	// NewBackupStorageKey; the random suffix is optional because keys written
	// before it existed are still around.
	serverBackupKeyRe = regexp.MustCompile(`^backups/(` + orphanServerIDPattern + `)/job-([0-9]+)/[0-9]{8}-[0-9]{6}(-[0-9a-f]{8})?\.tar\.gz$`)
	// PlatformBackupRunner.upload.
	platformBackupKeyRe = regexp.MustCompile(`^platform-backups/([0-9]+)/[0-9]{8}-[0-9]{6}-[^/]+\.dylaris-bundle$`)
	// MigrationOrchestrator.transferViaR2: "<server id>-<attempt>". The id may
	// contain '-' and the attempt (hex) does not, so the greedy group ends at
	// the last '-'.
	migrationTransferKeyRe = regexp.MustCompile(`^migration-transfer/(` + orphanServerIDPattern + `)-[^/]+\.zip$`)
	// handlers/backup_probe.go, a connection test that died between write and delete.
	probeKeyRe = regexp.MustCompile(`^__dylaris_probe_[0-9a-f]+\.(txt|multipart)$`)
)

// ClassifyOrphanKey reports which kind of object Core writes a key belongs to,
// and the server and job it names. ok is false for every key Core does not
// recognise as its own.
func ClassifyOrphanKey(key string) (kind, serverUUID string, jobID int, ok bool) {
	if m := serverBackupKeyRe.FindStringSubmatch(key); m != nil {
		id, _ := strconv.Atoi(m[2])
		return OrphanServerBackup, m[1], id, true
	}
	if m := platformBackupKeyRe.FindStringSubmatch(key); m != nil {
		id, _ := strconv.Atoi(m[1])
		return OrphanPlatformBackup, "", id, true
	}
	if m := migrationTransferKeyRe.FindStringSubmatch(key); m != nil {
		return OrphanMigrationTransfer, m[1], 0, true
	}
	if probeKeyRe.MatchString(key) {
		return OrphanProbe, "", 0, true
	}
	return "", "", 0, false
}

// OrphanCandidate is one object the scan offers for deletion.
type OrphanCandidate struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"lastModified"`
	Kind         string    `json:"kind"`
	ServerUUID   string    `json:"serverUuid,omitempty"`
	// ServerExists / JobExists are nil where the key names no server or job.
	ServerExists *bool `json:"serverExists,omitempty"`
	JobID        int   `json:"jobId,omitempty"`
	JobExists    *bool `json:"jobExists,omitempty"`
}

// MaxUnclassifiedListed caps OrphanScan.Unclassified; the counts cover the rest.
const MaxUnclassifiedListed = 200

// OrphanObject is a file the scan only reports, never offers.
type OrphanObject struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
	// LastModified is absent where the storage did not say.
	LastModified *time.Time `json:"lastModified,omitempty"`
}

// OrphanScan is the dry run: what would be offered, and what was left alone.
type OrphanScan struct {
	Candidates     []OrphanCandidate `json:"candidates"`
	CandidateBytes int64             `json:"candidateBytes"`
	// Live are unreferenced files whose server and job (or, for a platform
	// bundle, whose job) still exist. Never offered with the rest: that is
	// exactly what real backups look like to a database that lost their rows -
	// after a restore from a platform bundle, or to a second Core writing into
	// the same bucket. Deleting one needs an explicit includeLive.
	Live              []OrphanCandidate `json:"live"`
	LiveBytes         int64             `json:"liveBytes"`
	ReferencedCount   int               `json:"referencedCount"`
	ReferencedBytes   int64             `json:"referencedBytes"`
	RecentCount       int               `json:"recentCount"`
	RecentBytes       int64             `json:"recentBytes"`
	UnclassifiedCount int               `json:"unclassifiedCount"`
	UnclassifiedBytes int64             `json:"unclassifiedBytes"`
	// Unclassified lists the first MaxUnclassifiedListed of them, read-only.
	Unclassified []OrphanObject `json:"unclassified"`
	ScannedAt    time.Time      `json:"scannedAt"`
}

// isOrphanCandidate is the one rule both the scan and the delete apply.
// An unknown modification time is never old enough.
func isOrphanCandidate(key string, lastModified time.Time, referenced map[string]bool, now time.Time) bool {
	_, _, _, ok := ClassifyOrphanKey(key)
	return ok && !referenced[key] && !lastModified.IsZero() && now.Sub(lastModified) >= OrphanMinAge
}

// ClassifyOrphans sorts a listing into candidates and the three kinds of
// object that are left alone. Pure: existence badges are filled in afterwards.
func ClassifyOrphans(objs []backupstorage.Object, referenced map[string]bool, now time.Time) OrphanScan {
	scan := OrphanScan{Candidates: []OrphanCandidate{}, Unclassified: []OrphanObject{}, ScannedAt: now}
	for _, o := range objs {
		kind, serverUUID, jobID, ok := ClassifyOrphanKey(o.Key)
		switch {
		case !ok:
			scan.UnclassifiedCount++
			scan.UnclassifiedBytes += o.Size
			if len(scan.Unclassified) < MaxUnclassifiedListed {
				u := OrphanObject{Key: o.Key, Size: o.Size}
				if !o.LastModified.IsZero() {
					t := o.LastModified
					u.LastModified = &t
				}
				scan.Unclassified = append(scan.Unclassified, u)
			}
		case referenced[o.Key]:
			scan.ReferencedCount++
			scan.ReferencedBytes += o.Size
		case o.LastModified.IsZero() || now.Sub(o.LastModified) < OrphanMinAge:
			scan.RecentCount++
			scan.RecentBytes += o.Size
		default:
			scan.Candidates = append(scan.Candidates, OrphanCandidate{
				Key: o.Key, Size: o.Size, LastModified: o.LastModified,
				Kind: kind, ServerUUID: serverUUID, JobID: jobID,
			})
			scan.CandidateBytes += o.Size
		}
	}
	sort.Slice(scan.Candidates, func(i, j int) bool {
		return scan.Candidates[i].LastModified.Before(scan.Candidates[j].LastModified)
	})
	return scan
}

// OrphanObjectStore is the slice of a backup storage the scan needs.
type OrphanObjectStore interface {
	List(ctx context.Context, prefix string) ([]backupstorage.Object, error)
	Stat(ctx context.Context, key string) (backupstorage.Object, error)
	Delete(ctx context.Context, key string) error
}

// OrphanRefs answers which keys rows name, read fresh on every call. The
// migration sweep list belongs in it as well: those objects are already
// scheduled for deletion by their own owner.
type OrphanRefs func(ctx context.Context) (map[string]bool, error)

// StoreOrphanRefs reads the keys every backup row names plus the migration
// transfers waiting for their sweep. rdb may be nil (no Redis configured).
//
// A failed Redis read fails the whole answer rather than counting as "no
// transfers pending": a pending transfer is someone's move in flight.
func StoreOrphanRefs(st interface {
	ListReferencedBackupKeys() (map[string]bool, error)
}, rdb *redis.Client) OrphanRefs {
	return func(ctx context.Context) (map[string]bool, error) {
		keys, err := st.ListReferencedBackupKeys()
		if err != nil {
			return nil, err
		}
		if rdb == nil {
			return keys, nil
		}
		pending, err := rdb.ZRange(ctx, migrationR2SweepKey, 0, -1).Result()
		if err != nil {
			return nil, fmt.Errorf("reading the migration sweep list: %w", err)
		}
		for _, k := range pending {
			keys[k] = true
		}
		return keys, nil
	}
}

// OrphanLookups answers the existence badges. Any may be nil. An answer the
// store could not give must be "exists": that keeps a file out of the
// deletable list, never the other way round.
type OrphanLookups struct {
	ServerExists         func(uuid string) bool
	BackupJobExists      func(id int) bool
	PlatformBackupExists func(id int) bool
}

// ScanOrphans lists the whole storage and classifies it.
func ScanOrphans(ctx context.Context, st OrphanObjectStore, refs OrphanRefs, look OrphanLookups, now time.Time) (OrphanScan, error) {
	objs, err := st.List(ctx, "")
	if err != nil {
		return OrphanScan{}, fmt.Errorf("listing the storage: %w", err)
	}
	referenced, err := refs(ctx)
	if err != nil {
		return OrphanScan{}, fmt.Errorf("reading the referenced keys: %w", err)
	}
	scan := ClassifyOrphans(objs, referenced, now)
	cache := map[string]bool{}
	all := scan.Candidates
	scan.Candidates = []OrphanCandidate{}
	scan.Live = []OrphanCandidate{}
	scan.CandidateBytes = 0
	for _, c := range all {
		c.ServerExists, c.JobExists = look.existence(c.Kind, c.ServerUUID, c.JobID, cache)
		if isLiveOrphan(c.ServerExists, c.JobExists) {
			scan.Live = append(scan.Live, c)
			scan.LiveBytes += c.Size
			continue
		}
		scan.Candidates = append(scan.Candidates, c)
		scan.CandidateBytes += c.Size
	}
	return scan, nil
}

// existence answers the badges for one key, memoised in cache.
func (look OrphanLookups) existence(kind, serverUUID string, jobID int, cache map[string]bool) (server, job *bool) {
	ask := func(k string, f func() bool) *bool {
		v, seen := cache[k]
		if !seen {
			v = f()
			cache[k] = v
		}
		return &v
	}
	if serverUUID != "" && look.ServerExists != nil {
		server = ask("server/"+serverUUID, func() bool { return look.ServerExists(serverUUID) })
	}
	var exists func(int) bool
	switch kind {
	case OrphanServerBackup:
		exists = look.BackupJobExists
	case OrphanPlatformBackup:
		exists = look.PlatformBackupExists
	}
	if exists != nil {
		job = ask(kind+"/"+strconv.Itoa(jobID), func() bool { return exists(jobID) })
	}
	return server, job
}

// isLiveOrphan: the job still exists, and so does the server where the key
// names one. A platform bundle names no server, so its job alone decides.
func isLiveOrphan(server, job *bool) bool {
	return job != nil && *job && (server == nil || *server)
}

// MaxOrphanDeleteKeys bounds one delete request.
const MaxOrphanDeleteKeys = 1000

// OrphanDeleteResult is the outcome for one requested key.
type OrphanDeleteResult struct {
	Key     string `json:"key"`
	Deleted bool   `json:"deleted"`
	Size    int64  `json:"size,omitempty"`
	// Reason says why a key was not deleted.
	Reason string `json:"reason,omitempty"`
}

// DeleteOrphans deletes the requested keys that are STILL orphans now.
//
// The scan the operator looked at may be minutes or hours old, so nothing it
// said is trusted: the references are read again for every key immediately
// before its delete, and the object is asked for its age again. A key that a
// row started naming in between, or that was rewritten, is refused. So is a
// live one (see OrphanScan.Live) unless includeLive is set.
func DeleteOrphans(ctx context.Context, st OrphanObjectStore, refs OrphanRefs, look OrphanLookups,
	keys []string, includeLive bool, now func() time.Time) ([]OrphanDeleteResult, error) {
	if len(keys) > MaxOrphanDeleteKeys {
		return nil, fmt.Errorf("at most %d keys per request", MaxOrphanDeleteKeys)
	}
	out := make([]OrphanDeleteResult, 0, len(keys))
	seen := map[string]bool{}
	for _, key := range keys {
		if seen[key] {
			continue
		}
		seen[key] = true
		res := OrphanDeleteResult{Key: key}
		kind, serverUUID, jobID, ok := ClassifyOrphanKey(key)
		if !ok {
			res.Reason = "not a file Core writes"
			out = append(out, res)
			continue
		}
		if !includeLive && isLiveOrphan(look.existence(kind, serverUUID, jobID, map[string]bool{})) {
			res.Reason = "its server and backup job still exist; it may be a real backup this database lost"
			out = append(out, res)
			continue
		}
		obj, err := st.Stat(ctx, key)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				res.Reason = "already gone"
			} else {
				res.Reason = "could not read the file: " + err.Error()
			}
			out = append(out, res)
			continue
		}
		res.Size = obj.Size
		referenced, err := refs(ctx)
		if err != nil {
			// Without the references nothing below is safe; stop rather than
			// guess for the remaining keys.
			res.Reason = "could not read the referenced keys"
			out = append(out, res)
			return out, fmt.Errorf("reading the referenced keys: %w", err)
		}
		if !isOrphanCandidate(key, obj.LastModified, referenced, now()) {
			if referenced[key] {
				res.Reason = "a backup now refers to it"
			} else {
				res.Reason = "modified within the last 24 hours"
			}
			out = append(out, res)
			continue
		}
		if err := st.Delete(ctx, key); err != nil {
			res.Reason = "delete failed: " + err.Error()
			out = append(out, res)
			continue
		}
		res.Deleted = true
		out = append(out, res)
	}
	return out, nil
}

// MigrationTransferTTL is how long the presigned URLs of a migration transfer
// stay valid, the same value transferViaR2 signs them with.
func MigrationTransferTTL(st store.Store) time.Duration {
	return presignTTL(st, true)
}
