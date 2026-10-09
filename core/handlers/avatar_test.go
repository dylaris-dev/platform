package handlers

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gorilla/mux"
)

var pngHead = []byte("\x89PNG\r\n\x1a\nhead")

// upstream answers like an avatar service: a PNG, or the given status.
func upstream(t *testing.T, status int, ctype string, hits *int32) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", ctype)
		w.WriteHeader(status)
		if status == http.StatusOK {
			w.Write(pngHead)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/%s"
}

func getAvatar(h *AvatarHandler, name string) *httptest.ResponseRecorder {
	r := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/api/avatar/x", nil), map[string]string{"name": name})
	w := httptest.NewRecorder()
	h.Get(w, r)
	return w
}

func withUpstreams(t *testing.T, urls ...string) {
	t.Helper()
	orig := avatarUpstreams
	avatarUpstreams = urls
	t.Cleanup(func() { avatarUpstreams = orig })
}

// The service the panel used went down and every head with it; the next one
// in line answers instead, and a head is fetched once, not per render.
func TestAnAvatarFallsBackAndIsKept(t *testing.T) {
	var down, up int32
	withUpstreams(t, upstream(t, http.StatusServiceUnavailable, "text/html", &down), upstream(t, http.StatusOK, "image/png", &up))
	h := NewAvatarHandler()
	for i := 0; i < 3; i++ {
		w := getAvatar(h, "Notch")
		if w.Code != http.StatusOK || w.Body.String() != string(pngHead) || w.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("got %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
	}
	if down != 1 || up != 1 {
		t.Fatalf("upstream hits down=%d up=%d, want one each", down, up)
	}
}

// Only a PNG is passed on: a service answering with an HTML page under 200
// must not reach the panel as an image.
func TestANonImageAnswerIsNoAvatar(t *testing.T) {
	var hits int32
	withUpstreams(t, upstream(t, http.StatusOK, "text/html", &hits))
	if w := getAvatar(NewAvatarHandler(), "Notch"); w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", w.Code)
	}
}

// The name goes into an upstream URL; anything but a Minecraft name is refused
// before a request leaves.
func TestAnAvatarNameIsAMinecraftName(t *testing.T) {
	var hits int32
	withUpstreams(t, upstream(t, http.StatusOK, "image/png", &hits))
	h := NewAvatarHandler()
	for _, bad := range []string{"../x", "a b", "seventeen_chars_x", "x%2Fy", ""} {
		if w := getAvatar(h, bad); w.Code != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", bad, w.Code)
		}
	}
	if hits != 0 {
		t.Fatalf("%d upstream requests for refused names", hits)
	}
}
