package corestorage

import (
	"context"
	"fmt"

	"dylaris-core/models"
	"dylaris-core/storage"
)

// RetentionDropTemplateFiles deletes a template's files for billing retention.
// The caller already skips a template on the tenant's own storage; this refuses
// one too, so no wiring mistake can reach a tenant bucket. Storage that was
// never configured holds no files, so that counts as done.
func (s *Manager) RetentionDropTemplateFiles(ctx context.Context, t *models.Template) error {
	if t.BackupStorageID != nil {
		return fmt.Errorf("template %d is on the owner's own storage", t.ID)
	}
	if err := s.TemplateDelete(ctx, t, ""); err != nil && !s.TemplateStorageNeverConfigured(t) {
		return err
	}
	return nil
}

// TemplateJanitorStorage opens our template storage for the janitor: Core
// storage for connID 0, else that storage connection, scoped to the templates
// area exactly as TemplateStore scopes it. nil, nil when Core storage was never
// configured. There is no way to name a tenant storage here.
func (s *Manager) TemplateJanitorStorage(connID int) (storage.StorageProvider, error) {
	probe := &models.Template{OwnerID: "janitor", ID: 1, StorageConnectionID: connID}
	if s.TemplateStorageNeverConfigured(probe) {
		return nil, nil
	}
	prov, _, err := s.TemplateStore(probe)
	return prov, err
}
