package handlers

import (
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/gorilla/mux"
)

// modpackMirrorPath is the route a node fetches a panel-built pack from. The
// suffix after it is the storage key, so the full URL is
// {core_public_url}/mirror/modpacks/<hmac>/pack.mrpack.
//
// It used to live under /solder/mirror/, because the same route also served the
// Technic launcher. Solder is gone; this is the one job the route still has.
const modpackMirrorPath = "/mirror/"

// modpackMirrorBase is the absolute base a node is pointed at to download a
// rendered pack. It is Core's own public URL: the only other delivery the old
// route knew was a Solder-only "public bucket" mode, and a pack install was
// never meant to leave Core's hands.
//
// The returned base always ends with a single trailing slash.
func modpackMirrorBase(getSetting func(string) (string, error)) (string, error) {
	base, _ := getSetting("core_public_url")
	base = strings.TrimSpace(base)
	if base == "" {
		return "", fmt.Errorf("core_public_url is not set (a node needs it to download a pack from this Core)")
	}
	return strings.TrimRight(base, "/") + modpackMirrorPath, nil
}

// ModpackMirror streams a rendered pack .mrpack to the node installing it.
//
// Unauthenticated, because the node fetches it with a plain GET and no
// credential. The key carries an HMAC of CLUSTER_SECRET (mrpackStorageKey), so
// it cannot be guessed; the prefix check is what keeps this from being a
// general read of the storage bucket. SECURITY: only keys under modpacks/ after
// path.Clean, and no traversal. The Solder prefixes this route used to allow
// (solder/mods/, loaders/) are gone with Solder - anything left under them in a
// bucket is no longer reachable from here, which is the point.
func (h *PacksHandler) ModpackMirror(w http.ResponseWriter, r *http.Request) {
	if !h.state.FeatureFlags.IsModpacksEnabled(r.Context()) {
		sendJSONError(w, "Modpacks are disabled", http.StatusForbidden)
		return
	}
	// path.Clean resolves every ".." segment, so a cleaned key that starts with
	// modpacks/ cannot contain one and the prefix test alone is the boundary
	// (TestModpackMirrorServesBuiltPacksOnly). The explicit ".." check is a
	// second fence for the day the Clean moves; no input reaches it today.
	key := path.Clean(mux.Vars(r)["rest"])
	if strings.Contains(key, "..") || !strings.HasPrefix(key, "modpacks/") {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	prov, err := h.state.buildModpackStorageProvider()
	if err != nil || prov == nil {
		sendJSONError(w, "Storage not configured", http.StatusInternalServerError)
		return
	}
	// Streamed rather than redirected, so the node only ever talks to the host
	// its MODPACK_MIRROR_HOSTS allowlist names - a redirect would hand it a
	// storage-bucket URL instead. And streamed rather than loaded whole: Core
	// used to hold the entire pack in memory once per concurrent request.
	if err := serveModpackObject(w, r, prov, key, deliverStream, "application/zip", path.Base(key)); err != nil {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
}
