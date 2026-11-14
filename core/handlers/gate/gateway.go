package gate

import (
	"context"
	"dylaris-core/services"
	"dylaris-core/services/remotegw"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
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
	status, code, msg, ok := modeAnswer(err)
	if ok {
		writeGatewayError(w, status, code, msg)
	}
	return ok
}

func modeAnswer(err error) (status int, code, msg string, ok bool) {
	switch {
	case err == nil:
		return 0, "", "", false
	case errors.Is(err, services.ErrGatewayRemote):
		return http.StatusConflict, "gateway_remote", "Not available with the managed gateway.", true
	case errors.Is(err, services.ErrGatewayProviderUnknown):
		return http.StatusServiceUnavailable, "gateway_provider_unknown", "Gateway provider setting could not be read.", true
	case errors.Is(err, services.ErrGatewayChangePending):
		return http.StatusConflict, "change_pending", services.ErrGatewayChangePending.Error(), true
	}
	return 0, "", "", false
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
	status, code, msg, ok := ProviderErrorAnswer(err)
	if !ok {
		return false
	}
	var ae *remotegw.APIError
	if code == "rate_limited" && errors.As(err, &ae) {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(ae.RetryAfter)))
	} else if errors.Is(err, services.ErrOwnDomainWritesBusy) {
		w.Header().Set("Retry-After", "60")
	}
	writeGatewayError(w, status, code, msg)
	return true
}

// ProviderErrorAnswer is the answer GatewayProviderError would write, for a
// handler that reports a provider failure inside an answer of its own.
func ProviderErrorAnswer(err error) (status int, code, msg string, ok bool) {
	if err == nil {
		return 0, "", "", false
	}
	if status, code, msg, ok := modeAnswer(err); ok {
		return status, code, msg, true
	}
	var ae *remotegw.APIError
	switch {
	case errors.Is(err, services.ErrGatewayTargetName):
		return http.StatusBadRequest, "invalid_target", "This server's id cannot be used as a gateway target.", true
	case errors.Is(err, services.ErrGatewayDomainTaken):
		return http.StatusConflict, "domain_taken", services.ErrGatewayDomainTaken.Error(), true
	case errors.Is(err, services.ErrGatewayKitRetiring):
		return http.StatusConflict, "kit_retiring", kitRetiringText, true
	case errors.Is(err, services.ErrGatewayKitKnown):
		return http.StatusConflict, "kit_known", "This link belongs to a node of this platform. Replace or delete that node instead.", true
	case errors.Is(err, services.ErrGatewayNoKit):
		return http.StatusNotFound, "no_kit", "This node has no managed link yet.", true
	case errors.Is(err, services.ErrOwnDomainWritesBusy):
		return http.StatusTooManyRequests, "rate_limited", services.ErrOwnDomainWritesBusy.Error(), true
	case errors.As(err, &ae):
		status, code, msg := providerStatus(ae)
		// A provider message naming a TXT value is the record of the one
		// platform-wide claim: shown under Your own domains to the users holding the
		// domain, never in an answer that may reach someone else.
		if strings.Contains(msg, "dylaris-verify=") {
			msg = "The gateway provider refused this for the domain. Check Your own domains for its records."
		}
		return status, code, msg, true
	case errors.Is(err, remotegw.ErrBadAnswer):
		return http.StatusBadGateway, "provider_error", "The gateway provider sent an answer that could not be read. Try again in a moment.", true
	case errors.Is(err, remotegw.ErrUnreachable), errors.Is(err, context.DeadlineExceeded):
		return http.StatusBadGateway, "provider_unreachable", "The gateway provider could not be reached. Try again in a moment.", true
	}
	return 0, "", "", false
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
	// Before the generic 429: the provider's manual-check spacing for one
	// domain, not a rate limit on the key.
	case ae.Code == "verify_cooldown":
		return http.StatusTooManyRequests, ae.Code, ae.Message
	case ae.Status == http.StatusTooManyRequests || ae.Code == "rate_limited":
		return http.StatusServiceUnavailable, "rate_limited", ae.Message
	}
	switch ae.Code {
	case "domain_taken":
		return http.StatusConflict, ae.Code, ae.Message
	// The provider counts own domains for the whole platform: its words would
	// read as this user's own allowance.
	case "custom_domain_limit":
		return http.StatusForbidden, ae.Code, providerOwnDomainCapText
	case "too_many_domains":
		return http.StatusConflict, ae.Code, providerOwnDomainCapText
	// The provider's message names its TXT value, which is the record of one
	// platform-wide claim: it is shown under Your own domains to the users holding
	// the domain, never in a refusal.
	case "domain_unverified":
		return http.StatusForbidden, ae.Code, "The gateway provider has not verified this domain yet. Publish its TXT record shown under Your own domains, then check again."
	case "custom_domains_off":
		return http.StatusConflict, ae.Code, "Own domains are not enabled for this account on the managed gateway."
	case "domain_in_use":
		return http.StatusConflict, ae.Code, ae.Message
	case "route_limit", "domain_blocked", "billing_suspended", "no_entitlement", "forbidden":
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
	// Voice and Bedrock: the provider speaks of "this account" and "the
	// platform's domains", which here would read as the customer's own.
	case "voice_not_allowed":
		return http.StatusConflict, ae.Code, withPlanNeeded("Voice chat is not enabled for this platform's account on the managed gateway.", ae.Message)
	case "link_without_voice":
		return http.StatusConflict, ae.Code, "This node's managed link has not reported that it can carry voice chat. Make sure it is running and up to date."
	case "no_routes":
		return http.StatusConflict, ae.Code, RemoteVoiceNoRoutesText
	case "no_relay", "voice_pool_full", "bedrock_unavailable":
		return http.StatusConflict, ae.Code, ae.Message
	case "bedrock_disabled":
		return http.StatusConflict, ae.Code, withPlanNeeded("Bedrock is not enabled for this platform's account on the managed gateway.", ae.Message)
	}
	return http.StatusBadGateway, "provider_error", ae.Message
}

// ManagedPlanNeededText ends the provider's voice or Bedrock refusal of an
// account without an active managed gateway plan. A customer Core reads it
// to say the same in its own words (withPlanNeeded); an older provider never
// sends it.
const ManagedPlanNeededText = "It needs an active managed gateway plan."

// withPlanNeeded is own, plus why when the provider said the platform's
// managed gateway plan is what is missing.
func withPlanNeeded(own, providerMsg string) string {
	if strings.HasSuffix(providerMsg, ManagedPlanNeededText) {
		return own + " It needs an active managed gateway plan for this platform."
	}
	return own
}

const kitRetiringText = "This node's link is being replaced. Move the server to another node first."

// RemoteVoiceNoRoutesText is the refusal of voice chat on the managed gateway
// for a server without an address on one of the provider's domains.
const RemoteVoiceNoRoutesText = "Voice chat needs an address on one of the gateway provider's domains. Add one first."

// RemoteOwnDomainExtrasText is the refusal of voice chat and Bedrock on an
// address on the customer's own domain (own_domain_unsupported).
const RemoteOwnDomainExtrasText = "Voice chat and Bedrock are not available on your own domain yet. Use an address on the gateway provider's domain for them."

const providerOwnDomainCapText = "This platform's own-domain limit at the gateway provider is reached."

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
