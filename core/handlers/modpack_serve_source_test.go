package handlers

import (
	"os"
	"strings"
	"testing"
)

// TestPublicPackRoutesNeverCallGet enforces, in a way a test can fail on, that
// the two byte-serving public handlers go through serveModpackObject.
//
// This is a source check because no black-box test can catch the regression.
// Going back to prov.Get plus w.Write produces a byte-identical response with
// identical headers; the only difference is that Core holds the whole object
// in memory while doing it, once per concurrent request. The response cannot
// tell you that, so the assertion has to be about the code.
//
// The alternative was a comment asking future readers not to do it, and a
// comment does not fail a build. These two routes are unauthenticated and
// serve packs that reach into the gigabytes, which is why they get an
// enforcement rather than a note.
//
// Scoped deliberately to these two files. Every other caller of Get in this
// package genuinely needs the bytes - zip assembly, hashing, manifest parsing -
// and must keep using it.
func TestPublicPackRoutesNeverCallGet(t *testing.T) {
	files := []struct {
		path  string
		route string
	}{
		{path: "modpack_mirror.go", route: "GET /mirror/{rest} (unauthenticated)"},
		{path: "packs_share.go", route: "GET /api/share/{token} (unauthenticated)"},
	}

	for _, f := range files {
		t.Run(f.path, func(t *testing.T) {
			src, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatalf("read %s: %v", f.path, err)
			}
			if strings.Contains(string(src), "prov.Get(") {
				t.Fatalf("%s calls prov.Get: %s serves stored objects and must stream them "+
					"through serveModpackObject, or Core buffers a whole pack per concurrent request",
					f.path, f.route)
			}
			if !strings.Contains(string(src), "serveModpackObject(") {
				t.Fatalf("%s no longer calls serveModpackObject: %s must not grow its own serving path",
					f.path, f.route)
			}
		})
	}
}
