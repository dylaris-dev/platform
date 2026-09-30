package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"dylaris-core/pkg/crypto"

	"github.com/gorilla/mux"
)

// libraryMirrorTTL bounds how long a library download URL handed to a node
// stays valid. Long, because the install command waits in the node's queue
// while the node is offline, and the URL is only ever read by that node.
const libraryMirrorTTL = 24 * time.Hour

// libraryMirrorSig binds a library path to an expiry. Only Core can make one,
// so a node - or anyone who sees the URL - can fetch that one file and no other.
func libraryMirrorSig(secret string, exp int64, libPath string) string {
	mac := hmac.New(sha256.New, crypto.DeriveKey(secret, "library-mirror"))
	fmt.Fprintf(mac, "%d\x1f%s", exp, libPath)
	return hex.EncodeToString(mac.Sum(nil))
}

// libraryMirrorURL is where a node downloads one library file from. The path
// goes last so the node can still tell a .zip from a .jar by its suffix.
func libraryMirrorURL(base, secret, libPath string, now time.Time) string {
	exp := now.Add(libraryMirrorTTL).Unix()
	segs := strings.Split(libPath, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return fmt.Sprintf("%slibrary/%d/%s/%s", base, exp, libraryMirrorSig(secret, exp, libPath), strings.Join(segs, "/"))
}

// canonicalLibraryPath accepts a library path only in the one spelling the
// denylist compares against: plain segments, no ".", "..", empty segment or
// backslash. The storage backends clean a path themselves, so "a/./b.jar"
// found the file while the denylist check walked a different string and let a
// disabled file through.
func canonicalLibraryPath(raw string) (string, bool) {
	p := normalizeLibraryPath(raw)
	if p == "" || strings.Contains(p, "\\") {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	return p, true
}

// resolveLibraryInstall turns the library path a setup names into the URL the
// node downloads it from. The library lives in Core's storage, never on the
// node: the node used to be sent the path itself and copied whatever it found
// there on its OWN disk. Every check the browse and download handlers make is
// made here too, because this is a third way to get a library file.
func resolveLibraryInstall(ctx context.Context, state *AppState, rawPath string, isAdmin bool) (string, int, string) {
	if !state.FeatureFlags.IsLibraryEnabled(ctx) {
		return "", http.StatusForbidden, "The library is disabled"
	}
	libPath, ok := canonicalLibraryPath(rawPath)
	if !ok {
		return "", http.StatusBadRequest, "Invalid library path"
	}
	lh := &LibraryHandler{state: state}
	if !isAdmin {
		disabled, err := lh.disabledPathSet()
		if err != nil {
			return "", http.StatusServiceUnavailable, "Could not verify library access"
		}
		if isPathBlocked(libPath, disabled) {
			return "", http.StatusForbidden, "Access denied"
		}
	}
	base, err := modpackMirrorBase(state.Store.GetSetting)
	if err != nil {
		return "", http.StatusConflict, "Core has no public address set, so a node cannot download from the library"
	}
	prov, err := state.buildCoreStorageProvider(CoreStoragePrefixLibrary)
	if err != nil {
		return "", http.StatusServiceUnavailable, "Library storage is unavailable"
	}
	// Listed rather than opened: opening a directory succeeds on the local
	// backend, and the node would install an empty server.jar and call it done.
	dir := path.Dir(libPath)
	if dir == "." {
		dir = "/"
	}
	entries, err := prov.ListFiles(ctx, dir)
	found := false
	for _, e := range entries {
		if e.Name == path.Base(libPath) && !e.IsDir {
			found = true
		}
	}
	if err != nil || !found {
		return "", http.StatusNotFound, "Library file not found"
	}
	return libraryMirrorURL(base, state.ClusterSecret, libPath, time.Now()), 0, ""
}

// LibraryMirror GET /mirror/library/{exp}/{sig}/{path} - streams one library
// file to a node installing it. Unauthenticated like the pack mirror, because
// the node fetches with a plain GET; the signature is the credential and names
// exactly one path. Every refusal is the same 404.
func (h *LibraryHandler) LibraryMirror(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	exp, err := strconv.ParseInt(vars["exp"], 10, 64)
	libPath, ok := canonicalLibraryPath(vars["rest"])
	if err != nil || time.Now().Unix() > exp || !ok ||
		!h.state.FeatureFlags.IsLibraryEnabled(r.Context()) ||
		!hmac.Equal([]byte(vars["sig"]), []byte(libraryMirrorSig(h.state.ClusterSecret, exp, libPath))) {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	prov, err := h.state.buildCoreStorageProvider(CoreStoragePrefixLibrary)
	if err != nil {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	// Streamed, never redirected to a pre-signed URL: the node only accepts
	// Core's own host.
	rc, err := prov.GetFile(r.Context(), libPath)
	if err != nil {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, rc)
}
