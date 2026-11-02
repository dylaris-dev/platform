package packs

import (
	"net/http"
	"path"
	"strings"
	"time"

	"dylaris-core/handlers/httpx"
	"dylaris-core/services/corestorage"

	"github.com/gorilla/mux"
)

// ModpackMirror streams a rendered pack .mrpack to the node installing it.
//
// Unauthenticated, because the node fetches it with a plain GET and no
// credential. The URL carries an expiring signature Core made for that one key
// (corestorage.SignedModpackMirrorURL), and only a build's pack.mrpack is ever
// served: the unguessable path alone used to be the credential, with no expiry,
// which made every install URL a permanent public download. Every refusal is
// the same 404, so the route is not an oracle for which keys exist.
func (h *PacksHandler) ModpackMirror(w http.ResponseWriter, r *http.Request) {
	notFound := func() { httpx.SendJSONError(w, "Not found", http.StatusNotFound) }
	if !h.state.FeatureFlags.IsModpacksEnabled(r.Context()) {
		httpx.SendJSONError(w, "Modpacks are disabled", http.StatusForbidden)
		return
	}
	// path.Clean resolves every ".." segment, so a cleaned key that starts with
	// modpacks/ cannot contain one; the explicit check is a second fence.
	key := path.Clean(mux.Vars(r)["rest"])
	if strings.Contains(key, "..") || !strings.HasPrefix(key, "modpacks/") || !strings.HasSuffix(key, "/pack.mrpack") {
		notFound()
		return
	}
	q := r.URL.Query()
	if !corestorage.ValidModpackMirrorSig(h.state.ClusterSecret, key, q.Get("exp"), q.Get("sig"), time.Now()) {
		notFound()
		return
	}
	prov, err := h.state.CoreStorage.BuildModpackProvider()
	if err != nil || prov == nil {
		httpx.SendJSONError(w, "Storage not configured", http.StatusInternalServerError)
		return
	}
	// Streamed rather than redirected, so the node only ever talks to the host
	// it was told to trust - a redirect would hand it a storage-bucket URL.
	if err := serveModpackObject(w, r, prov, key, deliverStream, "application/zip", path.Base(key)); err != nil {
		notFound()
		return
	}
}
