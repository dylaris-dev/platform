package health

import (
	"encoding/json"
	"net/http"
)

type SystemHandler struct {
	// region + coreID are stamped at construction so the core-info endpoint
	// doesn't have to thread cfg through every request.
	region string
	coreID string
	// tabProxyHostSuffix is the DNS suffix a proxied custom tab is served under,
	// one host per tab. Reported on the PUBLIC core-info endpoint because both
	// the authed panel and the anonymous share wrapper need to know whether the
	// feature is configured at all - an empty suffix means proxied tabs cannot
	// be shown anywhere, and the UI says so instead of rendering a dead frame.
	tabProxyHostSuffix string
}

// NewSystemHandler builds the handler. region + coreID come from config
// (DYLARIS_REGION, DYLARIS_CORE_ID). region defaults to "default" upstream.
// tabProxyHostSuffix comes from TAB_PROXY_HOST_SUFFIX.
func NewSystemHandler(region, coreID, tabProxyHostSuffix string) *SystemHandler {
	if region == "" {
		region = "default"
	}
	return &SystemHandler{
		region:             region,
		coreID:             coreID,
		tabProxyHostSuffix: tabProxyHostSuffix,
	}
}

// GetCapabilities returns the system capabilities
func (h *SystemHandler) GetCapabilities(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	json.NewEncoder(w).Encode(map[string]interface{}{
		"mode":            "business",
		"max_nodes":       9999,
		"max_gates":       9999,
		"can_whitelabel":  true,
		"can_custom_jars": true,
		"can_use_dns":     true,
	})
}

// GetCoreInfo returns the region + id of the Core instance that handled the
// request. Drives the topbar "Connected to <region> Core" chip — handy for
// admins triaging "which Core am I hitting" when DNS-georouting splits load.
// Public on the auth-required surface; no secrets in here.
func (h *SystemHandler) GetCoreInfo(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":            true,
		"region":             h.region,
		"coreId":             h.coreID,
		"tabProxyHostSuffix": h.tabProxyHostSuffix,
		"tabProxyAvailable":  h.tabProxyHostSuffix != "",
	})
}
