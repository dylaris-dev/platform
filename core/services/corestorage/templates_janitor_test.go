package corestorage

import (
	"context"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// noCallStore panics on any call (nil embedded interface): the paths below
// must decide on the row alone, without loading or opening a tenant storage.
type noCallStore struct{ store.Store }

func TestRetentionAndAccountDeleteNeverTouchTenantStorage(t *testing.T) {
	tenant := 4
	s := &Manager{Store: noCallStore{}}
	tpl := models.Template{ID: 7, OwnerID: "u1", BackupStorageID: &tenant}

	if err := s.RetentionDropTemplateFiles(context.Background(), &tpl); err == nil {
		t.Error("retention accepted a template on the owner's own storage")
	}
	s.DropTemplateFiles(context.Background(), []models.Template{tpl})
}
