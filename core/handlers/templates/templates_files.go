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
// passing the size limit aborts with errTooLarge. On own storage, where the
// template has no size limit, the decompressed bytes of one extraction are
// still held to the transit ceiling (requestBound): they stream through Core,
// and a nested archive is staged on its disk. On any error the CALLER
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
	limit := requestBound(s, t.BackupStorageID != nil)
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
		reason, err := writeZipEntry(ctx, state, t, rel, f, &capReader{max: limit, used: &used}, s.ContentPolicy)
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

// ownStorageTransitDefault is the transit ceiling when the admin cap is 0: a
// cap of zero on our storage says nothing about what may pass through Core.
const ownStorageTransitDefault = 2 << 30

// requestBound is the size one request may carry for a template whose cap is
// s.MaxSizeBytes. A template on its owner's own storage has no size cap, but
// every request still lands on Core's disk first (the upload is spooled, a
// capture or a bundle is staged), and that disk is transit, not the tenant's
// bucket. So each such request is held to the admin cap, the largest template
// Core otherwise takes in; the template itself can still grow past it over
// several requests. nil (an operator's "unlimited" admin cap) leaves
// maxUploadBody as the bound.
func requestBound(s services.TemplateSettings, ownStorage bool) *int64 {
	if !ownStorage || s.MaxSizeBytes != nil {
		return s.MaxSizeBytes
	}
	if s.MaxSizeBytesAdmin != nil && *s.MaxSizeBytesAdmin == 0 {
		v := int64(ownStorageTransitDefault)
		return &v
	}
	return s.MaxSizeBytesAdmin
}

