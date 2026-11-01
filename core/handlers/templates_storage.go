package handlers

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"dylaris-core/authz"
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
func (s *AppState) TemplateStore(t *models.Template) (storage.StorageProvider, string, error) {
	root, err := templateRootKey(t.OwnerID, t.ID)
	if err != nil {
		return nil, "", err
	}
	if t.StorageConnectionID == 0 {
		prov, err := s.buildCoreStorageProvider(CoreStoragePrefixTemplates)
		if err != nil {
			return nil, "", fmt.Errorf("template %d: core storage: %w", t.ID, err)
		}
		return prov, root, nil
	}
	conn, err := s.Store.GetStorageConnection(t.StorageConnectionID)
	if err != nil {
		return nil, "", fmt.Errorf("template %d: storage connection %d could not be loaded: %w", t.ID, t.StorageConnectionID, err)
	}
	cfg := coreStorageConfigFromConnection(conn)
	if err := validateCoreStorageConfig(cfg); err != nil {
		return nil, "", fmt.Errorf("template %d: storage connection %q is not usable: %w", t.ID, conn.Name, err)
	}
	prov, err := newStorageProviderForConfig(cfg, CoreStoragePrefixTemplates, s.StorageGate, s.StorageS3)
	if err != nil {
		return nil, "", fmt.Errorf("template %d: storage connection %q: %w", t.ID, conn.Name, err)
	}
	return prov, root, nil
}

// TemplateStorageReady reports whether a template created now, on storage
// connection connID (0 = Core file storage), could have its files written. It
// builds the same provider TemplateStore would, so the answer is the real one.
func (s *AppState) TemplateStorageReady(connID int) error {
	_, _, err := s.TemplateStore(&models.Template{OwnerID: "probe", ID: 1, StorageConnectionID: connID})
	return err
}

// TemplateStorageNeverConfigured is true only when the template lives on Core
// file storage and that was never set up (no backend saved). No file can exist
// then, so the row may be deleted without touching storage. A transient error
// (settings read failure, unreachable mount, broken connection) is NOT this:
// files may exist and the row must stay.
func (s *AppState) TemplateStorageNeverConfigured(t *models.Template) bool {
	if t.StorageConnectionID != 0 {
		return false
	}
	cfg, err := s.effectiveCoreStorageConfig()
	return err == nil && cfg.Backend == ""
}

// TemplateList returns every file; FileInfo.Name is the path relative to the
// template root.
func (s *AppState) TemplateList(ctx context.Context, t *models.Template) ([]storage.FileInfo, error) {
	prov, root, err := s.TemplateStore(t)
	if err != nil {
		return nil, err
	}
	return templateList(ctx, prov, root)
}

// TemplateWrite stores one file. size is accepted for call-site symmetry; the
// provider streams until EOF.
func (s *AppState) TemplateWrite(ctx context.Context, t *models.Template, rel string, r io.Reader, _ int64) error {
	prov, root, err := s.TemplateStore(t)
	if err != nil {
		return err
	}
	return templateWrite(ctx, prov, root, rel, r)
}

func (s *AppState) TemplateRead(ctx context.Context, t *models.Template, rel string) (io.ReadCloser, error) {
	prov, root, err := s.TemplateStore(t)
	if err != nil {
		return nil, err
	}
	return templateRead(ctx, prov, root, rel)
}

// TemplateDelete removes one file or folder; rel == "" removes the whole template.
func (s *AppState) TemplateDelete(ctx context.Context, t *models.Template, rel string) error {
	prov, root, err := s.TemplateStore(t)
	if err != nil {
		return err
	}
	return templateDelete(ctx, prov, root, rel)
}

func (s *AppState) TemplateSize(ctx context.Context, t *models.Template) (int64, error) {
	prov, root, err := s.TemplateStore(t)
	if err != nil {
		return 0, err
	}
	return templateSize(ctx, prov, root)
}

func (s *AppState) TemplateZip(ctx context.Context, t *models.Template, w io.Writer) error {
	prov, root, err := s.TemplateStore(t)
	if err != nil {
		return err
	}
	return templateZip(ctx, prov, root, w)
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

func templateZip(ctx context.Context, prov storage.StorageProvider, root string, w io.Writer) error {
	files, err := templateList(ctx, prov, root)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	for _, f := range files {
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
func (s *AppState) TemplatesOfUser(userID string) []models.Template {
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
func (s *AppState) DropTemplateFiles(ctx context.Context, ts []models.Template) {
	for i := range ts {
		if err := s.TemplateDelete(ctx, &ts[i], ""); err != nil {
			log.Printf("templates: drop files of %d: %v", ts[i].ID, err)
		}
	}
}

// TemplateViewer resolves who is asking about a template, for the access rules
// in services (TemplateCanSee and siblings). t may be nil (list, create); the
// share level is only looked up for a concrete template.
func (s *AppState) TemplateViewer(r *http.Request, t *models.Template) (services.TemplateViewer, error) {
	id := authz.IdentityFromContext(r.Context())
	v := services.TemplateViewer{UserID: id.UserID, IsAdmin: id.IsAdmin}
	if s.Authz != nil {
		if res, err := s.Authz.Resolve(id, 0); err == nil {
			v.HasAdminRead = res.HasCap("templates.admin.read")
			v.HasCreate = res.HasCap("templates.create")
		}
	}
	if t != nil && v.UserID != "" && t.OwnerID != v.UserID {
		level, err := s.Store.TemplateShareLevel(t.ID, v.UserID)
		if err != nil {
			return v, err
		}
		v.ShareLevel = level
	}
	return v, nil
}
