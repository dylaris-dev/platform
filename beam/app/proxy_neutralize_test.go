package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Linux passes WebKit the type alone, so the type is the only lever: anything
// that is not the panel's own page must not render as a document at the app
// origin, where a document gets the native bridge.
func TestForeignDocumentsAreNeutralized(t *testing.T) {
	cases := []struct {
		ctype, disposition string
		want               string
	}{
		{"text/xml; charset=utf-8", "", "application/octet-stream"},
		{"image/svg+xml", "", "application/octet-stream"},
		{"application/xhtml+xml", "", "application/octet-stream"},
		{"application/xml", "", "application/octet-stream"},
		{"text/plain; charset=utf-8", `attachment; filename="a.txt"`, "application/octet-stream"},
		{"text/html; charset=utf-8", "", "text/html; charset=utf-8"},
		{"application/json", "", "application/json"},
		{"image/png", "", "image/png"},
		{"text/css", "", "text/css"},
	}
	for _, c := range cases {
		h := http.Header{}
		h.Set("Content-Type", c.ctype)
		if c.disposition != "" {
			h.Set("Content-Disposition", c.disposition)
		}
		neutralizeForeignDocument(h)
		if got := h.Get("Content-Type"); got != c.want {
			t.Errorf("%q (%q) -> %q, want %q", c.ctype, c.disposition, got, c.want)
		}
	}
}

// Through the real proxy: an attachment Core serves as text/xml leaves it as
// bytes.
func TestTheProxyNeutralizesAnXMLAttachment(t *testing.T) {
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		io.WriteString(w, `<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml"><script>1</script></html>`)
	}))
	defer panel.Close()
	isolateUserDirs(t)
	// The built-in official panel is in every list; without an entry of our
	// own it would be the target, and this test would ask production.
	if err := saveSettings(userSettings{Panels: []savedPanel{{URL: panel.URL}}, Active: panel.URL}); err != nil {
		t.Fatal(err)
	}
	a := NewApp()
	if got := a.GetPanelURL(); got != panel.URL {
		t.Fatalf("proxy target is %q, not the test server", got)
	}
	h := newPanelMiddleware(a, http.NotFoundHandler())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://wails.localhost/some/attachment.txt", nil))
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want application/octet-stream (status %d) %s", got, rec.Code, rec.Body.String())
	}
}
