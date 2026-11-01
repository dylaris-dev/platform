package templates

import (
	"context"
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
	return s.TemplateZip(ctx, t, w)
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
