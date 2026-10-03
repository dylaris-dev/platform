package handlers

import (
	"net/http"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// suspendedMessage is what a cut-off tenant is told, wherever they are stopped.
// One sentence for every path on purpose: the reason and the way out are the
// same whether they pressed Start or asked for an install.
const suspendedMessage = "Account suspended for non-payment. Settle payment to use your servers again."

// overLimitMessage is the same refusal for a tenant cut off for holding more
// than they bought, whose way out is a different one.
const overLimitMessage = "Account suspended: it holds more than its plan includes. Remove what is over the limit or upgrade to use your servers again."

// ownerRefusal reports whether this request must be refused because the
// server's OWNER is cut off - with the message that goes with it, "" when the
// request may go ahead.
//
// Suspension used to be enforced in exactly one place, on start and restart.
// That was measured on production and it does not hold: a suspended tenant
// called POST /servers/{id}/setup, the node installed and BOOTED the server,
// and the account was serving players again - the customer undoing their own
// cutoff with one call. The guard therefore belongs on every path that makes a
// node do work for the tenant, not only on the button labelled Start.
//
// What stays open is deliberate: reading, downloading their own files, opening
// a ticket, deleting things. A cutoff is not a lockout - they must be able to
// see what they are paying for and to leave. Deleting in particular frees
// resources and must never be blocked.
//
// past_due (the grace window) is unaffected, and an admin passes through so
// support keeps control of a suspended tenant's machines.
//
// Over-limit is the other cutoff and is refused the same way. The hourly pass
// stopped an over-limit tenant's servers and every gate here let them start
// again straight away, until the next pass - every hour, for as long as they
// cared to press Start.
func ownerRefusal(state *AppState, r *http.Request, ownerID string) string {
	if state == nil || state.Store == nil || ownerID == "" {
		return ""
	}
	if r != nil && operatorOverride(r) {
		return ""
	}
	b, err := state.Store.GetUserBilling(ownerID)
	if err != nil || b == nil {
		return ""
	}
	if b.Status == "suspended" {
		return suspendedMessage
	}
	if store.OwnerCutOff(b, state.SuspendGrace, services.OverLimitGrace, time.Now()) {
		return overLimitMessage
	}
	return ""
}

// refuseIfSuspended is ownerRefusal plus the 403, so a call site is
// one if-statement. It reports whether the request was answered.
// operatorOverride reports whether an operator's bypass of a tenant-facing
// guard applies to this request: an admin in a panel session, never an API key.
// The panel asks an operator to confirm these overrides; a key has nobody to
// ask, so an automation script holding an operator's key started suspended
// customers' servers and acted inside installs and moves without a word.
func operatorOverride(r *http.Request) bool {
	return IsAdmin(r) && APIKeyFromContext(r) == nil
}

func refuseIfSuspended(w http.ResponseWriter, r *http.Request, state *AppState, srv *models.Server) bool {
	if srv == nil {
		return false
	}
	msg := ownerRefusal(state, r, srv.OwnerID)
	if msg == "" {
		return false
	}
	sendJSONError(w, msg, http.StatusForbidden)
	return true
}
