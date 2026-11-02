package httpx

import (
	"mime"
	"net/http/httptest"
	"strings"
	"testing"
)

// A caller-supplied name must stay INSIDE the filename parameter.
//
// Four handlers built this header by concatenating the name into a quoted
// string, and one did not quote it at all. A name is not a token: a space ends
// an unquoted one and a quote closes a quoted one, so the uploader could append
// their own parameters - and RFC 6266 gives filename* precedence over filename,
// which decides what the DOWNLOADER's browser saves the file as.
func TestAFilenameCannotEscapeItsContentDispositionParameter(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		want     string // the filename the header actually carries
	}{
		{"an ordinary name", "report.pdf", "report.pdf"},
		{"a space", "quarterly report.pdf", "quarterly report.pdf"},
		{"a quote", `report".pdf`, `report".pdf`},
		{"an injected parameter", `a";filename*=UTF-8''evil.html;x="`, `a";filename*=UTF-8''evil.html;x="`},
		{"a semicolon", "a;b.txt", "a;b.txt"},
		{"a backslash", `a\b.txt`, `a\b.txt`},
		{"non-ascii", "Übersicht.pdf", "Übersicht.pdf"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			SetAttachmentDisposition(w, tt.filename)
			got := w.Header().Get("Content-Disposition")

			// Parse it the way a client does, rather than eyeballing the string.
			disp, params, err := mime.ParseMediaType(got)
			if err != nil {
				t.Fatalf("header %q does not parse: %v", got, err)
			}
			if disp != "attachment" {
				t.Errorf("disposition = %q, want attachment (header %q)", disp, got)
			}
			if params["filename"] != tt.want {
				t.Errorf("filename = %q, want %q (header %q)", params["filename"], tt.want, got)
			}
			// Nothing the caller wrote may become a parameter of its own.
			for k := range params {
				if k != "filename" {
					t.Errorf("the name injected a %q parameter: %q", k, got)
				}
			}
		})
	}
}

// A name that cannot be encoded at all must still produce a usable header
// rather than an empty one - losing the suggested filename is acceptable,
// sending no disposition is not (the browser may then render the body).
func TestAnUnencodableFilenameStillDisposesAsAttachment(t *testing.T) {
	w := httptest.NewRecorder()
	SetAttachmentDisposition(w, strings.Repeat("\x00", 4))
	got := w.Header().Get("Content-Disposition")
	if !strings.HasPrefix(got, "attachment") {
		t.Errorf("Content-Disposition = %q, want it to still start with attachment", got)
	}
}
