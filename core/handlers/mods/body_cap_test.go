package mods

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/handlers/httpx"
)

// The Modrinth PAT save and the admin modpack settings writes decode JSON
// that is a few hundred bytes at most; an unbounded body let one request
// buffer gigabytes in Core (C62). Each must answer 413 before decoding.
func TestModsJSONWritesRefuseOversizedBodies(t *testing.T) {
	fs := handlertest.NewSettingsStore()
	settings := newModpackSettingsTestHandler(fs)
	pat := NewModrinthPATHandler(Deps{Store: fs}, "secret")

	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"modrinth PAT set", pat.Set},
		{"modpack settings set", settings.Set},
		{"modpack user flag", settings.SetUserFlag},
	}
	body := `{"token":"` + strings.Repeat("A", httpx.SmallJSONBodyLimit) + `"}`
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Cancelled so a handler that does reach the Modrinth call fails
			// fast instead of going to the network.
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), "userID", "u1"))
			cancel()
			req := httptest.NewRequest(http.MethodPut, "/x", bytes.NewReader([]byte(body))).WithContext(ctx)
			rw := httptest.NewRecorder()
			c.handler(rw, req)
			if rw.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413 (%s)", rw.Code, rw.Body.String())
			}
		})
	}
}
