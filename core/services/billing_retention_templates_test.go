package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

type templateRetentionStore struct {
	store.Store

	suspended []store.UserBilling
	ids       map[string][]int
	rows      map[int]*models.Template
	idsErr    error
	deleted   []int
}

func (f *templateRetentionStore) ListUserBillingByStatus(string) ([]store.UserBilling, error) {
	return f.suspended, nil
}
func (f *templateRetentionStore) ListBackupRunsByOwner(string) ([]store.BackupRunRef, error) {
	return nil, nil
}
func (f *templateRetentionStore) GetSetting(string) (string, error) { return "", nil }
func (f *templateRetentionStore) ListTemplateIDsByOwner(owner string) ([]int, error) {
	return f.ids[owner], f.idsErr
}
func (f *templateRetentionStore) GetTemplate(id int) (*models.Template, error) {
	return f.rows[id], nil
}
func (f *templateRetentionStore) DeleteTemplate(id int) error {
	f.deleted = append(f.deleted, id)
	return nil
}

// Only templates on our storage go, files first; a failed file delete keeps
// the row so the next pass retries, and a tenant's own storage is never asked.
func TestDeleteTenantTemplates(t *testing.T) {
	tenant := 4
	fs := &templateRetentionStore{
		ids: map[string][]int{"u1": {1, 2, 3, 4}},
		rows: map[int]*models.Template{
			1: {ID: 1, OwnerID: "u1", Visibility: "personal"},
			2: {ID: 2, OwnerID: "u1", Visibility: "personal", BackupStorageID: &tenant},
			3: {ID: 3, OwnerID: "u1", Visibility: "personal", StorageConnectionID: 5},
			// 4 vanished between the list and the read.
		},
	}
	var dropped []int
	svc := &BillingLifecycleService{store: fs}
	svc.SetTemplateRetention(func(_ context.Context, t *models.Template) error {
		dropped = append(dropped, t.ID)
		if t.ID == 3 {
			return errors.New("storage down")
		}
		return nil
	})

	svc.deleteTenantTemplates(context.Background(), "u1")

	if !equalInts(dropped, []int{1, 3}) {
		t.Errorf("files dropped for %v, want [1 3]", dropped)
	}
	if !equalInts(fs.deleted, []int{1}) {
		t.Errorf("rows deleted %v, want [1]", fs.deleted)
	}
}

// Admin and platform templates are platform assets: an expired suspended
// creator loses their personal templates only.
func TestDeleteTenantTemplatesKeepsAdminAndPlatformTemplates(t *testing.T) {
	fs := &templateRetentionStore{
		ids: map[string][]int{"u1": {1, 2, 3}},
		rows: map[int]*models.Template{
			1: {ID: 1, OwnerID: "u1", Visibility: "personal"},
			2: {ID: 2, OwnerID: "u1", Visibility: "admin"},
			3: {ID: 3, OwnerID: "u1", Visibility: "platform"},
		},
	}
	var dropped []int
	svc := &BillingLifecycleService{store: fs}
	svc.SetTemplateRetention(func(_ context.Context, t *models.Template) error {
		dropped = append(dropped, t.ID)
		return nil
	})
	svc.deleteTenantTemplates(context.Background(), "u1")
	if !equalInts(dropped, []int{1}) || !equalInts(fs.deleted, []int{1}) {
		t.Errorf("dropped %v, deleted %v; want only [1]", dropped, fs.deleted)
	}
}

func TestDeleteTenantTemplatesUnwiredDoesNothing(t *testing.T) {
	fs := &templateRetentionStore{ids: map[string][]int{"u1": {1}}, rows: map[int]*models.Template{1: {ID: 1, OwnerID: "u1", Visibility: "personal"}}}
	(&BillingLifecycleService{store: fs}).deleteTenantTemplates(context.Background(), "u1")
	if len(fs.deleted) != 0 {
		t.Errorf("rows deleted without a file closure: %v", fs.deleted)
	}
}

// Template retention runs on the backup retention clock: not during an admin
// hold, not before the deadline, not without a suspension date.
func TestCleanupExpiredR2DeletesTemplatesOnTheSameClock(t *testing.T) {
	old := timePtr(time.Now().Add(-30 * 24 * time.Hour))
	recent := timePtr(time.Now().Add(-time.Hour))
	cases := []struct {
		name string
		b    store.UserBilling
		want bool
	}{
		{"expired", store.UserBilling{UserID: "u1", Status: "suspended", SuspendedAt: old, R2Retention: "1d"}, true},
		{"admin hold", store.UserBilling{UserID: "u1", Status: "suspended", SuspendedAt: old, R2Retention: "1d", AdminHold: true}, false},
		{"not yet expired", store.UserBilling{UserID: "u1", Status: "suspended", SuspendedAt: recent, R2Retention: "1d"}, false},
		{"no suspension date", store.UserBilling{UserID: "u1", Status: "suspended", R2Retention: "1d"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &templateRetentionStore{
				suspended: []store.UserBilling{tc.b},
				ids:       map[string][]int{"u1": {1}},
				rows:      map[int]*models.Template{1: {ID: 1, OwnerID: "u1", Visibility: "personal"}},
			}
			svc := &BillingLifecycleService{store: fs}
			svc.SetTemplateRetention(func(context.Context, *models.Template) error { return nil })
			svc.cleanupExpiredR2(context.Background())
			if got := len(fs.deleted) == 1; got != tc.want {
				t.Errorf("template deleted = %v, want %v", got, tc.want)
			}
		})
	}
}
