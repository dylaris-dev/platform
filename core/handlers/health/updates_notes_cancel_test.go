package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

const remoteNotes = `## 2099.01.01

### Features
- Something newer than this build.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.
`

// One aborted request must not decide what every caller on this replica sees
// for the next 15 minutes: the fetch outlives the request that started it.
func TestNotesFetchSurvivesACancelledRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(remoteNotes))
	}))
	defer srv.Close()

	h := &UpdatesHandler{urls: map[string]string{feedPlatform: srv.URL}, cache: map[string]cachedNotes{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rs := h.notes(ctx, feedPlatform)
	if len(rs) == 0 || rs[0].Version.String() != "2099.01.01" {
		t.Fatalf("notes after a cancelled request = %v, want the remote 2099.01.01", rs)
	}
	if c := h.cache[feedPlatform]; len(c.releases) == 0 || c.releases[0].Version.String() != "2099.01.01" {
		t.Fatalf("cached %v, want the remote notes", c.releases)
	}
}
