package services

import (
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// allowanceStore: a 1 GiB allowance, usedBytes on our storage.
type allowanceStore struct {
	store.Store
	used int64
}

func (f *allowanceStore) GetUserByID(id string) (*models.User, error) {
	return &models.User{ID: id}, nil
}
func (f *allowanceStore) GetUserBilling(string) (*store.UserBilling, error) {
	q := int64(1)
	return &store.UserBilling{R2QuotaGB: &q}, nil
}
func (f *allowanceStore) BackupBytesByOwner(string) (int64, error) { return f.used, nil }

// Since Core's completed uploads count in the owner's usage, a retried
// completion of the same run met its own recorded size in "used" and counted
// it twice: an archive that fits was deleted as over the allowance.
func TestARetriedCompletionDoesNotCountItselfTwice(t *testing.T) {
	const gib = int64(1) << 30
	size := gib / 2
	tr := &BackupTransfer{store: &allowanceStore{used: gib/4 + size}}
	recorded := &runUpload{run: &models.BackupRun{ID: 1, UploadedBytes: &size}, owner: "o"}
	if tr.exceedsAllowance(recorded, size) {
		t.Fatal("a completed upload of 512 MiB on 256 MiB used was refused against 1 GiB")
	}
	fresh := &runUpload{run: &models.BackupRun{ID: 2}, owner: "o"}
	if !tr.exceedsAllowance(fresh, size) {
		t.Fatal("a second 512 MiB upload over 768 MiB used was accepted against 1 GiB")
	}
}