// bodyLimit is max plus slack, never more than maxUploadBody; nil is maxUploadBody.
func bodyLimit(max *int64) int64 {
	limit := int64(maxUploadBody)
	if max != nil && *max+uploadSlack < limit {
		limit = *max + uploadSlack
	}
	return limit
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

func tooLarge(w http.ResponseWriter) { tooLargeNote(w, "") }

// tooLargeNote is tooLarge with note (see discard) appended.
func tooLargeNote(w http.ResponseWriter, note string) {
	handlers.SendJSONErrorCode(w, withNote("The template would exceed its size limit.", note), "templates_too_large", http.StatusRequestEntityTooLarge)
}

func withNote(msg, note string) string {
	if note == "" {
		return msg
	}
	return msg + " " + note
}

// Upload POST /api/templates/upload - multipart meta (JSON, as for create) and
// file (a zip). Creates the template and unpacks the zip into it; skipped
// entries are listed. Past the size limit or on any error nothing is kept.
func (h *TemplatesHandler) Upload(w http.ResponseWriter, r *http.Request) {
	s := h.settings(r)
	// What can be decided from the caller alone is decided before the body is
	// spooled to disk; a non-admin can only ever create a personal template.
	// newTemplate repeats the full checks once meta is read.
	v, _ := h.viewer(r, nil)
	if h.refuseSuspended(w, r, v.UserID) {
		return
	}
	own, ok := h.ownTarget(w, v.UserID, "personal")
	if !ok || (!v.IsAdmin && !h.mayCreate(w, s, v, "personal", own)) {
		return
	}
	// The body is bounded by the largest cap this caller can get: every
	// template an admin owns takes the admin cap, and only personal ones go to
	// own storage, where the transit ceiling applies. Extraction then applies
	// the template's real cap.
	s.MaxSizeBytes = s.SizeCap("personal", v.IsAdmin, own != nil)
	if !handlers.CapBody(w, r, bodyLimit(requestBound(s, own != nil))) || !parseMultipart(w, r) {
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
		h.extractFailed(w, t, err, h.discard(t))
		return
	}
	v, _ = h.viewer(r, t)
	writeJSON(w, http.StatusCreated, map[string]interface{}{"template": item(*t, v, s.MaxSizeBytes), "skipped": skipped})
}

// extractFailed answers for an extractTemplateZip error once the caller has
// discarded the template; note is what discard returned.
func (h *TemplatesHandler) extractFailed(w http.ResponseWriter, t *models.Template, err error, note string) {
	switch {
	case errors.Is(err, errTooLarge):
		tooLargeNote(w, note)
	case errors.Is(err, errBadZip):
		handlers.SendJSONError(w, withNote("The file is not a readable zip archive.", note), http.StatusBadRequest)
	case errors.Is(err, errTooManyEntries):
		handlers.SendJSONError(w, withNote(fmt.Sprintf("The archive has more than %d files and folders.", maxZipEntries), note), http.StatusBadRequest)
	default:
		log.Printf("templates: extract into %d: %v", t.ID, err)
		handlers.SendJSONError(w, withNote("Could not store the template files.", note), http.StatusInternalServerError)
	}
}

// discard removes a template whose creation failed: files, then row. When the
// files on our storage cannot be removed the row stays, so the owner sees it
// and can delete it; on the owner's own storage the row goes regardless (see
// deleteFiles). It returns the sentence the error answer must carry when
// files may remain in the owner's bucket, else "".
func (h *TemplatesHandler) discard(t *models.Template) string {
	ctx := context.Background() // the request may be what was cancelled
	rowMayGo, filesLeft := h.deleteFiles(ctx, t)
	if !rowMayGo {
		log.Printf("templates: discard of %d keeps the row", t.ID)
		return ""
	}
	if err := h.state.Store.DeleteTemplate(t.ID); err != nil {
		log.Printf("templates: discard %d: %v", t.ID, err)
	}
	if filesLeft {
		return ownFilesMayRemain
	}
	return ""
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
	if !ok || h.refuseSuspended(w, r, t.OwnerID) {
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

// A bundled add-files request carries at most maxBundleFiles files and
// bundleBytesCap bytes of file data. The panel batches by the same numbers; Core
// enforces them while streaming so a larger request is refused without being
// spooled. A request with exactly ONE file may exceed bundleBytesCap: it is
// then bounded only by the template size cap (see AddFiles), so a single large
// file still uploads. bundleBytesCap is a var only so tests can use small caps.
const maxBundleFiles = 200

var (
	bundleBytesCap int64 = 100 << 20
	// stageDir holds staged parts; "" is os.TempDir. Tests point it at a temp dir.
	stageDir = ""
)

// stagedFile is one uploaded part held on disk until the whole request is checked.
type stagedFile struct {
	name string // the part's own file name
	tmp  string
	size int64
}

func removeStaged(files []stagedFile) {
	for _, f := range files {
		os.Remove(f.tmp)
	}
}

// readBundle streams the multipart body: "dir", repeated "path" fields and
// repeated "file" parts. It stages each file part to a temp file, and stops at
// the first limit breached. On any non-zero status nothing stays staged; on
// success the CALLER removes the returned files.
func readBundle(r *http.Request) (dir string, paths []string, files []stagedFile, status int, msg string) {
	var staged []stagedFile
	defer func() {
		if status != 0 {
			removeStaged(staged)
			files = nil
		}
	}()
	fail := func(code int, m string) (string, []string, []stagedFile, int, string) {
		return "", nil, nil, code, m
	}
	mr, err := r.MultipartReader()
	if err != nil {
		return fail(http.StatusBadRequest, "Upload is not multipart")
	}
	var total int64
	for parts := 1; ; parts++ {
		p, err := mr.NextPart()
		if err == io.EOF {
			return dir, paths, staged, 0, ""
		}
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				return fail(http.StatusRequestEntityTooLarge, "The upload is larger than allowed")
			}
			return fail(http.StatusBadRequest, "Upload malformed")
		}
		// Every part counts, whatever its name: a flood of unknown fields is not free.
		if parts > 2*maxBundleFiles+1 {
			return fail(http.StatusRequestEntityTooLarge, fmt.Sprintf("At most %d files per request", maxBundleFiles))
		}
		switch p.FormName() {
		case "dir", "path":
			b, err := io.ReadAll(io.LimitReader(p, 4097))
			if err != nil || len(b) > 4096 {
				return fail(http.StatusBadRequest, "A path is invalid or too long")
			}
			if p.FormName() == "dir" {
				dir = string(b)
			} else {
				if len(paths) >= maxBundleFiles {
					return fail(http.StatusRequestEntityTooLarge, fmt.Sprintf("At most %d files per request", maxBundleFiles))
				}
				paths = append(paths, string(b))
			}
		case "file":
			if len(staged) >= maxBundleFiles {
				return fail(http.StatusRequestEntityTooLarge, fmt.Sprintf("At most %d files per request", maxBundleFiles))
			}
			// Every file after the first counts against the bundle cap; the first
			// is bounded only by the body limit.
			var src io.Reader = p
			if len(staged) > 0 {
				left := bundleBytesCap - total
				if left < 0 {
					return fail(http.StatusRequestEntityTooLarge, "The upload is larger than allowed")
				}
				src = io.LimitReader(p, left+1)
			}
			tmp, err := os.CreateTemp(stageDir, services.TemplateStageAddPattern)
			if err != nil {
				log.Printf("templates: stage add-files part: %v", err)
				return fail(http.StatusInternalServerError, "Could not read the upload")
			}
			n, err := io.Copy(tmp, src)
			tmp.Close()
			staged = append(staged, stagedFile{p.FileName(), tmp.Name(), n}) // appended first so it is removed on failure
			if err != nil {
				var maxErr *http.MaxBytesError
				if errors.As(err, &maxErr) {
					return fail(http.StatusRequestEntityTooLarge, "The upload is larger than allowed")
				}
				return fail(http.StatusBadRequest, "Upload malformed")
			}
			total += n
			if len(staged) > 1 && total > bundleBytesCap {
				return fail(http.StatusRequestEntityTooLarge, "The upload is larger than allowed")
			}
		}
	}
}

// AddFiles POST /api/templates/{id}/files - multipart: repeated "file" parts stored under the repeated "path" fields in order (legacy: under the folder "dir").
// Read as a stream. path[i] belongs to file[i] and is relative, with sub-folders
// and the file name; without any "path" field every file is stored under "dir"
// ("" = the template root) by its own name. Files the content policy refuses are
// skipped and listed.
func (h *TemplatesHandler) AddFiles(w http.ResponseWriter, r *http.Request) {
	t, _, s, ok := h.load(w, r, needEdit)
	if !ok || h.refuseSuspended(w, r, t.OwnerID) {
		return
	}
	// The stream may carry one file up to the template cap (the transit ceiling
	// on own storage, maxUploadBody when uncapped); readBundle holds a
	// multi-file request to bundleBytesCap.
	limit := bodyLimit(requestBound(s, t.BackupStorageID != nil))
	if r.ContentLength > limit {
		tooLarge(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dir, paths, files, status, msg := readBundle(r)
	defer removeStaged(files)
	if status != 0 {
		if status == http.StatusRequestEntityTooLarge {
			handlers.SendJSONErrorCode(w, msg, "templates_too_large", status)
		} else {
			handlers.SendJSONError(w, msg, status)
		}
		return
	}
	if len(paths) > 0 && len(paths) != len(files) {
		handlers.SendJSONError(w, "Every file needs exactly one path field", http.StatusBadRequest)
		return
	}
	if dir != "" {
		if dir, ok = relParam(w, dir); !ok {
			return
		}
	}
	type accepted struct {
		rel string
		f   stagedFile
	}
	var take []accepted
	add := map[string]int64{}
	seen := map[string]bool{}
	var skipped skipList
	for i, sf := range files {
		shown := sf.name
		full := path.Base(strings.ReplaceAll(sf.name, `\`, "/"))
		legacyBad := full == "." || full == "/"
		if len(paths) > 0 {
			shown = paths[i]
			full = strings.ReplaceAll(paths[i], `\`, "/")
		}
		if dir != "" {
			full = dir + "/" + full
		}
		rel, ok := services.TemplateRelPath(full)
		if !ok || (len(paths) == 0 && legacyBad) || strings.HasSuffix(full, "/") {
			skipped.add(shown, "unsafe path")
			continue
		}
		if _, dup := seen[rel]; dup {
			// Refused, not deduped: which copy "wins" would depend on request order.
			handlers.SendJSONError(w, "The same path appears twice: "+rel, http.StatusBadRequest)
			return
		}
		seen[rel] = true
		f, err := os.Open(sf.tmp)
		if err != nil {
			handlers.SendJSONError(w, "Could not read the upload", http.StatusInternalServerError)
			return
		}
		allowed, reason := contentAllowed(rel, f, sf.size, s.ContentPolicy)
		f.Close()
		if !allowed {
			skipped.add(rel, reason)
			continue
		}
		take = append(take, accepted{rel, sf})
		add[rel] = sf.size
	}
	if !h.fits(w, r, t, s, add) {
		return
	}
	for _, a := range take {
		f, err := os.Open(a.f.tmp)
		if err != nil {
			handlers.SendJSONError(w, "Could not read the upload", http.StatusInternalServerError)
			return
		}
		err = h.state.TemplateWrite(r.Context(), t, a.rel, f, a.f.size)
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
