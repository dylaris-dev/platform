package templates

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"strings"
	"unicode/utf8"

	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/storage"
)

// Skipped is a file left out of a template, with why.
type Skipped struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

const (
	maxTextFileBytes = 1 << 20
	// uploadSlack is what a request body may carry beyond the size limit: zip
	// and multipart headers, and the stored (uncompressed) zip entries.
	uploadSlack = 16 << 20
	// maxUploadBody bounds a body when the size limit is "unlimited": the body
	// is spooled to Core's disk before anything else looks at it, so "no limit
	// on a template" must not become "no limit on what one request may write".
	maxUploadBody = 8 << 30
	// maxZipEntries bounds an archive before any entry is touched. The size
	// limit counts bytes, not files: a few MiB of empty entries is hundreds of
	// thousands of storage writes and a walk per refresh. 20000 is far above a
	// real server folder (a large modded world is a few thousand files).
	maxZipEntries = 20000
	// maxSkippedListed caps the skipped list in a response; the rest is a count.
	maxSkippedListed = 200
)

var (
	errTooLarge       = errors.New("template: size limit exceeded")
	errBadZip         = errors.New("template: unreadable zip")
	errTooManyEntries = errors.New("template: too many archive entries")
)

// skipList collects skipped files, listing at most maxSkippedListed of them.
type skipList struct {
	items []Skipped
	more  int
}

func (l *skipList) add(p, reason string) {
	if len(l.items) < maxSkippedListed {
		l.items = append(l.items, Skipped{p, reason})
		return
	}
	l.more++
}

// list never returns nil, so the JSON is [] rather than null.
func (l *skipList) list() []Skipped {
	out := append([]Skipped{}, l.items...)
	if l.more > 0 {
		out = append(out, Skipped{fmt.Sprintf("... and %d more", l.more), "not listed individually"})
	}
	return out
}

// capReader fails the read that takes the running total past the limit, so a
// zip entry whose header lies about its size is still stopped mid-stream.
type capReader struct {
	r    io.Reader
	max  *int64
	used *int64
	over bool
}

func (c *capReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.used += int64(n)
	if services.Exceeds(c.max, *c.used) {
		c.over = true
		return n, errTooLarge
	}
	return n, err
}

func isSymlink(f *zip.File) bool {
	return f.Mode()&os.ModeSymlink != 0 || (f.ExternalAttrs>>16)&0o170000 == 0o120000
}

// contentAllowed applies the content policy to one file. A nested archive is
// stored as is, never unpacked, but its entry names still have to be safe
// paths: whoever unpacks it later trusts them.
func contentAllowed(rel string, ra io.ReaderAt, size int64, policy string) (bool, string) {
	if ok, reason := services.TemplateFileAllowed(rel, policy); !ok {
		return false, reason
	}
	if !strings.HasSuffix(strings.ToLower(rel), ".zip") {
		return true, ""
	}
	if ok, reason := services.TemplateZipAllowed(ra, size, policy); !ok {
		return false, reason
	}
	if zr, err := zip.NewReader(ra, size); err == nil {
		if len(zr.File) > maxZipEntries {
			return false, fmt.Sprintf("the archive has more than %d entries", maxZipEntries)
		}
		for _, f := range zr.File {
			if _, ok := services.TemplateRelPath(strings.TrimSuffix(f.Name, "/")); !ok || isSymlink(f) {
				return false, "the archive contains an unsafe path"
			}
		}
	}
	return true, ""
}

