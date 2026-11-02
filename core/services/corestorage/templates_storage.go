package corestorage

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/storage"
)

var errTemplatePath = errors.New("template: invalid file path")

// templateRootKey is the template's folder inside its provider. The owner id is
// a path segment, so anything that could add or remove one is refused.
func templateRootKey(ownerID string, id int) (string, error) {
	if ownerID == "" || id <= 0 || strings.ContainsAny(ownerID, "/\\") || strings.ContainsRune(ownerID, 0) || ownerID == "." || ownerID == ".." {
		return "", fmt.Errorf("template: invalid owner %q or id %d", ownerID, id)
	}
	return ownerID + "/" + strconv.Itoa(id), nil
}

// TemplateStore resolves where a template's files live. The storage connection
// is read from the template row, not from settings, so a template keeps living
// where it was created when the operator changes the default later.
//
// A template on a tenant storage (BackupStorageID) is served from that bucket
// or not at all. Missing, foreign or non-s3 is an error and never falls back to
// Core storage: the bytes are not there, and writing them there would bill us
// for data the tenant chose to keep themselves.
func (s *Manager) TemplateStore(t *models.Template) (storage.StorageProvider, string, error) {
	root, err := templateRootKey(t.OwnerID, t.ID)
	if err != nil {
		return nil, "", err
	}
	if t.BackupStorageID != nil {
		bs, err := s.Store.GetBackupStorage(*t.BackupStorageID)
		if err != nil {
			return nil, "", fmt.Errorf("template %d: storage %d could not be loaded: %w", t.ID, *t.BackupStorageID, err)
		}
		if bs == nil || bs.Provider != "s3" || bs.OwnerID == nil || *bs.OwnerID != t.OwnerID {
			return nil, "", fmt.Errorf("template %d: storage %d is missing or not the owner's", t.ID, *t.BackupStorageID)
		}
		prov, err := storage.NewTenantS3Provider(context.Background(), bs.Config, PrefixTemplates)
		if err != nil {
			return nil, "", fmt.Errorf("template %d: storage %q: %w", t.ID, bs.Name, err)
		}
		return prov, root, nil
	}
	if t.StorageConnectionID == 0 {
		prov, err := s.BuildProvider(PrefixTemplates)
		if err != nil {
			return nil, "", fmt.Errorf("template %d: core storage: %w", t.ID, err)
		}
		return prov, root, nil
	}
	conn, err := s.Store.GetStorageConnection(t.StorageConnectionID)
	if err != nil {
		return nil, "", fmt.Errorf("template %d: storage connection %d could not be loaded: %w", t.ID, t.StorageConnectionID, err)
	}
	cfg := ConfigFromConnection(conn)
	if err := Validate(cfg); err != nil {
		return nil, "", fmt.Errorf("template %d: storage connection %q is not usable: %w", t.ID, conn.Name, err)
	}
	prov, err := NewProviderForConfig(cfg, PrefixTemplates, s.Gate, s.S3)
	if err != nil {
		return nil, "", fmt.Errorf("template %d: storage connection %q: %w", t.ID, conn.Name, err)
	}
	return prov, root, nil
}

// TemplateStorageReady reports whether a template created now by ownerID, on
// the owner's storage backupStorageID or else on storage connection connID
// (0 = Core file storage), could have its files written. It builds the same
// provider TemplateStore would, so the answer is the real one.
func (s *Manager) TemplateStorageReady(ownerID string, connID int, backupStorageID *int) error {
	_, _, err := s.TemplateStore(&models.Template{OwnerID: ownerID, ID: 1, StorageConnectionID: connID, BackupStorageID: backupStorageID})
	return err
}

// TemplateStorageNeverConfigured is true only when the template lives on Core
// file storage and that was never set up (no backend saved). No file can exist
// then, so the row may be deleted without touching storage. A transient error
// (settings read failure, unreachable mount, broken connection) is NOT this:
// files may exist and the row must stay.
func (s *Manager) TemplateStorageNeverConfigured(t *models.Template) bool {
	if t.BackupStorageID != nil || t.StorageConnectionID != 0 {
		return false
	}
	cfg, err := s.EffectiveConfig()
	return err == nil && cfg.Backend == ""
}

// TemplateList returns every file; FileInfo.Name is the path relative to the
// template root.
func (s *Manager) TemplateList(ctx context.Context, t *models.Template) ([]storage.FileInfo, error) {
	prov, root, err := s.TemplateStore(t)
	if err != nil {
		return nil, err
	}
	return templateList(ctx, prov, root)
}

// TemplateWrite stores one file. size is accepted for call-site symmetry; the
// provider streams until EOF.
func (s *Manager) TemplateWrite(ctx context.Context, t *models.Template, rel string, r io.Reader, _ int64) error {
	prov, root, err := s.TemplateStore(t)
	if err != nil {
		return err
	}
	return templateWrite(ctx, prov, root, rel, r)
}

func (s *Manager) TemplateRead(ctx context.Context, t *models.Template, rel string) (io.ReadCloser, error) {
	prov, root, err := s.TemplateStore(t)
	if err != nil {
		return nil, err
	}
	return templateRead(ctx, prov, root, rel)
}

