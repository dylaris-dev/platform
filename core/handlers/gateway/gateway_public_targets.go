package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"

	"dylaris-core/handlers/audit"
	"dylaris-core/handlers/httpx"
	"dylaris-pkg/validate"
)

// publicTargetsKey is the per-account switch an admin flips to let external
// gateway routes point at public addresses. Written only by SetPublicTargets.
func publicTargetsKey(userID string) string { return "gateway_public_targets:" + userID }

var errTargetNotAllowed = errors.New("target not allowed")

const targetNotAllowedMsg = "This target is not allowed. Use one of your servers (mc-<uuid>) or a private address; public targets need to be enabled for your account."

// externalPrivateNets are the ranges a key-created route may target by IP.
var externalPrivateNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8", "100.64.0.0/10", "fc00::/7"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// validateExternalTarget is validateLocalTarget plus the policy for routes made
// with an API key: a key can leak and a script can loop, so by default it may
// only aim a route at the customer's own servers (any name
// validate.ServerHostname gives, which is what a customer Core sends) or a
// private network. A DNS name other than localhost is refused because it can
// resolve to a public address after the check. The panel keeps the looser rule.
func validateExternalTarget(host string, publicAllowed bool) error {
	if err := validateLocalTarget(host); err != nil {
		return err
	}
	if publicAllowed {
		return nil
	}
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "localhost" || validate.IsServerHostname(host) {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		for _, n := range externalPrivateNets {
			if n.Contains(ip) {
				return nil
			}
		}
	}
	return errTargetNotAllowed
}

// checkExternalTarget applies the policy to a route request's targetHost and
// hands the body back to the session handler it adapts. It decodes the way
// CreateLinkRoute does (first JSON value, trailing data ignored) so the value
// checked is the value the handler will use, and it fails closed: a body that
// does not decode never reaches the handler.
func (h *ExternalGatewayHandler) checkExternalTarget(w http.ResponseWriter, r *http.Request) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeExternalError(w, http.StatusBadRequest, "Invalid request body.", "invalid_request")
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var req struct {
		TargetHost string `json:"targetHost"`
	}
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&req); err != nil {
		writeExternalError(w, http.StatusBadRequest, "Invalid request body.", "invalid_request")
		return false
	}
	if strings.TrimSpace(req.TargetHost) == "" {
		return true // the session handler words "target host is required"
	}
	userID, _ := r.Context().Value("userID").(string)
	allowed, err := h.gw.d.Store.GetSetting(publicTargetsKey(userID))
	if err != nil {
		// Fail closed: treated as "not enabled".
		log.Printf("gateway: reading %s failed, public targets treated as off: %v", publicTargetsKey(userID), err)
		allowed = ""
	}
	if err := validateExternalTarget(req.TargetHost, allowed == "true"); errors.Is(err, errTargetNotAllowed) {
		writeExternalError(w, http.StatusBadRequest, targetNotAllowedMsg, "target_not_allowed")
		return false
	}
	// Any other error is the session handler's to word.
	return true
}

// GetPublicTargets GET /api/admin/users/{id}/gateway-public-targets - whether this account's API keys may create gateway routes with public targets.
func (h *GatewayHandler) GetPublicTargets(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.ParseUserID(w, r)
	if !ok {
		return
	}
	v, err := h.d.Store.GetSetting(publicTargetsKey(userID))
	if err != nil {
		httpx.SendJSONError(w, "Failed to read the setting", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "enabled": v == "true"})
}

// SetPublicTargets PUT /api/admin/users/{id}/gateway-public-targets - body {"enabled": bool}, lets this account's API keys create gateway routes with public targets.
func (h *GatewayHandler) SetPublicTargets(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.ParseUserID(w, r)
	if !ok {
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
		httpx.SendJSONError(w, "Invalid JSON, expected {\"enabled\": bool}", http.StatusBadRequest)
		return
	}
	if u, err := h.d.Store.GetUserByID(userID); err != nil || u == nil {
		httpx.SendJSONError(w, "User not found", http.StatusNotFound)
		return
	}
	var err error
	if *req.Enabled {
		err = h.d.Store.SetSetting(publicTargetsKey(userID), "true")
	} else {
		err = h.d.Store.DeleteSetting(publicTargetsKey(userID))
	}
	if err != nil {
		httpx.SendJSONError(w, "Failed to save the setting", http.StatusInternalServerError)
		return
	}
	actorID, _ := r.Context().Value("userID").(string)
	audit.LogIdentity(h.d.Store, r, audit.AuditEventGatewayPublicTargets, actorID, userID, map[string]interface{}{"enabled": *req.Enabled})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "enabled": *req.Enabled})
}
