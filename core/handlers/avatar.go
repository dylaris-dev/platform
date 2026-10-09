package handlers

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
)

// Player heads for the panel, fetched here and kept for a while.
//
// The panel used to load them straight from cravatar.eu, so when that service
// went down every avatar in the Players tab and the user menu broke at once,
// and the Beam app's CSP allowed that one host only. Served from Core, a dead
// service is one upstream among two and the browser only ever talks to us.

var avatarNameRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,16}$`)

// avatarUpstreams are tried in order; both answer a 64px head with the hat
// layer for a Java name.
var avatarUpstreams = []string{
	"https://mc-heads.net/avatar/%s/64",
	"https://minotar.net/helm/%s/64.png",
}

const (
	avatarTTL        = 6 * time.Hour
	avatarMissTTL    = 10 * time.Minute
	avatarMaxBytes   = 256 << 10
	avatarMaxEntries = 4096
)

type avatarEntry struct {
	png     []byte
	expires time.Time
}

type AvatarHandler struct {
	client *http.Client
	mu     sync.Mutex
	cache  map[string]avatarEntry
}

func NewAvatarHandler() *AvatarHandler {
	return &AvatarHandler{
		client: &http.Client{
			Timeout: 5 * time.Second,
			// A head is one image; a redirect is not something either host does
			// for one, and following it would make the upstream list a suggestion.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		cache: map[string]avatarEntry{},
	}
}

// Get GET /api/avatar/{name} - the 64px head of a Minecraft player, or 404.
func (h *AvatarHandler) Get(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if !avatarNameRe.MatchString(name) {
		http.Error(w, "invalid player name", http.StatusBadRequest)
		return
	}
	key := strings.ToLower(name)
	now := time.Now()

	h.mu.Lock()
	e, hit := h.cache[key]
	h.mu.Unlock()
	if !hit || now.After(e.expires) {
		e = avatarEntry{png: h.fetch(name), expires: now.Add(avatarTTL)}
		if e.png == nil {
			// Remembered briefly, so a name no service knows does not reach
			// them on every render of the Players tab.
			e.expires = now.Add(avatarMissTTL)
		}
		h.mu.Lock()
		// ponytail: dropped whole when full; an LRU if 4096 distinct heads per
		// replica ever turns over within six hours.
		if len(h.cache) >= avatarMaxEntries {
			h.cache = map[string]avatarEntry{}
		}
		h.cache[key] = e
		h.mu.Unlock()
	}
	if e.png == nil {
		http.Error(w, "no avatar", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=21600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(e.png)
}

func (h *AvatarHandler) fetch(name string) []byte {
	for _, u := range avatarUpstreams {
		if b := h.fetchOne(fmt.Sprintf(u, name)); b != nil {
			return b
		}
	}
	return nil
}

func (h *AvatarHandler) fetchOne(url string) []byte {
	resp, err := h.client.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/png") {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, avatarMaxBytes+1))
	if err != nil || len(b) > avatarMaxBytes || len(b) < 8 || string(b[1:4]) != "PNG" {
		return nil
	}
	return b
}
