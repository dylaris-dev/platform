package services

import (
	"errors"
	"testing"

	"dylaris-core/models"
)

type fakeBackupStorageStore struct {
	byID    map[int]*models.BackupStorage
	def     *models.BackupStorage
	userDef map[string]*models.BackupStorage
	prefs   map[string]int // user -> backups_storage_id
	byIDErr error
	defErr  error
}

func (f fakeBackupStorageStore) GetBackupStorage(id int) (*models.BackupStorage, error) {
	if f.byIDErr != nil {
		return nil, f.byIDErr
	}
	return f.byID[id], nil
}

func (f fakeBackupStorageStore) GetDefaultBackupStorage() (*models.BackupStorage, error) {
	if f.defErr != nil {
		return nil, f.defErr
	}
	return f.def, nil
}

func (f fakeBackupStorageStore) GetUserDefaultBackupStorage(ownerID string) (*models.BackupStorage, error) {
	return f.userDef[ownerID], nil
}

func (f fakeBackupStorageStore) GetUserStoragePrefs(userID string) (*int, *int, error) {
	if id, ok := f.prefs[userID]; ok {
		return &id, nil, nil
	}
	return nil, nil, nil
}

func intPtr(i int) *int { return &i }

// The user's backups preference sits between the job and the tenant default,
// and is held to the same ownership rule as a storage the job names.
func TestResolveJobStoragePrefersThePreference(t *testing.T) {
	alice, bob := "alice", "bob"
	byID := map[int]*models.BackupStorage{
		5: {ID: 5, Name: "alice pref", OwnerID: &alice},
		6: {ID: 6, Name: "alice default", OwnerID: &alice, IsDefault: true},
		8: {ID: 8, Name: "bob's", OwnerID: &bob},
	}
	def := &models.BackupStorage{ID: 1, Name: "platform default"}
	tests := []struct {
		name    string
		prefs   map[string]int
		owner   string
		job     *int
		wantID  int
		wantErr error
	}{
		{"pref beats tenant default", map[string]int{alice: 5}, alice, nil, 5, nil},
		{"no pref keeps tenant default", nil, alice, nil, 6, nil},
		{"job storage beats pref", map[string]int{alice: 5}, alice, intPtr(6), 6, nil},
		{"foreign pref refused", map[string]int{alice: 8}, alice, nil, 0, ErrForeignBackupStorage},
		{"vanished pref refused", map[string]int{alice: 99}, alice, nil, 0, ErrNoBackupStorage},
		{"no owner skips prefs", map[string]int{"": 5}, "", nil, 1, nil},
	}
	for _, tc := range tests {
		s := fakeBackupStorageStore{byID: byID, def: def, prefs: tc.prefs,
			userDef: map[string]*models.BackupStorage{alice: byID[6]}}
		got, err := ResolveJobStorage(s, tc.job, tc.owner)
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("%s: err = %v, want %v", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got == nil || got.ID != tc.wantID {
			t.Errorf("%s: got %+v, %v; want storage %d", tc.name, got, err, tc.wantID)
		}
	}
}

// TestResolveJobStorage pins the contract the two halves of the backup feature
// disagreed on. The run path fell back to the default storage when a job had
// none; restore, download and the archive check refused outright. Since the
// panel's storage dropdown offers "Default storage" FIRST and stores NULL, a
// job created by not choosing produced backups that could not be restored,
// downloaded or verified - which is only discovered while recovering.
func TestResolveJobStorage(t *testing.T) {
	own := &models.BackupStorage{ID: 7, Name: "job storage"}
	def := &models.BackupStorage{ID: 1, Name: "default storage"}

	t.Run("job storage wins when set", func(t *testing.T) {
		s := fakeBackupStorageStore{byID: map[int]*models.BackupStorage{7: own}, def: def}
		got, err := ResolveJobStorage(s, intPtr(7), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ID != 7 {
			t.Errorf("got storage %d, want the job's own (7)", got.ID)
		}
	})

	// The case that shipped broken.
	t.Run("nil storage falls back to the default", func(t *testing.T) {
		s := fakeBackupStorageStore{byID: map[int]*models.BackupStorage{}, def: def}
		got, err := ResolveJobStorage(s, nil, "")
		if err != nil {
			t.Fatalf("a job on the default storage was refused: %v", err)
		}
		if got.ID != 1 {
			t.Errorf("got storage %d, want the default (1)", got.ID)
		}
	})

	// Refusing IS correct here, and the message has to stay distinguishable
	// from "the id you gave does not resolve".
	t.Run("nil storage and no default is the one real refusal", func(t *testing.T) {
		s := fakeBackupStorageStore{byID: map[int]*models.BackupStorage{}}
		if _, err := ResolveJobStorage(s, nil, ""); !errors.Is(err, ErrNoBackupStorage) {
			t.Errorf("err = %v, want ErrNoBackupStorage", err)
		}
	})

	t.Run("a storage id that no longer exists is not silently defaulted", func(t *testing.T) {
		s := fakeBackupStorageStore{byID: map[int]*models.BackupStorage{}, def: def}
		if _, err := ResolveJobStorage(s, intPtr(99), ""); !errors.Is(err, ErrNoBackupStorage) {
			t.Errorf("err = %v, want ErrNoBackupStorage", err)
		}
	})

	t.Run("lookup errors propagate", func(t *testing.T) {
		boom := errors.New("db down")
		if _, err := ResolveJobStorage(fakeBackupStorageStore{byIDErr: boom}, intPtr(7), ""); !errors.Is(err, boom) {
			t.Errorf("by-id err = %v, want the db error", err)
		}
		if _, err := ResolveJobStorage(fakeBackupStorageStore{defErr: boom}, nil, ""); !errors.Is(err, boom) {
			t.Errorf("default err = %v, want the db error", err)
		}
	})
}
