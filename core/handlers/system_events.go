package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"dylaris-core/authz"
	"dylaris-core/services"
)

// SystemEventsHandler exposes the SSE side of the system events channel.
// One handler per Core process — the panel opens one stream per session and
// receives broadcasts about regions/modules/features/maintenance/server-list
// changes so it can refresh cached state without polling.
type SystemEventsHandler struct {
	state *AppState
}

// mayReceive decides whether one broadcast frame goes to THIS subscriber.
//
// The channel is a single fleet-wide fan-out, so everything published on it
// reached every authenticated session. Most of that is harmless by design -
// the events are cache-invalidation signals whose payload is usually empty, and
// the panel re-fetches through the normal authorized routes. But a few name a
// serverId, and those were being delivered to people with no access to that
// server: measured on a live instance, a non-admin who owns one server received
// server_tabs.changed for a server belonging to someone else. No content and no
// credentials, but other tenants' identifiers and the timing of their activity.
//
// Fails OPEN, which is the opposite of the rule everywhere else in this file's
// neighbourhood and is deliberate. This is not an access decision - the data
// behind every one of these events is fetched over a route that authorizes it
// properly. Dropping a frame we could not classify would silently stop a panel
// from refreshing; forwarding one leaks an integer. So only a resolution that
// actually says "no" drops the frame.
func (h *SystemEventsHandler) mayReceive(r *http.Request, payload string) bool {
	userID, _ := r.Context().Value("userID").(string)
	username, _ := r.Context().Value("username").(string)
	isAdmin, _ := r.Context().Value("isAdmin").(bool)

	// A frame naming an ACCOUNT goes only to that account, and to admins.
	//
	// packs.changed carries the pack owner's id and users.changed the subject's,
	// and the filter below looks only at serverId - so both reached every
	// authenticated session, handing any account the user ids of everyone else
	// who touched a modpack, plus the timing of that activity.
	//
	// The owner id used to be worth more than that: it was the first path segment
	// of modpacks/<ownerID>/<slug>/<version>/pack.mrpack, which /solder/mirror
	// serves to anyone, unauthenticated, on purpose (a Technic launcher cannot
	// present a credential). It no longer is - PacksHandler.mrpackStorageKey
	// derives that directory from CLUSTER_SECRET, because the public Solder API
	// prints the same owner id in every mods[].url and made the old path
	// derivable anyway. This filter is not what carries that guarantee now, and
	// it stays regardless: broadcasting other accounts' user ids and the timing
	// of their activity is its own leak.
	//
	// Nothing loses a refresh: every subscriber in the panel ignores the payload
	// and re-fetches, and what it re-fetches is its OWN packs (listPacks) and its
	// OWN entitlement. An id that is not yours was never actionable.
	if subject := eventAccountID(payload); subject != "" && !isAdmin && subject != userID {
		return false
	}

	serverID := eventServerID(payload)
	if serverID == 0 {
		return true // no server named: a platform-wide signal, or an empty one
	}
	if h.state == nil || h.state.Authz == nil {
		return true
	}
	res, err := h.state.Authz.Resolve(authz.Identity{UserID: userID, Username: username, IsAdmin: isAdmin}, serverID)
	if err != nil {
		return true
	}
	// overview.read is "may this account see that this server exists at all",
	// which is exactly what a bare id discloses. Every invite carries it.
	return res.HasCap("overview.read")
}

// eventAccountID pulls the account an event is ABOUT out of its payload: the
// pack owner (ownerId) or the subject of a user change (userId). Returns "" when
// the frame names neither, which is the common case - most of these payloads are
// empty cache-invalidation signals.
//
// Both spellings are read here rather than one being normalised at the publish
// sites, because the point is to catch the id wherever it appears: a frame that
// names an account under a name this does not know is a frame that goes out
// unscoped, and there is nothing to fail on.
func eventAccountID(payload string) string {
	var ev struct {
		Payload struct {
			OwnerID string `json:"ownerId"`
			UserID  string `json:"userId"`
		} `json:"payload"`
	}
	if json.Unmarshal([]byte(payload), &ev) != nil {
		return ""
	}
	if ev.Payload.OwnerID != "" {
		return ev.Payload.OwnerID
	}
	return ev.Payload.UserID
}

// eventServerID pulls the serverId out of an event payload, or 0 when it names
// none. JSON numbers decode as float64; anything else is treated as absent
// rather than guessed at.
func eventServerID(payload string) int {
	var ev struct {
		Payload struct {
			ServerID *float64 `json:"serverId"`
		} `json:"payload"`
	}
	if json.Unmarshal([]byte(payload), &ev) != nil || ev.Payload.ServerID == nil {
		return 0
	}
	return int(*ev.Payload.ServerID)
}

func NewSystemEventsHandler(state *AppState) *SystemEventsHandler {
	return &SystemEventsHandler{state: state}
}

// StreamEvents GET /api/system/events
//
// SSE stream subscribed to Redis Pub/Sub channel `dylaris:system:events`.
// Sends a `hello` event immediately so the client can flip into "connected"
// state without waiting for the first real event. Keepalive comment frames
// every 15s keep the connection alive across proxies that idle-close at 60s.
func (h *SystemEventsHandler) StreamEvents(w http.ResponseWriter, r *http.Request) {
	if h.state.Redis == nil {
		sendJSONError(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		sendJSONError(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	fmt.Fprintf(w, "data: {\"type\":\"hello\"}\n\n")
	flusher.Flush()

	// This stream is the platform's definition of "a user is here now" - see
	// presence.go for why that is counted in Redis rather than in memory.
	userID, _ := r.Context().Value("userID").(string)
	defer presenceTrack(r.Context(), h.state.Redis, h.state.CoreID, userID)()

	pubsub := h.state.Redis.Subscribe(r.Context(), services.SystemEventsChannel)
	defer pubsub.Close()

	ch := pubsub.Channel()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if !h.mayReceive(r, msg.Payload) {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", msg.Payload)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
