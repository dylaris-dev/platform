package guard

import (
	"net/http"
	"time"

	"dylaris-core/handlers/httpx"
	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// SuspendedMessage is what a cut-off tenant is told, wherever they are stopped.
// One sentence for every path on purpose: the reason and the way out are the
// same whether they pressed Start or asked for an install.
const SuspendedMessage = "Account suspended for non-payment. Settle payment to use your servers again."

// overLimitMessage is the same refusal for a tenant cut off for holding more
// than they bought, whose way out is a different one.
const overLimitMessage = "Account suspended: it holds more than its plan includes. Remove what is over the limit or upgrade to use your servers again."

// OwnerRefusal reports whether this request must be refused because the
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
func OwnerRefusal(st store.Store, grace time.Duration, r *http.Request, ownerID string) string {
	if st == nil || ownerID == "" {
		return ""
	}
	if r != nil && reqctx.OperatorOverride(r) {
		return ""
	}
	b, err := st.GetUserBilling(ownerID)
	if err != nil || b == nil {
		return ""
	}
	if b.Status == "suspended" {
		return SuspendedMessage
	}
	if store.OwnerCutOff(b, grace, services.OverLimitGrace, time.Now()) {
		return overLimitMessage
	}
	return ""
}

// RefuseIfSuspended is OwnerRefusal plus the 403, so a call site is
// one if-statement. It reports whether the request was answered.
func RefuseIfSuspended(w http.ResponseWriter, r *http.Request, st store.Store, grace time.Duration, srv *models.Server) bool {
	if srv == nil {
		return false
	}
	msg := OwnerRefusal(st, grace, r, srv.OwnerID)
	if msg == "" {
		return false
	}
	httpx.SendJSONError(w, msg, http.StatusForbidden)
	return true
}