// extractTemplateZip unpacks zipPath into t's storage entry by entry. Unsafe
// names, symlinks and files the content policy refuses are skipped and listed;
// passing the size limit aborts with errTooLarge. On any error the CALLER
// deletes the template root and the row. On success size_bytes is updated.
func extractTemplateZip(ctx context.Context, state *handlers.AppState, t *models.Template, zipPath string, s services.TemplateSettings) ([]Skipped, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errBadZip, err)
	}
	defer zr.Close()
	if len(zr.File) > maxZipEntries {
		return nil, errTooManyEntries
	}
	var skipped skipList
	var used int64
	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return skipped.list(), err
		}
		if strings.HasSuffix(f.Name, "/") || f.FileInfo().IsDir() {
			continue
		}
		if isSymlink(f) {
			skipped.add(f.Name, "symbolic links are not allowed")
			continue
		}
		rel, ok := services.TemplateRelPath(f.Name)
		if !ok {
			skipped.add(f.Name, "unsafe path")
			continue
		}
		if ok, reason := services.TemplateFileAllowed(rel, s.ContentPolicy); !ok {
			skipped.add(f.Name, reason)
			continue
		}
		reason, err := writeZipEntry(ctx, state, t, rel, f, &capReader{max: s.MaxSizeBytes, used: &used}, s.ContentPolicy)
		if err != nil {
			return skipped.list(), err
		}
		if reason != "" {
			skipped.add(f.Name, reason)
		}
	}
	if err := refreshSize(ctx, state, t); err != nil {
		return skipped.list(), err
	}
	return skipped.list(), nil
}

// writeZipEntry stores one entry through cr. A nested archive is staged first
// so it can be inspected before it is stored; a refusal comes back as reason.
func writeZipEntry(ctx context.Context, state *handlers.AppState, t *models.Template, rel string, f *zip.File, cr *capReader, policy string) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", errBadZip, f.Name, err)
	}
	defer rc.Close()
	cr.r = rc
	var src io.Reader = cr
	if strings.HasSuffix(strings.ToLower(rel), ".zip") {
		tmp, err := os.CreateTemp("", "template-nested-*.zip")
		if err != nil {
			return "", fmt.Errorf("template: stage nested archive: %w", err)
		}
		defer os.Remove(tmp.Name())
		defer tmp.Close()
		n, err := io.Copy(tmp, cr)
		if err != nil {
			return "", entryErr(cr, f.Name, err)
		}
		if ok, reason := contentAllowed(rel, tmp, n, policy); !ok {
			*cr.used -= n
			return reason, nil
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return "", fmt.Errorf("template: rewind nested archive: %w", err)
		}
		src = tmp
	}
	if err := state.TemplateWrite(ctx, t, rel, src, int64(f.UncompressedSize64)); err != nil {
		return "", entryErr(cr, f.Name, err)
	}
	return "", nil
}

func entryErr(cr *capReader, name string, err error) error {
	if cr.over {
		return errTooLarge
	}
	var corrupt flate.CorruptInputError
	if errors.Is(err, zip.ErrChecksum) || errors.Is(err, zip.ErrFormat) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &corrupt) {
		return fmt.Errorf("%w: %s: %v", errBadZip, name, err)
	}
	return fmt.Errorf("template: write %s: %w", name, err)
}

// refreshSize recounts the stored files rather than trusting a running total,
// which double-counts a path written twice.
// ponytail: walks the whole tree per write; keep a delta if templates get large file counts.
func refreshSize(ctx context.Context, state *handlers.AppState, t *models.Template) error {
	size, err := state.TemplateSize(ctx, t)
	if err != nil {
		return err
	}
	if err := state.Store.SetTemplateSize(t.ID, size); err != nil {
		return err
	}
	t.SizeBytes = size
	return nil
}

// sizeWith is the template's total once the files in add (path -> size) are written.
// ponytail: check-then-write, parallel writes to one template can pass the limit together.
func sizeWith(ctx context.Context, state *handlers.AppState, t *models.Template, add map[string]int64) (int64, error) {
	files, err := state.TemplateList(ctx, t)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, f := range files {
		if _, replaced := add[f.Name]; !replaced {
			total += f.Size
		}
	}
	for _, n := range add {
		total += n
	}
	return total, nil
}

