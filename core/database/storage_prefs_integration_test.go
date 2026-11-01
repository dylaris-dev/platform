package database

import (
	"errors"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

func TestIntegrationUserStoragePrefs(t *testing.T) {
	db, st := integrationDB(t)
	// A bare user, so the cascade below can delete it without the fixture's
	// server and node in the way.
	var userID string
	if err := db.QueryRow(`INSERT INTO users (username, password, role) VALUES ($1, 'x', 'user') RETURNING id`,
		uniqueName("pref_u_")).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, userID) })

	own, err := st.CreateBackupStorage(s3Storage(uniqueName("pref_own_"), &userID, false))
	if err != nil {
		t.Fatalf("own storage: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM backup_storages WHERE id = $1`, own) })

	// No row yet: both unset.
	if b, tp, err := st.GetUserStoragePrefs(userID); err != nil || b != nil || tp != nil {
		t.Fatalf("empty prefs = %v, %v, %v; want nil, nil, nil", b, tp, err)
	}

	tests := []struct {
		name               string
		backups, templates *int
	}{
		{"both own", &own, &own},
		{"backups only", &own, nil},
		{"templates only", nil, &own},
		{"back to ours", nil, nil},
	}
	for _, tc := range tests {
		if err := st.SetUserBackupsStoragePref(userID, tc.backups); err != nil {
			t.Fatalf("%s: set backups: %v", tc.name, err)
		}
		if err := st.SetUserTemplatesStoragePref(userID, tc.templates); err != nil {
			t.Fatalf("%s: set templates: %v", tc.name, err)
		}
		b, tp, err := st.GetUserStoragePrefs(userID)
		if err != nil {
			t.Fatalf("%s: get: %v", tc.name, err)
		}
		if !sameIntPtr(b, tc.backups) || !sameIntPtr(tp, tc.templates) {
			t.Errorf("%s: got %v/%v, want %v/%v", tc.name, b, tp, tc.backups, tc.templates)
		}
	}

	// Each setter writes its own column only.
	if err := st.SetUserBackupsStoragePref(userID, &own); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUserTemplatesStoragePref(userID, &own); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUserTemplatesStoragePref(userID, nil); err != nil {
		t.Fatal(err)
	}
	if b, tp, err := st.GetUserStoragePrefs(userID); err != nil || !sameIntPtr(b, &own) || tp != nil {
		t.Fatalf("templates write touched backups: %v/%v, %v", b, tp, err)
	}
	// Clearing only applies while the choice still names the storage.
	if err := st.ClearUserBackupsStoragePrefIf(userID, own+1); err != nil {
		t.Fatal(err)
	}
	if b, _, _ := st.GetUserStoragePrefs(userID); !sameIntPtr(b, &own) {
		t.Fatalf("clear of another id dropped the choice: %v", b)
	}
	if err := st.ClearUserBackupsStoragePrefIf(userID, own); err != nil {
		t.Fatal(err)
	}
	if b, _, _ := st.GetUserStoragePrefs(userID); b != nil {
		t.Fatalf("clear of the chosen id kept it: %v", b)
	}

	// A preference pins its storage: in use, and the delete is refused.
	if err := st.SetUserTemplatesStoragePref(userID, &own); err != nil {
		t.Fatal(err)
	}
	if used, err := st.BackupStorageInUse(own); err != nil || !used {
		t.Fatalf("storage named by a template pref: in use = %v, %v; want true", used, err)
	}
	if err := st.DeleteBackupStorage(own); !errors.Is(err, store.ErrStorageInUse) {
		t.Fatalf("delete of a preferred storage: err = %v, want ErrStorageInUse", err)
	}
	// ON DELETE RESTRICT holds even past the store's check.
	if _, err := db.Exec(`DELETE FROM backup_storages WHERE id = $1`, own); err == nil {
		t.Fatal("raw delete of a preferred storage succeeded; want a foreign key refusal")
	}

	// Deleting the account takes the prefs, a template on the storage and the
	// storage itself in one statement. The references must not block that
	// cascade: account deletion has to succeed while they still point at it.
	if err := st.SetUserBackupsStoragePref(userID, &own); err != nil {
		t.Fatal(err)
	}
	tpl := &models.Template{OwnerID: userID, Name: uniqueName("tpl_"), Kind: "server", BackupStorageID: &own}
	if err := st.CreateTemplate(tpl); err != nil {
		t.Fatalf("template on own storage: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM user_storage_prefs WHERE user_id = $1)
		+ (SELECT COUNT(*) FROM backup_storages WHERE id = $2)`, userID, own).Scan(&n); err != nil || n != 0 {
		t.Fatalf("prefs and storage rows after user delete = %d, %v; want 0", n, err)
	}
}

func TestIntegrationTemplateOnTenantStorage(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)

	own, err := st.CreateBackupStorage(s3Storage(uniqueName("tpl_own_"), &f.user.ID, false))
	if err != nil {
		t.Fatalf("own storage: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM backup_storages WHERE id = $1`, own) })

	tpl := &models.Template{OwnerID: f.user.ID, Name: uniqueName("tpl_"), Kind: "server", BackupStorageID: &own}
	if err := st.CreateTemplate(tpl); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { st.DeleteTemplate(tpl.ID) })
	got, err := st.GetTemplate(tpl.ID)
	if err != nil || got == nil || got.BackupStorageID == nil || *got.BackupStorageID != own {
		t.Fatalf("GetTemplate = %+v, %v; want backupStorageId %d", got, err, own)
	}
	if used, err := st.BackupStorageInUse(own); err != nil || !used {
		t.Fatalf("storage holding a template: in use = %v, %v; want true", used, err)
	}
	if err := st.DeleteBackupStorage(own); !errors.Is(err, store.ErrStorageInUse) {
		t.Fatalf("delete of a storage holding a template: err = %v, want ErrStorageInUse", err)
	}

	// A template on our storage stays NULL.
	ours := &models.Template{OwnerID: f.user.ID, Name: uniqueName("tpl_"), Kind: "server"}
	if err := st.CreateTemplate(ours); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DeleteTemplate(ours.ID) })
	if got, err := st.GetTemplate(ours.ID); err != nil || got == nil || got.BackupStorageID != nil {
		t.Fatalf("template on our storage = %+v, %v; want nil backupStorageId", got, err)
	}
}

