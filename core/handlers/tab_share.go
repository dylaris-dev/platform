package handlers

import (
	"context"
	"crypto/rand"
	"database/sql"
	"dylaris-core/store"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"dylaris-pkg/validate"

	"github.com/gorilla/mux"
	"github.com/lib/pq"
)

// shareTokenAlphabet is URL-safe base62; a 16-char token carries ~95 bits of
// entropy, generated with crypto/rand and rejection sampling (no modulo bias).
const shareTokenAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
const shareTokenLen = 16

func generateShareToken() (string, error) {
	// Reject bytes >= the largest multiple of 62 that fits in a byte so every
	// alphabet symbol is equiprobable.
	const limit = 256 - (256 % len(shareTokenAlphabet))
	out := make([]byte, shareTokenLen)
	var b [1]byte
	for i := 0; i < shareTokenLen; {
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		if int(b[0]) >= limit {
			continue
		}
		out[i] = shareTokenAlphabet[int(b[0])%len(shareTokenAlphabet)]
		i++
	}
	return string(out), nil
}

// proxyHostLabelAlphabet is lowercase base36, and the lowercase is the point.
//
// The share token next to it is base62 WITH uppercase, which is fine for a URL
// path and wrong for a hostname: DNS labels are case-insensitive, so two tokens
// differing only in case would resolve to the same host and the proxy would have
// to guess which tab a request meant. A separate alphabet makes that impossible
// rather than unlikely.
//
// 20 characters of base36 is about 103 bits, and the alphabet keeps the result a
// valid DNS label by construction: alphanumeric only, so it can never start or
// end with a hyphen, and 20 is far inside the 63-character limit.
const proxyHostLabelAlphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
const proxyHostLabelLen = 20

// generateProxyHostLabel mints the hostname label a proxied tab is served on.
// Same rejection sampling as generateShareToken, for the same reason: a plain
// modulo would make the first few symbols of the alphabet more likely.
func generateProxyHostLabel() (string, error) {
	const limit = 256 - (256 % len(proxyHostLabelAlphabet))
	out := make([]byte, proxyHostLabelLen)
	var b [1]byte
	for i := 0; i < proxyHostLabelLen; {
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		if int(b[0]) >= limit {
			continue
		}
		out[i] = proxyHostLabelAlphabet[int(b[0])%len(proxyHostLabelAlphabet)]
		i++
	}
	return string(out), nil
}

// isProxyHostLabel reports whether s is one of our labels. Used on the request
// path before any database lookup, so a hostile Host header cannot reach the
// query at all.
func isProxyHostLabel(s string) bool {
	if len(s) != proxyHostLabelLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}

// normalizeTabSubServer trims and validates the sub-server a tab is pinned to.
// A nil pointer and an empty string both mean "every sub-server"; the caller
// tells those apart itself when it matters (a PATCH), because only there does
// the difference between "keep" and "clear" exist.
func normalizeTabSubServer(v *string) (string, error) {
	if v == nil {
		return "", nil
	}
	name := strings.TrimSpace(*v)
	if name == "" {
		return "", nil
	}
	if !validate.IsSubServerName(name) {
		return "", errors.New("Invalid sub-server name")
	}
	return name, nil
}

// customShareSlugPattern is what a user may choose for their own link.
//
// Narrower than the URL would allow, and on purpose: the slug is typed by
// humans, read aloud, and pasted into chat clients that guess where a link
// ends. Lowercase alphanumerics and single inner hyphens survive all of that.
// The 4-character floor is not security - a chosen slug is guessable by
// definition - it just stops "a" and "x" being taken by the first two people.
var customShareSlugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

const (
	customShareSlugMin = 4
	customShareSlugMax = 40
)

