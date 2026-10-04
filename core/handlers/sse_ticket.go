package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// sseTicketTTL is the lifetime of an SSE auth ticket. AuthMiddleware also
// uses this value when refreshing the ticket on each accepted SSE request so
// long-lived streams (and native EventSource reconnects) keep the same ticket
// valid as a sliding window.
const sseTicketTTL = 5 * time.Minute

// sseTicketSep separates the username from the password fingerprint in a
// ticket's stored value. Neither can contain it.
const sseTicketSep = ""

// sseTicketPaths are the only routes a ticket opens. They are the streams an
// EventSource reads, which is the one reason a credential has to ride in a URL
// at all. A ticket used to be accepted on EVERY GET behind AuthMiddleware -
// file reads, server lists, admin reads - and slid forward on each one, so a
// ticket somebody got hold of was a read-everything credential for as long as
// they kept using it.
var sseTicketPaths = regexp.MustCompile(`^/api/(system/events|servers/[0-9]+/(console|stats)/stream)$`)

// sseMaxStreamAge ends every stream after this long. The browser reconnects on
// its own, and the reconnect goes through authentication and the capability
// check again. A stream was checked once, when it opened, and then ran until
// the client left - through a revoked permission, a removal from the server,
// a password change, for days.
var sseMaxStreamAge = 5 * time.Minute

// streamContext bounds one stream's lifetime.
func streamContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), sseMaxStreamAge)
}

// sseEventID is a Redis stream ID, the only Last-Event-ID a console resume
// accepts.
var sseEventID = regexp.MustCompile(`^[0-9]{1,20}-[0-9]{1,20}$`)

// parseSSETicket splits a stored ticket value. A ticket minted before the
// fingerprint existed has none, and is honoured once without being extended,
// so a stream open across the deploy reconnects instead of going silent.
func parseSSETicket(val string) (username, fp string, legacy bool) {
	if i := strings.Index(val, sseTicketSep); i >= 0 {
		return val[:i], val[i+len(sseTicketSep):], false
	}
	return val, "", true
}

// MintSSETicket issues a short-lived, single-purpose ticket that an
// EventSource can carry in the URL (?ticket=) instead of the long-lived
// session JWT. EventSource cannot set an Authorization header, so the only
// way to authenticate an SSE stream is via the URL — and putting the session
// JWT there leaks it into access logs / Referer. The ticket is random,
// expires in 5 minutes, and only maps back to a username inside Redis.
//
// Registered under AuthMiddleware, so the caller authenticates with the
// normal Bearer header to obtain the ticket.
func (h *AuthHandler) MintSSETicket(w http.ResponseWriter, r *http.Request) {
	if h.state.Redis == nil {
		sendJSONError(w, "Ticket store unavailable", http.StatusServiceUnavailable)
		return
	}

	username, _ := r.Context().Value("username").(string)
	if username == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	// The ticket carries the fingerprint of the password it was minted under,
	// so a password change ends it the way it ends a session token.
	fp := ""
	if h.state.Store != nil {
		user, err := h.state.Store.GetUserByUsername(username)
		if err != nil || user == nil {
			sendJSONError(w, "Could not verify account", http.StatusServiceUnavailable)
			return
		}
		fp = passwordFingerprint(sessionKey(user))
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		sendJSONError(w, "Failed to generate ticket", http.StatusInternalServerError)
		return
	}
	ticket := hex.EncodeToString(buf)

	if err := h.state.Redis.Set(r.Context(), "sse:ticket:"+ticket, username+sseTicketSep+fp, sseTicketTTL).Err(); err != nil {
		sendJSONError(w, "Failed to store ticket", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"ticket": ticket})
}
