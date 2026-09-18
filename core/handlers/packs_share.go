package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"dylaris-core/models"
	"dylaris-core/storage/modpack"

	"github.com/gorilla/mux"
)

type createShareLinkRequest struct {
	Kind          string `json:"kind"`
	ExpiresInDays int    `json:"expiresInDays,omitempty"`
}

// CreateShareLink POST /api/packs/{id}/builds/{buildId}/share-link - mints a
// share token for one build, so it can be downloaded without a session.
func (h *PacksHandler) CreateShareLink(w http.ResponseWriter, r *http.Request) {
	b, ok := h.loadOwnedBuild(r)
	if !ok {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	userID, _ := r.Context().Value("userID").(string)
	var req createShareLinkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Kind != models.ShareLinkClientMrpack && req.Kind != models.ShareLinkServerPack {
		sendJSONError(w, "kind must be client-mrpack or server-pack", http.StatusBadRequest)
		return
	}
	token, err := generatePlaintextKey()
	if err != nil {
		sendJSONError(w, "Failed to mint token", http.StatusInternalServerError)
		return
	}
	link := &models.ShareLink{
		BuildID:   b.ID,
		Kind:      req.Kind,
		Token:     token,
		CreatedBy: userID,
	}
	if req.ExpiresInDays > 0 {
		exp := time.Now().Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour)
		link.ExpiresAt = &exp
	}
	id, err := h.state.Store.CreateShareLink(link)
	if err != nil {
		sendJSONError(w, "Failed to create share link", http.StatusInternalServerError)
		return
	}
	link.ID = id
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "link": link})
}

// ListShareLinks GET /api/packs/{id}/builds/{buildId}/share-links - the share
// links issued for one build.
func (h *PacksHandler) ListShareLinks(w http.ResponseWriter, r *http.Request) {
	b, ok := h.loadOwnedBuild(r)
	if !ok {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	links, err := h.state.Store.ListShareLinksByBuild(b.ID)
	if err != nil {
		sendJSONError(w, "Failed to list share links", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "links": links})
}

// RevokeShareLink DELETE /api/packs/{id}/builds/{buildId}/share-links/{linkId}
// - revokes one share link. Owning the pack and build is proven first, and the
// revoke itself is additionally scoped by creator, so one owner cannot revoke
// another's link.
func (h *PacksHandler) RevokeShareLink(w http.ResponseWriter, r *http.Request) {
	// loadOwnedBuild proves the caller owns the pack+build in the path; the store
	// revoke is additionally created_by-scoped, so cross-user revoke is blocked.
	if _, ok := h.loadOwnedBuild(r); !ok {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	userID, _ := r.Context().Value("userID").(string)
	linkID, _ := strconv.Atoi(mux.Vars(r)["linkId"])
	if err := h.state.Store.RevokeShareLink(linkID, userID); err != nil {
		sendJSONError(w, "Share link not found", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// ServeShare GET /api/share/{token} is PUBLIC, unauthenticated. Registered on the
// root router as a sibling of /mirror so it bypasses setup-lock, maintenance, and
// auth (the token is the credential). The modpacks feature is gated in-handler.
func (h *PacksHandler) ServeShare(w http.ResponseWriter, r *http.Request) {
	// Uniform 404 for every negative case (feature off, unknown/revoked/expired
	// token, missing build/pack) so the endpoint is not a validity oracle.
	notFound := func() { sendJSONError(w, "Not found", http.StatusNotFound) }
	if !h.state.FeatureFlags.IsModpacksEnabled(r.Context()) {
		notFound()
		return
	}
	token := mux.Vars(r)["token"]
	link, err := h.state.Store.GetShareLinkByToken(token)
	if err != nil || link == nil || link.Revoked {
		notFound()
		return
	}
	if link.ExpiresAt != nil && time.Now().After(*link.ExpiresAt) {
		notFound()
		return
	}
	build, err := h.state.Store.GetPackBuild(link.BuildID)
	if err != nil || build == nil {
		notFound()
		return
	}
	pack, err := h.state.Store.GetPack(build.PackID)
	if err != nil || pack == nil {
		notFound()
		return
	}

	switch link.Kind {
	case models.ShareLinkClientMrpack:
		key, err := h.ensureInstallMrpack(r.Context(), pack, build)
		if err != nil {
			sendJSONError(w, "Failed to render pack", http.StatusInternalServerError)
			return
		}
		prov, err := h.state.buildModpackStorageProvider()
		if err != nil || prov == nil {
			sendJSONError(w, "Storage unavailable", http.StatusInternalServerError)
			return
		}
		// deliverRedirect is safe here where the pack mirror streams instead:
		// this link is opened by a browser, and browsers follow a 302, while a
		// node downloads from the mirror against a host allowlist that a
		// storage-bucket redirect would step outside of. On an
		// S3-backed modpack storage the pack then never enters this process
		// at all.
		filename := pack.InternalSlug + "-" + build.VersionString + ".mrpack"
		if err := serveModpackObject(w, r, prov, key, deliverRedirect, "application/x-modrinth-modpack+zip", filename); err != nil {
			// A missing object is a 404 like every other absent resource on
			// this route, not a 500. The route's documented shape is a uniform
			// 404 so a token cannot be probed by status code, and answering 500
			// for a deleted pack broke both that and the helper's own contract.
			if errors.Is(err, modpack.ErrNotFound) {
				notFound()
				return
			}
			sendJSONError(w, "Failed to read pack", http.StatusInternalServerError)
			return
		}

	case models.ShareLinkServerPack:
		content, err := h.state.Store.ListBuildContent(build.ID)
		if err != nil {
			sendJSONError(w, "Failed to load content", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", pack.InternalSlug+"-"+build.VersionString+"-server.zip"))
		// The pack streams straight to the client so Core never holds the whole
		// archive in memory. cw tracks whether any byte has been sent: a render
		// error before the first byte can still be a clean 500 (the headers set
		// above are only sent on the first write), while a failure partway
		// through can only be logged - the status is already committed. The
		// security checks inside renderServerPack run before each entry is
		// written, so a rejected entry never reaches the client.
		cw := &countingWriter{w: w}
		if err := h.renderServerPack(r.Context(), content, cw); err != nil {
			if cw.written == 0 {
				sendJSONError(w, "Failed to render server pack", http.StatusInternalServerError)
			} else {
				log.Printf("share: server-pack render failed for %s after %d bytes: %v", pack.InternalSlug, cw.written, err)
			}
			return
		}

	default:
		notFound()
	}
}