// The phase copies a tenant's own default into the backups pref, once: a row
// already there (the user's later choice) is never overwritten.
func TestIntegrationStoragePrefsBackfill(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)

	def, err := st.CreateBackupStorage(s3Storage(uniqueName("bf_def_"), &f.user.ID, true))
	if err != nil {
		t.Fatalf("default storage: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM user_storage_prefs WHERE user_id = $1`, f.user.ID)
		db.Exec(`DELETE FROM backup_storages WHERE id = $1`, def)
	})

	if err := applyStoragePrefs(db); err != nil {
		t.Fatalf("phase: %v", err)
	}
	b, tp, err := st.GetUserStoragePrefs(f.user.ID)
	if err != nil || b == nil || *b != def || tp != nil {
		t.Fatalf("after backfill = %v/%v, %v; want %d/nil", b, tp, err, def)
	}

	// The user picks our storage; a second boot must not undo it. Choosing ours
	// also clears the default flag, which a NULL pref would fall back to.
	if err := st.SetUserBackupsStoragePref(f.user.ID, nil); err != nil {
		t.Fatal(err)
	}
	if bs, err := st.GetUserDefaultBackupStorage(f.user.ID); err != nil || bs != nil {
		t.Fatalf("own default after choosing ours = %+v, %v; want none", bs, err)
	}
	if err := st.SetUserBackupsStoragePref(f.user.ID, &def); err != nil {
		t.Fatal(err)
	}
	if bs, err := st.GetUserDefaultBackupStorage(f.user.ID); err != nil || bs == nil || bs.ID != def {
		t.Fatalf("own default after choosing it = %+v, %v; want %d", bs, err, def)
	}
	if err := st.SetUserBackupsStoragePref(f.user.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := applyStoragePrefs(db); err != nil {
		t.Fatalf("phase again: %v", err)
	}
	if b, _, err := st.GetUserStoragePrefs(f.user.ID); err != nil || b != nil {
		t.Fatalf("after second boot = %v, %v; want the user's nil kept", b, err)
	}
}

func sameIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