// validateCustomShareSlug checks a user-chosen link slug.
//
// A caller that supplies one is trading unguessability for readability, which
// is theirs to trade for a PUBLIC link - that link is meant to be handed out.
// It changes nothing for a private link: those are gated by the ticket, not by
// the slug.
func validateCustomShareSlug(v string) (string, error) {
	slug := strings.ToLower(strings.TrimSpace(v))
	if slug == "" {
		return "", nil
	}
	if len(slug) < customShareSlugMin || len(slug) > customShareSlugMax {
		return "", fmt.Errorf("a custom link must be between %d and %d characters", customShareSlugMin, customShareSlugMax)
	}
	if !customShareSlugPattern.MatchString(slug) {
		return "", errors.New("a custom link may use lowercase letters, digits and single hyphens between them")
	}
	return slug, nil
}

// isShareSlugTaken reports a Postgres unique-constraint violation (23505).
//
// The store package has its own copy of this and keeps it unexported on
// purpose - callers there see ErrNameTaken, never the driver's class. This
// handler talks to the database directly (RawDB), so it needs its own.
func isShareSlugTaken(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

// capReached reports whether an additive operation would exceed a max cap.
func capReached(current, max int) bool {
	return current >= max
}

// ensureProxyHostLabel gives a tab a content host if it does not have one.
//
// Idempotent on purpose. A label is public the moment somebody copies the link,
// so re-minting it on an ordinary edit would silently break every link already
// in circulation. Only the absence of one is filled in.
func (h *ServerTabsHandler) ensureProxyHostLabel(db *sql.DB, tabID int) error {
	var existing sql.NullString
	if err := db.QueryRow(`SELECT proxy_host_label FROM server_tabs WHERE id=$1`, tabID).Scan(&existing); err != nil {
		return err
	}
	if existing.Valid && existing.String != "" {
		return nil
	}
	label, err := generateProxyHostLabel()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE server_tabs SET proxy_host_label=$2 WHERE id=$1`, tabID, label)
	return err
}

// countUserProxiedTabsOnServer counts what ONE user holds on ONE server.
//
// Counting the server instead would let whoever created a tab first spend the
// allowance of everyone else who shares that server, and the limit exists to
// bound an account, not a machine.
func (h *ServerTabsHandler) countUserProxiedTabsOnServer(db *sql.DB, serverID int, userID string) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM server_tabs
		WHERE server_id=$1 AND mode='proxied' AND created_by=$2`, serverID, userID).Scan(&n)
	return n, err
}

// countUserProxiedTabsTotal counts a user's proxied tabs everywhere.
//
// Without this the per-server cap is not a ceiling at all: a user with twenty
// servers holds twenty times it.
func (h *ServerTabsHandler) countUserProxiedTabsTotal(db *sql.DB, userID string) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM server_tabs
		WHERE mode='proxied' AND created_by=$1`, userID).Scan(&n)
	return n, err
}

// tabCapExceeded applies both caps and names the one that stopped you. A
// message that says only "limit reached" sends the user looking on the wrong
// screen.
func (h *ServerTabsHandler) tabCapExceeded(db *sql.DB, ctx context.Context, serverID int, userID string) string {
	if userID == "" {
		return ""
	}
	if n, err := h.countUserProxiedTabsTotal(db, userID); err == nil &&
		capReached(n, h.state.FeatureFlags.TabProxyMaxPerUserTotal(ctx)) {
		return "You have reached your total limit of proxied tabs across all servers."
	}
	if n, err := h.countUserProxiedTabsOnServer(db, serverID, userID); err == nil &&
		capReached(n, h.state.FeatureFlags.TabProxyMaxPerUserPerServer(ctx)) {
		return "You have reached your limit of proxied tabs on this server."
	}
	return ""
}

func (h *ServerTabsHandler) countUserShareLinks(db *sql.DB, userID string) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM server_tabs WHERE created_by=$1 AND share_token IS NOT NULL`, userID).Scan(&n)
	return n, err
}

