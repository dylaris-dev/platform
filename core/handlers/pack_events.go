package handlers

import (
	"log"
	"net/http"
)

// publishPackEvent sends a pack event to the pack's owner only. It names the
// owner in the payload, which is what the events stream filters on.
//
// These events used to carry only the pack or build id, so every signed-in
// session received the ids of every tenant's packs and the moment each one was
// edited - the leak packs.changed was already fixed for. A pack whose owner
// cannot be read is not announced at all: the page that would have refreshed
// simply does not, which is the safe way to be wrong.
func publishPackEvent(r *http.Request, state *AppState, event string, packID int, payload map[string]interface{}) {
	if state == nil || state.Store == nil || state.Events == nil {
		return
	}
	pack, err := state.Store.GetPack(packID)
	if err != nil || pack == nil || pack.OwnerID == "" {
		log.Printf("events: %s for pack %d not sent: owner unknown (%v)", event, packID, err)
		return
	}
	payload["ownerId"] = pack.OwnerID
	state.Events.Publish(r.Context(), event, payload)
}
