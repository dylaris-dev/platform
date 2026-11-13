package gate

import (
	"context"
	"dylaris-core/services"
	"dylaris-core/services/remotegw"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// RequireGatewayEnabled blocks the request with 503 gateway_disabled when the
// platform routing mode is ip_port. Wrap gateway-route WRITE endpoints with
// this so they refuse instead of silently accepting no-op work that the
// (absent) gateway would never apply. Reads + DELETE stay ungated: reads are
// harmless and DELETE must work for cleanup after switching back to ip_port.
func (s Gates) RequireGatewayEnabled(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !services.GatewayRoutingEnabled(s.Store) {
			GatewayDisabledResponse(w)
			return
		}
		next(w, r)
	}
}

// RequireLocalGateway refuses a route that only works against this platform's
// own gateway state (Redis, Hub, warp) while the managed gateway is the
// provider: 409 gateway_remote, or 503 gateway_provider_unknown while the mode
// cannot be read. A local platform passes through untouched.
func (s Gates) RequireLocalGateway(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if GatewayModeError(w, services.LocalGatewayErr(s.GatewayRemote)) {
			return
		}
		next(w, r)
	}
}

// GatewayModeError writes the answer for a gateway-mode sentinel in err and
// reports whether it did; any other error (or nil) is left to the caller.
func GatewayModeError(w http.ResponseWriter, err error) bool {
	status, code, msg := 0, "", ""
	switch {
	case err == nil:
		return false
	case errors.Is(err, services.ErrGatewayRemote):
		status, code, msg = http.StatusConflict, "gateway_remote", "Not available with the managed gateway."
	case errors.Is(err, services.ErrGatewayProviderUnknown):
		status, code, msg = http.StatusServiceUnavailable, "gateway_provider_unknown", "Gateway provider setting could not be read."
	case errors.Is(err, services.ErrGatewayChangePending):
		status, code, msg = http.StatusConflict, "change_pending", services.ErrGatewayChangePending.Error()
	default:
		return false
	}
	writeGatewayError(w, status, code, msg)
	return true
}

// GatewayLocalError answers a route that exists only for the managed gateway
// while this platform runs its own: 409 gateway_local.
func GatewayLocalError(w http.ResponseWriter) {
	writeGatewayError(w, http.StatusConflict, "gateway_local", "The managed gateway is not enabled.")
}

// GatewayProviderError writes the answer for an error from a managed-gateway
// call and reports whether it did: the mode sentinels, a server id that cannot
// be a target, a provider refusal by its code, or no answer at all. Any other
// error (a local store failure) is left to the caller. The provider's message
// is written for end users and is passed through; the key never is (the
// client redacts it).
func GatewayProviderError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	if GatewayModeError(w, err) {
		return true
	}
	var ae *remotegw.APIError
	switch {
	case errors.Is(err, services.ErrGatewayTargetName):
		writeGatewayError(w, http.StatusBadRequest, "invalid_target", "This server's id cannot be used as a gateway target.")
	case errors.Is(err, services.ErrGatewayDomainTaken):
		writeGatewayError(w, http.StatusConflict, "domain_taken", services.ErrGatewayDomainTaken.Error())
	case errors.Is(err, services.ErrGatewayKitRetiring):
		writeGatewayError(w, http.StatusConflict, "kit_retiring", kitRetiringText)
	case errors.Is(err, services.ErrGatewayKitKnown):
		writeGatewayError(w, http.StatusConflict, "kit_known", "This link belongs to a node of this platform. Replace or delete that node instead.")
	case errors.Is(err, services.ErrGatewayNoKit):
		writeGatewayError(w, http.StatusNotFound, "no_kit", "This node has no managed link yet.")
	case errors.As(err, &ae):
		status, code, msg := providerStatus(ae)
		if code == "rate_limited" {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(ae.RetryAfter)))
		}
		writeGatewayError(w, status, code, msg)
	case errors.Is(err, remotegw.ErrBadAnswer):
		writeGatewayError(w, http.StatusBadGateway, "provider_error", "The gateway provider sent an answer that could not be read. Try again in a moment.")
	case errors.Is(err, remotegw.ErrUnreachable), errors.Is(err, context.DeadlineExceeded):
		writeGatewayError(w, http.StatusBadGateway, "provider_unreachable", "The gateway provider could not be reached. Try again in a moment.")
	default:
		return false
	}
	return true
}

