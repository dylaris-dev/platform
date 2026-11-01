package templates

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"

	"dylaris-core/handlers"
	"dylaris-core/models"
)

// templateZipTo is replaceable in tests (a storage that fails mid-stream).
var templateZipTo = func(ctx context.Context, s *handlers.AppState, t *models.Template, w io.Writer) error {
	extra, err := modrinthManifest(s, t.ID)
	if err != nil {
		return err
	}
	return s.TemplateZipWith(ctx, t, w, extra)
}

// modrinthManifestName lists a template's Modrinth references at the root of
// its download. The jars themselves are never in it: they are Modrinth's to
// host, and the manifest says where each one comes from and where it goes.
const modrinthManifestName = "dylaris-modrinth.json"

type modrinthManifestFile struct {
	Path      string `json:"path"`
	ProjectID string `json:"projectId"`
	VersionID string `json:"versionId"`
	URL       string `json:"url"`
	Sha512    string `json:"sha512"`
	FileName  string `json:"fileName"`
}

// modrinthManifest is nil for a template without references, so its download
// is exactly its stored files.
func modrinthManifest(s *handlers.AppState, templateID int) (map[string][]byte, error) {
	refs, err := s.Store.ListTemplateModrinth(templateID)
	if err != nil {
		return nil, fmt.Errorf("modrinth references: %w", err)
	}
	if len(refs) == 0 {
		return nil, nil
	}
	files := make([]modrinthManifestFile, 0, len(refs))
	for _, r := range refs {
		files = append(files, modrinthManifestFile{r.Path, r.ProjectID, r.VersionID, r.URL, r.Sha512, r.FileName})
	}
	data, err := json.MarshalIndent(map[string]any{"version": 1, "files": files}, "", "  ")
	if err != nil {
		return nil, err
	}
	return map[string][]byte{modrinthManifestName: data}, nil
}

var unsafeFilenameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// downloadName is the template name reduced to [A-Za-z0-9._-], so it can sit in
// a quoted Content-Disposition value without escaping.
func downloadName(id int, name string) string {
	n := strings.Trim(unsafeFilenameChars.ReplaceAllString(name, "_"), "._-")
	if n == "" {
		n = fmt.Sprintf("template-%d", id)
	}
	return n + ".zip"
}

// Download GET /api/templates/{id}/download - the template's files as a zip.
// No suspension gate: a tenant can always take their files with them.
func (h *TemplatesHandler) Download(w http.ResponseWriter, r *http.Request) {
	t, _, _, ok := h.load(w, r, needSee)
	if !ok {
		return
	}
	// The zip is streamed, so a failure before the first byte can still be an
	// error status; after that the client only sees a truncated download.
	cw := &countingWriter{w: w}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+downloadName(t.ID, t.Name)+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := templateZipTo(r.Context(), h.state, t, cw); err != nil {
		log.Printf("templates: download %d: %v", t.ID, err)
		if cw.n == 0 {
			w.Header().Del("Content-Disposition")
			handlers.SendJSONError(w, "Could not read the template files", http.StatusBadGateway)
			return
		}
		// Bytes are out: returning would end the chunked stream cleanly and the
		// browser would report a finished download of a broken zip. Aborting
		// cuts the connection so the client sees the failure.
		panic(http.ErrAbortHandler)
	}
}

type countingWriter struct {
	w http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