// RotateShareLink POST /api/servers/{id}/tabs/{tabId}/share-link - (re)generate
// the unguessable slug for a proxied page tab.
func (h *ServerTabsHandler) RotateShareLink(w http.ResponseWriter, r *http.Request) {
	// An empty body still means "give me a random one", which is what the
	// button did before a chosen slug existed.
	var req struct {
		Slug string `json:"slug"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	custom, verr := validateCustomShareSlug(req.Slug)
	if verr != nil {
		sendJSONError(w, verr.Error(), http.StatusBadRequest)
		return
	}
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	if !h.serverExists(serverID) {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	tabID, _ := strconv.Atoi(mux.Vars(r)["tabId"])
	db := h.db()
	if db == nil {
		sendJSONError(w, "DB unavailable", http.StatusInternalServerError)
		return
	}
	var mode, surface, owner string
	var existing sql.NullString
	if err := db.QueryRow(`SELECT mode, surface, share_token, COALESCE(created_by::text,'')
		FROM server_tabs WHERE id=$1 AND server_id=$2`,
		tabID, serverID).Scan(&mode, &surface, &existing, &owner); err != nil {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	if mode != "proxied" || (surface != "page" && surface != "both") {
		sendJSONError(w, "Share links only apply to proxied page tabs", http.StatusBadRequest)
		return
	}
	// Minting a slug where there was none ADDS to the allowance Create checks
	// and this handler did not, so the per-user cap was one PATCH away from
	// being irrelevant: create the tab with surface "tab" (no slug, no check),
	// PATCH it to "page", rotate. Re-rolling an EXISTING slug is not a new
	// link and stays allowed at the cap.
	//
	// The count is against created_by, not the caller: that is the column
	// countUserShareLinks measures and the one this row will keep, so counting
	// anyone else would bill the wrong allowance.
	if !existing.Valid || existing.String == "" {
		if owner != "" {
			if used, uerr := h.countUserShareLinks(db, owner); uerr == nil &&
				capReached(used, h.state.FeatureFlags.TabProxyMaxShareLinksPerUser(r.Context())) {
				sendJSONError(w, "This tab's owner has reached their share-link limit.", http.StatusConflict)
				return
			}
		}
	}
	tok := custom
	if tok == "" {
		var terr error
		if tok, terr = generateShareToken(); terr != nil {
			sendJSONError(w, "Failed to generate share token", http.StatusInternalServerError)
			return
		}
	}
	// Rotating hands the owner a NEW link, so an expiry that has ALREADY run
	// out is dropped with the slug it belonged to - keeping it would mint a
	// link that is dead the moment it is copied, which reads as a broken
	// button. A future expiry is the owner's live choice and survives.
	// The content host moves with the link. Rotating is what an owner does to
	// lock out the people who had the old link, and the host they had been
	// sent to - in the old link and in every page it served - stayed open.
	label, lerr := generateProxyHostLabel()
	if lerr != nil {
		sendJSONError(w, "Failed to rotate the link", http.StatusInternalServerError)
		return
	}
	res, err := store.RotateTabShare(db, tabID, serverID, tok, label)
	if err != nil {
		// The unique index on share_token is what decides this, not a SELECT
		// before the write: two people picking the same slug at the same moment
		// would both pass a check-then-insert and one would silently take the
		// other's link.
		if isShareSlugTaken(err) {
			sendJSONError(w, "That link is already taken. Pick another.", http.StatusConflict)
			return
		}
		sendJSONError(w, "Failed to save share link", http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	h.state.Events.Publish(r.Context(), "server_tabs.changed", map[string]interface{}{"serverId": serverID})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "shareToken": tok})
}

// RevokeShareLink DELETE /api/servers/{id}/tabs/{tabId}/share-link - null the
// slug so the standalone page 404s. The tab itself stays.
func (h *ServerTabsHandler) RevokeShareLink(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	if !h.serverExists(serverID) {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	tabID, _ := strconv.Atoi(mux.Vars(r)["tabId"])
	db := h.db()
	if db == nil {
		sendJSONError(w, "DB unavailable", http.StatusInternalServerError)
		return
	}
	res, err := db.Exec(`UPDATE server_tabs SET share_token=NULL WHERE id=$1 AND server_id=$2`, tabID, serverID)
	if err != nil {
		sendJSONError(w, "Failed to revoke share link", http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	h.state.Events.Publish(r.Context(), "server_tabs.changed", map[string]interface{}{"serverId": serverID})
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
