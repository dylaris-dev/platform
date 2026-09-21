package handlers

import (
	"net/http"

	"dylaris-core/models"
)

// suspendedMessage is what a cut-off tenant is told, wherever they are stopped.
// One sentence for every path on purpose: the reason and the way out are the
// same whether they pressed Start or asked for an install.
const suspendedMessage = "Account suspended for non-payment. Settle payment to use your servers again."

// suspendedForNonPayment reports whether this request must be refused because
// the server's OWNER is suspended for non-payment.
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
func suspendedForNonPayment(state *AppState, r *http.Request, ownerID string) bool {
	if state == nil || state.Store == nil || ownerID == "" {
		return false
	}
	if r != nil && IsAdmin(r) {
		return false
	}
	b, err := state.Store.GetUserBilling(ownerID)
	return err == nil && b != nil && b.Status == "suspended"
}

// refuseIfSuspended is suspendedForNonPayment plus the 403, so a call site is
// one if-statement. It reports whether the request was answered.
func refuseIfSuspended(w http.ResponseWriter, r *http.Request, state *AppState, srv *models.Server) bool {
	if srv == nil || !suspendedForNonPayment(state, r, srv.OwnerID) {
		return false
	}
	sendJSONError(w, suspendedMessage, http.StatusForbidden)
	return true
}
