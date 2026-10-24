package packs

import (
	"net/http"
	"path"
	"strings"

	"dylaris-core/handlers"

	"github.com/gorilla/mux"
)

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
		handlers.SendJSONError(w, "Modpacks are disabled", http.StatusForbidden)
		return
	}
	// path.Clean resolves every ".." segment, so a cleaned key that starts with
	// modpacks/ cannot contain one and the prefix test alone is the boundary
	// (TestModpackMirrorServesBuiltPacksOnly). The explicit ".." check is a
	// second fence for the day the Clean moves; no input reaches it today.
	key := path.Clean(mux.Vars(r)["rest"])
	if strings.Contains(key, "..") || !strings.HasPrefix(key, "modpacks/") {
		handlers.SendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	prov, err := h.state.BuildModpackStorageProvider()
	if err != nil || prov == nil {
		handlers.SendJSONError(w, "Storage not configured", http.StatusInternalServerError)
		return
	}
	// Streamed rather than redirected, so the node only ever talks to the host
	// its MODPACK_MIRROR_HOSTS allowlist names - a redirect would hand it a
	// storage-bucket URL instead. And streamed rather than loaded whole: Core
	// used to hold the entire pack in memory once per concurrent request.
	if err := handlers.ServeModpackObject(w, r, prov, key, handlers.DeliverStream, "application/zip", path.Base(key)); err != nil {
		handlers.SendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
}