// TemplateDelete removes one file or folder; rel == "" removes the whole template.
func (s *Manager) TemplateDelete(ctx context.Context, t *models.Template, rel string) error {
	prov, root, err := s.TemplateStore(t)
	if err != nil {
		return err
	}
	return templateDelete(ctx, prov, root, rel)
}

func (s *Manager) TemplateSize(ctx context.Context, t *models.Template) (int64, error) {
	prov, root, err := s.TemplateStore(t)
	if err != nil {
		return 0, err
	}
	return templateSize(ctx, prov, root)
}

func (s *Manager) TemplateZip(ctx context.Context, t *models.Template, w io.Writer) error {
	return s.TemplateZipWith(ctx, t, w, nil)
}

// TemplateZipWith is TemplateZip plus extra entries written at the zip root.
// An extra entry replaces a stored file of the same name.
func (s *Manager) TemplateZipWith(ctx context.Context, t *models.Template, w io.Writer, extra map[string][]byte) error {
	prov, root, err := s.TemplateStore(t)
	if err != nil {
		return err
	}
	return templateZip(ctx, prov, root, w, extra)
}

// The helpers below take the provider so they are testable without a Core
// storage config (which must be an absolute Linux path).

func templateList(ctx context.Context, prov storage.StorageProvider, root string) ([]storage.FileInfo, error) {
	files, err := storage.WalkProvider(ctx, prov, root)
	if err != nil {
		return nil, fmt.Errorf("template: list: %w", err)
	}
	out := make([]storage.FileInfo, 0, len(files))
	for _, f := range files {
		out = append(out, storage.FileInfo{Name: f.Key, Size: f.Size, Enabled: true})
	}
	return out, nil
}

func templateWrite(ctx context.Context, prov storage.StorageProvider, root, rel string, r io.Reader) error {
	clean, ok := services.TemplateRelPath(rel)
	if !ok {
		return fmt.Errorf("%w: %q", errTemplatePath, rel)
	}
	return prov.WriteFile(ctx, root+"/"+clean, r)
}

func templateRead(ctx context.Context, prov storage.StorageProvider, root, rel string) (io.ReadCloser, error) {
	clean, ok := services.TemplateRelPath(rel)
	if !ok {
		return nil, fmt.Errorf("%w: %q", errTemplatePath, rel)
	}
	return prov.GetFile(ctx, root+"/"+clean)
}

func templateDelete(ctx context.Context, prov storage.StorageProvider, root, rel string) error {
	if rel == "" {
		return prov.DeletePath(ctx, root)
	}
	clean, ok := services.TemplateRelPath(rel)
	if !ok {
		return fmt.Errorf("%w: %q", errTemplatePath, rel)
	}
	return prov.DeletePath(ctx, root+"/"+clean)
}

func templateSize(ctx context.Context, prov storage.StorageProvider, root string) (int64, error) {
	files, err := templateList(ctx, prov, root)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	return total, nil
}

func templateZip(ctx context.Context, prov storage.StorageProvider, root string, w io.Writer, extra map[string][]byte) error {
	files, err := templateList(ctx, prov, root)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	for name, data := range extra {
		ew, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			return fmt.Errorf("template: zip entry %q: %w", name, err)
		}
		if _, err := ew.Write(data); err != nil {
			return fmt.Errorf("template: zip copy %q: %w", name, err)
		}
	}
	for _, f := range files {
		if _, ok := extra[f.Name]; ok {
			continue
		}
		ew, err := zw.CreateHeader(&zip.FileHeader{Name: f.Name, Method: zip.Deflate})
		if err != nil {
			return fmt.Errorf("template: zip entry %q: %w", f.Name, err)
		}
		rc, err := prov.GetFile(ctx, root+"/"+f.Name)
		if err != nil {
			return fmt.Errorf("template: read %q: %w", f.Name, err)
		}
		_, err = io.Copy(ew, rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("template: zip copy %q: %w", f.Name, err)
		}
	}
	return zw.Close()
}

// TemplatesOfUser reads the account's templates before the user row goes:
// the rows cascade with it, and afterwards nothing says where their files were.
func (s *Manager) TemplatesOfUser(userID string) []models.Template {
	ids, err := s.Store.ListTemplateIDsByOwner(userID)
	if err != nil {
		log.Printf("templates: ids of %s: %v", userID, err)
		return nil
	}
	var out []models.Template
	for _, id := range ids {
		t, err := s.Store.GetTemplate(id)
		if err != nil || t == nil {
			log.Printf("templates: read %d of %s: %v", id, userID, err)
			continue
		}
		out = append(out, *t)
	}
	return out
}

// DropTemplateFiles removes the files of templates whose rows are gone.
// Failures are logged: the account is already deleted and must stay so.
// Templates on the tenant's own storage are skipped: that bucket is never ours
// to delete from, and its storage row cascaded with the account anyway.
func (s *Manager) DropTemplateFiles(ctx context.Context, ts []models.Template) {
	for i := range ts {
		if ts[i].BackupStorageID != nil {
			continue
		}
		if err := s.TemplateDelete(ctx, &ts[i], ""); err != nil {
			log.Printf("templates: drop files of %d: %v", ts[i].ID, err)
		}
	}
}
