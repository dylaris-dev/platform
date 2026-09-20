package services

import (
	"context"
	"errors"
	"testing"

	"dylaris-core/models"
	backupstorage "dylaris-core/storage/backup"
	"dylaris-core/store"
)

// fakeBackupStorage records what was deleted and can be told to fail. The
// interface is embedded: only Delete is exercised here, and a nil call on any
// other method would be a test reaching further than it claims to.
type fakeBackupStorage struct {
	backupstorage.Storage

	deleted []string
	failOn  string
}

func (f *fakeBackupStorage) Delete(_ context.Context, key string) error {
	if key == f.failOn {
		return errors.New("bucket said no")
	}
	f.deleted = append(f.deleted, key)
	return nil
}

// purgeStore answers the storage lookups the resolver makes.
type purgeStore struct {
	byID             map[int]*models.BackupStorage
	platform         *models.BackupStorage
	userDefault      *models.BackupStorage
	defaultErr       error
	lookupCalls      int
	userDefaultAsked int
}

func (p *purgeStore) GetBackupStorage(id int) (*models.BackupStorage, error) {
	p.lookupCalls++
	if bs, ok := p.byID[id]; ok {
		return bs, nil
	}
	return nil, errors.New("no such storage")
}

func (p *purgeStore) GetDefaultBackupStorage() (*models.BackupStorage, error) {
	if p.defaultErr != nil {
		return nil, p.defaultErr
	}
	return p.platform, nil
}

func (p *purgeStore) GetUserDefaultBackupStorage(string) (*models.BackupStorage, error) {
	p.userDefaultAsked++
	return p.userDefault, nil
}

func ref(id int, key string, storageID *int, owner string) store.BackupRunRef {
	return store.BackupRunRef{RunID: id, StorageKey: key, StorageID: storageID, OwnerID: owner}
}

func intp(v int) *int { return &v }

func withFakeStorage(t *testing.T, f *fakeBackupStorage) {
	t.Helper()
	prev := openBackupStorage
	openBackupStorage = func(context.Context, *models.BackupStorage, backupstorage.Deps) (backupstorage.Storage, error) {
		return f, nil
	}
	t.Cleanup(func() { openBackupStorage = prev })
}

// The archives of a deleted schedule or server are the ones nothing will ever
// name again, so every one that CAN be deleted has to be.
func TestPurgeDeletesEveryArchiveOnOurStorage(t *testing.T) {
	f := &fakeBackupStorage{}
	withFakeStorage(t, f)
	owner := "tenant"
	platform := &models.BackupStorage{ID: 1, Name: "platform"}
	// The tenant has a default of their own, which a run that names no storage
	// must NOT be resolved into: it went to the platform default, and deleting
	// its key from their bucket succeeds against nothing.
	st := &purgeStore{
		byID:        map[int]*models.BackupStorage{1: platform},
		platform:    platform,
		userDefault: &models.BackupStorage{ID: 9, Name: "their-bucket", OwnerID: &owner},
	}

	deleted, kept := PurgeBackupArchives(context.Background(), st, backupstorage.Deps{}, []store.BackupRunRef{
		ref(1, "server-backups/a.tar.gz", intp(1), owner),
		ref(2, "server-backups/b.tar.gz", nil, owner), // predates the column: the platform default
	})
	if deleted != 2 || kept != 0 {
		t.Fatalf("deleted=%d kept=%d, want 2 and 0", deleted, kept)
	}
	if len(f.deleted) != 2 {
		t.Fatalf("objects deleted: %v", f.deleted)
	}
	if st.userDefaultAsked != 0 {
		t.Errorf("the tenant's default storage was consulted %d time(s); an archive that names no storage is on the platform default", st.userDefaultAsked)
	}
}

// A bucket the tenant connected is theirs. We pay nothing for it, and emptying
// it is not ours to do - the same rule the billing retention pass follows.
func TestPurgeLeavesATenantsOwnBucketAlone(t *testing.T) {
	f := &fakeBackupStorage{}
	withFakeStorage(t, f)
	owner := "tenant"
	theirs := &models.BackupStorage{ID: 7, Name: "their-b2", OwnerID: &owner}
	st := &purgeStore{byID: map[int]*models.BackupStorage{7: theirs}}

	deleted, kept := PurgeBackupArchives(context.Background(), st, backupstorage.Deps{}, []store.BackupRunRef{
		ref(1, "server-backups/a.tar.gz", intp(7), owner),
	})
	if deleted != 0 || kept != 1 {
		t.Fatalf("deleted=%d kept=%d, want 0 and 1", deleted, kept)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("their bucket was touched: %v", f.deleted)
	}
}

// A failure must not stop the rest: one unreachable archive is one leftover,
// not a whole delete's worth.
func TestPurgeKeepsGoingAfterAFailure(t *testing.T) {
	f := &fakeBackupStorage{failOn: "server-backups/bad.tar.gz"}
	withFakeStorage(t, f)
	platform := &models.BackupStorage{ID: 1, Name: "platform"}
	st := &purgeStore{byID: map[int]*models.BackupStorage{1: platform}, platform: platform}

	deleted, kept := PurgeBackupArchives(context.Background(), st, backupstorage.Deps{}, []store.BackupRunRef{
		ref(1, "server-backups/bad.tar.gz", intp(1), ""),
		ref(2, "server-backups/good.tar.gz", intp(1), ""),
		ref(3, "", intp(1), ""), // a run that never wrote one
	})
	if deleted != 1 || kept != 1 {
		t.Fatalf("deleted=%d kept=%d, want 1 and 1", deleted, kept)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "server-backups/good.tar.gz" {
		t.Fatalf("wrong object deleted: %v", f.deleted)
	}
}

// An unresolvable storage must never be answered by opening something else:
// deleting the key against the wrong bucket succeeds on S3 and removes nothing.
func TestPurgeKeepsAnArchiveItCannotPlace(t *testing.T) {
	f := &fakeBackupStorage{}
	withFakeStorage(t, f)
	st := &purgeStore{byID: map[int]*models.BackupStorage{}, defaultErr: errors.New("db down")}

	deleted, kept := PurgeBackupArchives(context.Background(), st, backupstorage.Deps{}, []store.BackupRunRef{
		ref(1, "server-backups/a.tar.gz", nil, ""),
	})
	if deleted != 0 || kept != 1 {
		t.Fatalf("deleted=%d kept=%d, want 0 and 1", deleted, kept)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("something was deleted although the storage was unknown: %v", f.deleted)
	}
}