// bodyCap bounds an upload by the size limit plus slack, or by maxUploadBody.
func bodyCap(w http.ResponseWriter, r *http.Request, s services.TemplateSettings) bool {
	limit := int64(maxUploadBody)
	if s.MaxSizeBytes != nil && *s.MaxSizeBytes+uploadSlack < limit {
		limit = *s.MaxSizeBytes + uploadSlack
	}
	return handlers.CapBody(w, r, limit)
}

func parseMultipart(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseMultipartForm(8 << 20); err != nil { // 8 MiB in RAM, rest to disk
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			handlers.SendJSONErrorCode(w, "The upload is larger than the template size limit", "templates_too_large", http.StatusRequestEntityTooLarge)
			return false
		}
		handlers.SendJSONError(w, "Upload too large or malformed", http.StatusBadRequest)
		return false
	}
	return true
}

func tooLarge(w http.ResponseWriter) {
	handlers.SendJSONErrorCode(w, "The template would exceed its size limit", "templates_too_large", http.StatusRequestEntityTooLarge)
}

// Upload POST /api/templates/upload - multipart meta (JSON, as for create) and
// file (a zip). Creates the template and unpacks the zip into it; skipped
// entries are listed. Past the size limit or on any error nothing is kept.
func (h *TemplatesHandler) Upload(w http.ResponseWriter, r *http.Request) {
	s := h.settings(r)
	// What can be decided from the caller alone is decided before the body is
	// spooled to disk; a non-admin can only ever create a personal template.
	// newTemplate repeats the full checks once meta is read.
	if v, _ := h.viewer(r, nil); !v.IsAdmin && !h.mayCreate(w, s, v, "personal") {
		return
	}
	if !bodyCap(w, r, s) || !parseMultipart(w, r) {
		return
	}
	defer r.MultipartForm.RemoveAll()
	var m templateMeta
	if err := json.Unmarshal([]byte(r.FormValue("meta")), &m); err != nil {
		handlers.SendJSONError(w, "meta must be the template details as JSON", http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		handlers.SendJSONError(w, "file field required", http.StatusBadRequest)
		return
	}
	defer file.Close()
	staged, err := stage(file)
	if err != nil {
		log.Printf("templates: stage upload: %v", err)
		handlers.SendJSONError(w, "Could not read the upload", http.StatusInternalServerError)
		return
	}
	defer os.Remove(staged)

	t, s, ok := h.newTemplate(w, r, m)
	if !ok {
		return
	}
	skipped, err := extractTemplateZip(r.Context(), h.state, t, staged, s)
	if err != nil {
		h.discard(t)
		h.extractFailed(w, t, err)
		return
	}
	v, _ := h.viewer(r, t)
	writeJSON(w, http.StatusCreated, map[string]interface{}{"template": item(*t, v), "skipped": skipped})
}

// extractFailed answers for an extractTemplateZip error; the caller discards the template.
func (h *TemplatesHandler) extractFailed(w http.ResponseWriter, t *models.Template, err error) {
	switch {
	case errors.Is(err, errTooLarge):
		tooLarge(w)
	case errors.Is(err, errBadZip):
		handlers.SendJSONError(w, "The file is not a readable zip archive", http.StatusBadRequest)
	case errors.Is(err, errTooManyEntries):
		handlers.SendJSONError(w, fmt.Sprintf("The archive has more than %d files and folders", maxZipEntries), http.StatusBadRequest)
	default:
		log.Printf("templates: extract into %d: %v", t.ID, err)
		handlers.SendJSONError(w, "Could not store the template files", http.StatusInternalServerError)
	}
}

// discard removes a template whose creation failed: files, then row. When the
// files cannot be removed the row stays, so the owner sees it and can delete it.
func (h *TemplatesHandler) discard(t *models.Template) {
	ctx := context.Background() // the request may be what was cancelled
	if err := h.state.TemplateDelete(ctx, t, ""); err != nil {
		log.Printf("templates: discard files of %d, keeping the row: %v", t.ID, err)
		return
	}
	if err := h.state.Store.DeleteTemplate(t.ID); err != nil {
		log.Printf("templates: discard %d: %v", t.ID, err)
	}
}

func stage(src io.Reader) (string, error) {
	tmp, err := os.CreateTemp("", "template-upload-*.zip")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// Files GET /api/templates/{id}/files - every file, path relative to the template.
func (h *TemplatesHandler) Files(w http.ResponseWriter, r *http.Request) {
	t, _, _, ok := h.load(w, r, needSee)
	if !ok {
		return
	}
	files, err := h.state.TemplateList(r.Context(), t)
	if err != nil {
		log.Printf("templates: list files of %d: %v", t.ID, err)
		handlers.SendJSONError(w, "Could not list the template files", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"files": files})
}

func relParam(w http.ResponseWriter, raw string) (string, bool) {
	rel, ok := services.TemplateRelPath(raw)
	if !ok {
		handlers.SendJSONError(w, "Invalid file path", http.StatusBadRequest)
	}
	return rel, ok
}

// GetText GET /api/templates/{id}/files/text?path= - a text file up to 1 MiB.
func (h *TemplatesHandler) GetText(w http.ResponseWriter, r *http.Request) {
	t, _, _, ok := h.load(w, r, needSee)
	if !ok {
		return
	}
	rel, ok := relParam(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	rc, err := h.state.TemplateRead(r.Context(), t, rel)
	if err != nil {
		if storage.IsNotExist(err) {
			handlers.SendJSONError(w, "File not found", http.StatusNotFound)
			return
		}
		log.Printf("templates: read %d/%s: %v", t.ID, rel, err)
		handlers.SendJSONError(w, "Could not read the file", http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxTextFileBytes+1))
	if err != nil {
		log.Printf("templates: read %d/%s: %v", t.ID, rel, err)
		handlers.SendJSONError(w, "Could not read the file", http.StatusInternalServerError)
		return
	}
	if len(b) > maxTextFileBytes {
		handlers.SendJSONError(w, "The file is larger than 1 MiB and cannot be edited here", http.StatusRequestEntityTooLarge)
		return
	}
	if !utf8.Valid(b) {
		handlers.SendJSONError(w, "The file is not UTF-8 text and cannot be edited here", http.StatusUnsupportedMediaType)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": rel, "content": string(b)})
}

// PutText PUT /api/templates/{id}/files/text - writes {path, content}.
func (h *TemplatesHandler) PutText(w http.ResponseWriter, r *http.Request) {
	t, _, s, ok := h.load(w, r, needEdit)
	if !ok {
		return
	}
	// JSON escaping can grow content several times over; the decoded length is checked below.
	if !handlers.CapBody(w, r, 8*maxTextFileBytes) {
		return
	}
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		handlers.SendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	rel, ok := relParam(w, p.Path)
	if !ok {
		return
	}
	if len(p.Content) > maxTextFileBytes || !utf8.ValidString(p.Content) {
		handlers.SendJSONError(w, "Content must be UTF-8 text of at most 1 MiB", http.StatusBadRequest)
		return
	}
	data := []byte(p.Content)
	if ok, reason := contentAllowed(rel, bytes.NewReader(data), int64(len(data)), s.ContentPolicy); !ok {
		handlers.SendJSONError(w, reason, http.StatusBadRequest)
		return
	}
	if !h.fits(w, r, t, s, map[string]int64{rel: int64(len(data))}) {
		return
	}
	if err := h.state.TemplateWrite(r.Context(), t, rel, bytes.NewReader(data), int64(len(data))); err != nil {
		log.Printf("templates: write %d/%s: %v", t.ID, rel, err)
		handlers.SendJSONError(w, "Could not save the file", http.StatusInternalServerError)
		return
	}
	h.written(w, r, t, nil)
}

// fits answers 413 when writing add would take t past the size limit.
func (h *TemplatesHandler) fits(w http.ResponseWriter, r *http.Request, t *models.Template, s services.TemplateSettings, add map[string]int64) bool {
	if s.MaxSizeBytes == nil {
		return true
	}
	total, err := sizeWith(r.Context(), h.state, t, add)
	if err != nil {
		log.Printf("templates: size of %d: %v", t.ID, err)
		handlers.SendJSONError(w, "Could not check the template size", http.StatusInternalServerError)
		return false
	}
	if services.Exceeds(s.MaxSizeBytes, total) {
		tooLarge(w)
		return false
	}
	return true
}

// written updates size_bytes after a change and answers with the new size.
func (h *TemplatesHandler) written(w http.ResponseWriter, r *http.Request, t *models.Template, skipped []Skipped) {
	if err := refreshSize(r.Context(), h.state, t); err != nil {
		log.Printf("templates: size of %d: %v", t.ID, err)
	}
	body := map[string]interface{}{"success": true, "sizeBytes": t.SizeBytes}
	if skipped != nil {
		body["skipped"] = skipped
	}
	writeJSON(w, http.StatusOK, body)
}

// AddFiles POST /api/templates/{id}/files - multipart: one or more "file"
// parts, stored under the folder "dir" ("" = the template root). Files the
// content policy refuses are skipped and listed.
func (h *TemplatesHandler) AddFiles(w http.ResponseWriter, r *http.Request) {
	t, _, s, ok := h.load(w, r, needEdit)
	if !ok {
		return
	}
	if !bodyCap(w, r, s) || !parseMultipart(w, r) {
		return
	}
	defer r.MultipartForm.RemoveAll()
	dir := r.FormValue("dir")
	if dir != "" {
		if dir, ok = relParam(w, dir); !ok {
			return
		}
	}
	type accepted struct {
		rel string
		fh  *multipart.FileHeader
	}
	var take []accepted
	add := map[string]int64{}
	var skipped skipList
	for _, fh := range r.MultipartForm.File["file"] {
		name := path.Base(strings.ReplaceAll(fh.Filename, `\`, "/"))
		full := name
		if dir != "" {
			full = dir + "/" + name
		}
		rel, ok := services.TemplateRelPath(full)
		if !ok || name == "." || name == "/" {
			skipped.add(fh.Filename, "unsafe path")
			continue
		}
		f, err := fh.Open()
		if err != nil {
			handlers.SendJSONError(w, "Could not read the upload", http.StatusBadRequest)
			return
		}
		allowed, reason := contentAllowed(rel, f, fh.Size, s.ContentPolicy)
		f.Close()
		if !allowed {
			skipped.add(rel, reason)
			continue
		}
		take = append(take, accepted{rel, fh})
		add[rel] = fh.Size
	}
	if !h.fits(w, r, t, s, add) {
		return
	}
	for _, a := range take {
		f, err := a.fh.Open()
		if err != nil {
			handlers.SendJSONError(w, "Could not read the upload", http.StatusBadRequest)
			return
		}
		err = h.state.TemplateWrite(r.Context(), t, a.rel, f, a.fh.Size)
		f.Close()
		if err != nil {
			log.Printf("templates: write %d/%s: %v", t.ID, a.rel, err)
			// Files written before this one stay; record their size.
			_ = refreshSize(r.Context(), h.state, t)
			handlers.SendJSONError(w, "Could not store "+a.rel, http.StatusInternalServerError)
			return
		}
	}
	h.written(w, r, t, skipped.list())
}

// DeleteFile DELETE /api/templates/{id}/files?path= - a file or a folder.
func (h *TemplatesHandler) DeleteFile(w http.ResponseWriter, r *http.Request) {
	t, _, _, ok := h.load(w, r, needEdit)
	if !ok {
		return
	}
	rel, ok := relParam(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	if err := h.state.TemplateDelete(r.Context(), t, rel); err != nil {
		log.Printf("templates: delete %d/%s: %v", t.ID, rel, err)
		handlers.SendJSONError(w, "Could not delete the file", http.StatusInternalServerError)
		return
	}
	h.written(w, r, t, nil)
}