// retryAfterSeconds is the provider's wait in whole seconds, rounded up; 30
// when it named none, so a client always has a wait to honour.
func retryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 30
	}
	return int((d + time.Second - 1) / time.Second)
}

func providerStatus(ae *remotegw.APIError) (int, string, string) {
	switch {
	case ae.Status == http.StatusUnauthorized || ae.Code == "unauthorized":
		return http.StatusBadGateway, "provider_key_rejected", "The gateway provider rejected the API key."
	case ae.Status == http.StatusTooManyRequests || ae.Code == "rate_limited":
		return http.StatusServiceUnavailable, "rate_limited", ae.Message
	}
	switch ae.Code {
	case "domain_taken":
		return http.StatusConflict, ae.Code, ae.Message
	case "route_limit", "custom_domain_limit", "domain_unverified", "domain_blocked",
		"billing_suspended", "no_entitlement", "forbidden":
		return http.StatusForbidden, ae.Code, ae.Message
	case "target_not_allowed", "invalid_request":
		return http.StatusBadRequest, ae.Code, ae.Message
	case "proxy_protocol_disabled":
		// The provider's own text says "this platform", which here would read
		// as the customer's.
		return http.StatusConflict, ae.Code, "Real player IP forwarding is not enabled on the gateway provider."
	case "link_without_client_addr":
		return http.StatusConflict, ae.Code, ae.Message
	case "route_suspended":
		return http.StatusForbidden, ae.Code, ae.Message
	// The provider speaks of retiring a link; this panel of replacing a node's.
	case "kit_retiring":
		return http.StatusConflict, ae.Code, kitRetiringText
	case "kit_already_retiring":
		return http.StatusConflict, ae.Code, "This node's link is already being replaced."
	case "link_limit":
		return http.StatusConflict, ae.Code, "This plan's host limit is reached. Remove a node's link, or use Replace this node's link on a node you are retiring."
	case "retire_limit":
		return http.StatusConflict, ae.Code, "All replacement slots of this plan are in use. Wait until a link being replaced has run out."
	case "retire_budget":
		return http.StatusConflict, ae.Code, "This plan's monthly number of link replacements is used up."
	case "retire_not_needed":
		return http.StatusConflict, ae.Code, "This plan has no host limit, so no link needs replacing: add the new node and move the servers to it."
	case "link_not_found":
		return http.StatusNotFound, ae.Code, ae.Message
	}
	return http.StatusBadGateway, "provider_error", ae.Message
}

const kitRetiringText = "This node's link is being replaced. Move the server to another node first."

// WriteGatewayError writes the one body shape of every managed-gateway answer
// ({success:false, error, code}), for a handler's own remote-mode refusals.
func WriteGatewayError(w http.ResponseWriter, status int, code, msg string) {
	writeGatewayError(w, status, code, msg)
}

func writeGatewayError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"error":   msg,
		"code":    code,
	})
}

// RequireAutoMoveEnabled blocks the request with 503 auto_move_disabled unless
// the auto-move feature flag is on. It runs in either routing mode: without the
// gateway a moved server takes its new node's address.
func (s Gates) RequireAutoMoveEnabled(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.FeatureFlags.IsAutoMoveEnabled(r.Context()) {
			autoMoveDisabledResponse(w)
			return
		}
		next(w, r)
	}
}

// autoMoveDisabledResponse writes the canonical 503 + machine-readable body.
func autoMoveDisabledResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"error":   "auto_move_disabled",
		"message": "Auto-move is disabled; enable it in Settings → Features while gateway routing is active.",
	})
}

// GatewayDisabledResponse writes the canonical 503 + machine-readable body,
// mirroring FeatureDisabledResponse.
func GatewayDisabledResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"error":   "gateway_disabled",
		"message": "Gateway routing is disabled; switch routing mode to gateway or both to manage routes.",
	})
}
