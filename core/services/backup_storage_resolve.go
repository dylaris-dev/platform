package services

import (
	"errors"

	"dylaris-core/models"
)

// ErrNoBackupStorage means neither the job, the tenant nor the platform has a
// storage to use. It is the only case where refusing is correct.
var ErrNoBackupStorage = errors.New("no backup storage available")

// ErrForeignBackupStorage means the job points at a storage belonging to
// somebody else. Refused rather than resolved past: a job that silently fell
// back to the platform default would write a tenant's data somewhere they did
// not choose, and one that honoured the pointer would write it into a stranger's
// bucket.
var ErrForeignBackupStorage = errors.New("that backup storage belongs to another account")

// backupStorageLookup is the narrow port ResolveJobStorage needs, satisfied by
// *store.PostgresStore and by the scheduler's own store.
type backupStorageLookup interface {
	GetBackupStorage(id int) (*models.BackupStorage, error)
	GetDefaultBackupStorage() (*models.BackupStorage, error)
	GetUserDefaultBackupStorage(ownerID string) (*models.BackupStorage, error)
	GetUserStoragePrefs(userID string) (backups, templates *int, err error)
}

// ResolveJobStorage returns the storage a backup job uses, walking
// job -> the owner's backups preference -> the owner's own default -> the
// platform default.
//
// This exists because the two halves disagreed. The run path already had the
// fallback and named it - "Resolve storage (job-level or fallback to default)" -
// while restore, download and the archive verification each did
// `if job.StorageID == nil { refuse }`. The panel's storage dropdown offers
// "Default storage" as its FIRST option, which stores NULL, so a job created by
// not choosing backed up perfectly and then could not be restored, downloaded,
// or even checked for whether its archive existed.
//
// Backups that cannot be restored are worse than no backups: the failure only
// surfaces when someone is already trying to recover.
//
// ownerID is who the job belongs to, and may be empty where the caller has no
// owner in hand (platform-internal paths). Empty simply skips the middle step -
// it must never be treated as "matches any owner", which would let one tenant's
// job resolve into another tenant's bucket.
func ResolveJobStorage(s backupStorageLookup, storageID *int, ownerID string) (*models.BackupStorage, error) {
	if storageID == nil && ownerID != "" {
		// A preference is resolved exactly like a storage the job names, so a
		// vanished or foreign one is refused rather than resolved past.
		pref, _, err := s.GetUserStoragePrefs(ownerID)
		if err != nil {
			return nil, err
		}
		storageID = pref
	}
	if storageID != nil {
		bs, err := s.GetBackupStorage(*storageID)
		if err != nil {
			return nil, err
		}
		if bs == nil {
			return nil, ErrNoBackupStorage
		}
		// A platform storage (no owner) is available to everyone; a tenant's own
		// is available to them alone.
		if bs.OwnerID != nil && *bs.OwnerID != ownerID {
			return nil, ErrForeignBackupStorage
		}
		return bs, nil
	}
	if ownerID != "" {
		bs, err := s.GetUserDefaultBackupStorage(ownerID)
		if err != nil {
			return nil, err
		}
		if bs != nil {
			return bs, nil
		}
	}
	bs, err := s.GetDefaultBackupStorage()
	if err != nil {
		return nil, err
	}
	if bs == nil {
		return nil, ErrNoBackupStorage
	}
	return bs, nil
}

// jobOwnerLookup is the one call needed to turn a job into an owner.
type jobOwnerLookup interface {
	GetServerByID(id int) (*models.Server, error)
}

// BackupJobOwner is who a job's server belongs to, or "" when it cannot be
// determined. Empty is the safe answer rather than a guess: it skips the
// tenant step of the chain, and it never matches an owner.
func BackupJobOwner(s jobOwnerLookup, serverID int) string {
	srv, err := s.GetServerByID(serverID)
	if err != nil || srv == nil {
		return ""
	}
	return srv.OwnerID
}

// ResolveRunStorage is where ONE archive actually lives.
//
// A run records the storage it was written to, and that is what is used when it
// has one. Re-deriving it from the job is only correct while a job never changes
// storage: change it, and every previous archive becomes unreachable for
// restore, download and deletion while still listed as present. Runs from before
// the column existed fall back to the job chain, which is exactly the old
// behaviour for exactly the rows that had no better answer.
func ResolveRunStorage(s backupStorageLookup, run *models.BackupRun, jobStorageID *int, ownerID string) (*models.BackupStorage, error) {
	if run != nil && run.StorageID != nil {
		return ResolveJobStorage(s, run.StorageID, ownerID)
	}
	if jobStorageID != nil {
		return ResolveJobStorage(s, jobStorageID, ownerID)
	}
	// Nothing recorded on either. The run therefore predates the column, and a
	// run that predates the column predates tenants having a default at all - so
	// it went to the PLATFORM default and nowhere else.
	//
	// Walking the full chain here would be actively wrong: a tenant who
	// connects a bucket tomorrow would have yesterday's archives resolved into
	// it, where S3 answers a delete of a missing key with success. The row would
	// be dropped and our object left orphaned with nothing pointing at it.
	bs, err := s.GetDefaultBackupStorage()
	if err != nil {
		return nil, err
	}
	if bs == nil {
		return nil, ErrNoBackupStorage
	}
	return bs, nil
}
