package gateway

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"dylaris-core/handlers/access"
	"dylaris-core/handlers/audit"
	"dylaris-core/handlers/gate"
	"dylaris-core/handlers/httpx"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

// RemoteKitHandler serves what only exists while the managed gateway is the
// provider: a node's link kit for the deploy file and the provider's usage.
// In local mode both answer 409 gateway_local.
type RemoteKitHandler struct {
	d Deps
	// views limits kit views per admin (keyed by user id, not by IP).
	views *httpx.IPRateLimiter
}

func NewRemoteKitHandler(d Deps) *RemoteKitHandler {
	return &RemoteKitHandler{d: d, views: httpx.NewIPRateLimiter()}
}

const managedLinkViewsPerMinute = 30

// activeRemote is the remote gateway, or false after writing the refusal.
func (h *RemoteKitHandler) activeRemote(w http.ResponseWriter) (*services.RemoteGateway, bool) {
	rg, err := services.ActiveRemoteGateway(h.d.GatewayRemote, h.d.RemoteGateway)
	if gate.GatewayModeError(w, err) {
		return nil, false
	}
	if rg == nil {
		gate.GatewayLocalError(w)
		return nil, false
	}
	return rg, true
}

// ManagedLink GET /api/nodes/{id}/managed-link returns the node's managed-gateway
// link kit (id and key) for its deploy file. topology.write at the route, like
// the other admin endpoints that hand out a link or warp key. Mints the
// node's kit on the provider the first time, then serves the stored one. The
// answer carries the kit KEY, so it is never cached and every view is audited
// (without the key).
func (h *RemoteKitHandler) ManagedLink(w http.ResponseWriter, r *http.Request) {
	rg, ok := h.activeRemote(w)
	if !ok {
		return
	}
	userID, _ := r.Context().Value("userID").(string)
	if !h.views.Allow("managed-link:"+userID, managedLinkViewsPerMinute) {
		w.Header().Set("Retry-After", "60")
		httpx.SendJSONErrorCode(w, "Too many requests, try again in a minute.", "rate_limited", http.StatusTooManyRequests)
		return
	}
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		httpx.SendJSONError(w, "Node not found", http.StatusNotFound)
		return
	}
	node, err := h.d.Store.GetNodeByID(id)
	// A customer's machine is theirs: an operator gets no kit key for it.
	if err != nil || node == nil || access.OwnedByOther(h.d.FeatureFlags, r, node) {
		httpx.SendJSONError(w, "Node not found", http.StatusNotFound)
		return
	}
	pc, err := h.d.GatewayProviderConfig()
	if err != nil {
		log.Printf("managed link: read the provider settings: %v", err)
		httpx.SendJSONError(w, "Failed to read the gateway provider settings", http.StatusInternalServerError)
		return
	}
	kit, err := rg.EnsureKit(r.Context(), uint(node.ID))
	if err != nil {
		if gate.GatewayProviderError(w, err) {
			return
		}
		log.Printf("managed link: node %d: %v", node.ID, err)
		httpx.SendJSONError(w, "Failed to load the node's link kit", http.StatusInternalServerError)
		return
	}
	// Unknown (nil) when the provider cannot say right now; the key is still
	// worth showing.
	var online *bool
	if kits, err := rg.KitStatus(r.Context()); err == nil {
		if k, found := kits[uint(node.ID)]; found {
			online = k.Online
		}
	}
	// The node reports no server network name to Core (NODE_DOCKER_NETWORK,
	// else the first dylaris_net / *_dylaris_net it finds), so this is only a
	// hint for the deploy form: the name the panel's BYON kit gives a
	// customer's node, and the plain one an operator's node creates.
	networkHint := "dylaris_net"
	if node.OwnerID != nil {
		networkHint = "byon_dylaris_net"
	}

	audit.LogIdentity(h.d.Store, r, audit.AuditEventManagedLinkKitViewed, userID, "", map[string]interface{}{
		"nodeId": node.ID,
		"linkId": kit.LinkID,
	})
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":      true,
		"linkId":       kit.LinkID,
		"linkKey":      kit.LinkKey,
		"coreUrl":      pc.URL,
		"network":      "",
		"networkKnown": false,
		"networkHint":  networkHint,
		"online":       online,
	})
}

// ProviderUsage GET /api/gateway/provider/usage returns the managed gateway
// provider's usage for the running month, cached for 60 s. topology.read at
// the route.
func (h *RemoteKitHandler) ProviderUsage(w http.ResponseWriter, r *http.Request) {
	rg, ok := h.activeRemote(w)
	if !ok {
		return
	}
	u, err := rg.Usage(r.Context())
	if err != nil {
		if gate.GatewayProviderError(w, err) {
			return
		}
		log.Printf("provider usage: %v", err)
		httpx.SendJSONError(w, "Failed to read the provider's usage", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"period":     u.Period,
		"edgeBytes":  u.EdgeBytes,
		"relayBytes": u.RelayBytes,
		"final":      u.Final,
		"pool":       u.Pool,
	})
}
