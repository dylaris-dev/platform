package gateway

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"unicode/utf8"

	"dylaris-core/handlers/audit"
	"dylaris-core/handlers/httpx"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

const maxSuspendReason = 500

// SuspendRoute POST /api/gateway/routes/{domain}/suspend - body {"reason": "..."}, takes one route-only route off the edges until an admin lifts it. The route is withdrawn in the same request; a republisher pass that read the row just before can restore the key for at most one 60 s tick (leader only), and the next pass removes it.
func (h *GatewayHandler) SuspendRoute(w http.ResponseWriter, r *http.Request) {
	domain := mux.Vars(r)["domain"]
	var req struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON, expected {\"reason\": string}", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		httpx.SendJSONError(w, "A reason is required", http.StatusBadRequest)
		return
	}
	if utf8.RuneCountInString(reason) > maxSuspendReason {
		httpx.SendJSONError(w, "The reason is too long (500 characters at most)", http.StatusBadRequest)
		return
	}
	owner, err := h.d.Gateway.SuspendCoreOwnedRoute(domain, reason)
	if !h.suspendDone(w, domain, owner, err, audit.AuditEventGatewayRouteSuspended, reason, r) {
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "Route suspended"})
}

// UnsuspendRoute POST /api/gateway/routes/{domain}/unsuspend - lifts a suspension and publishes the route again right away.
func (h *GatewayHandler) UnsuspendRoute(w http.ResponseWriter, r *http.Request) {
	domain := mux.Vars(r)["domain"]
	owner, err := h.d.Gateway.UnsuspendCoreOwnedRoute(domain)
	if !h.suspendDone(w, domain, owner, err, audit.AuditEventGatewayRouteUnsuspended, "", r) {
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "Route unsuspended"})
}

// suspendDone answers the failure cases and writes the audit row; it reports
// whether the caller may send the success body.
func (h *GatewayHandler) suspendDone(w http.ResponseWriter, domain, owner string, err error, event, reason string, r *http.Request) bool {
	switch {
	case errors.Is(err, services.ErrRouteNotStored):
		httpx.SendJSONError(w, "Route not found", http.StatusNotFound)
		return false
	case errors.Is(err, services.ErrRouteAlreadySuspended), errors.Is(err, services.ErrRouteNotSuspended):
		httpx.SendJSONError(w, strings.ToUpper(err.Error()[:1])+err.Error()[1:]+".", http.StatusConflict)
		return false
	case errors.Is(err, services.ErrRouteChangeNotApplied):
		// The row changed, so the trail must say so; the republisher repairs the rest.
		log.Printf("route suspend %s: %v", domain, err)
		h.auditRouteChange(r, event, owner, domain, reason, err.Error())
		httpx.SendJSONError(w, "The change was saved but the gateway could not be updated yet. It will be retried automatically.", http.StatusInternalServerError)
		return false
	case errors.Is(err, services.ErrRouteNotCoreOwned):
		httpx.SendJSONError(w, "This is a managed server route. Only route-only routes can be suspended.", http.StatusConflict)
		return false
	case err != nil:
		log.Printf("route suspend %s: %v", domain, err)
		httpx.SendJSONError(w, "Failed to change the route", http.StatusInternalServerError)
		return false
	}
	h.auditRouteChange(r, event, owner, domain, reason, "")
	return true
}

func (h *GatewayHandler) auditRouteChange(r *http.Request, event, owner, domain, reason, failure string) {
	actorID, _ := r.Context().Value("userID").(string)
	meta := map[string]interface{}{"domain": domain}
	if reason != "" {
		meta["reason"] = reason
	}
	if failure != "" {
		meta["live_update_failed"] = failure
	}
	audit.LogIdentity(h.d.Store, r, event, actorID, owner, meta)
}
