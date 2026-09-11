package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dylaris-core/services"
	"dylaris-core/services/redisacl"
	"dylaris-core/store"
	"dylaris-pkg/validate"

	"github.com/gorilla/mux"
)

type warpCtxKey string

const warpKeyCtx warpCtxKey = "warpAPIKey"

// WarpHandler serves enrollment + admin warp endpoints.
type WarpHandler struct {
	state *AppState
	svc   *services.WarpService
}

func NewWarpHandler(state *AppState, svc *services.WarpService) *WarpHandler {
	return &WarpHandler{state: state, svc: svc}
}

// WarpAPIKeyMiddleware authenticates a Bearer warp API key (separate from user
// sessions) and stuffs the resolved key into the request context.
func (h *WarpHandler) WarpAPIKeyMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			sendJSONError(w, "Missing Authorization", http.StatusUnauthorized)
			return
		}
		plaintext := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		key, err := h.state.Store.GetWarpAPIKeyByHash(HashAPIKey(plaintext))
		if err != nil {
			sendJSONError(w, "Invalid warp key", http.StatusUnauthorized)
			return
		}
		if key.RevokedAt != nil {
			sendJSONError(w, "Key revoked", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), warpKeyCtx, *key)
		next(w, r.WithContext(ctx))
	}
}

// Enroll POST /api/warp/enroll - registers a warp client's public key and
// answers with its overlay address, the region's leader endpoints and the Core
// and Redis addresses it should proxy to. Refused with 409 while platform
// routing is not in gateway mode, since there is no overlay to enroll into,
// and with 403 once a tenant's suspension has outlived the grace period.
func (h *WarpHandler) Enroll(w http.ResponseWriter, r *http.Request) {
	key, ok := r.Context().Value(warpKeyCtx).(store.WarpAPIKey)
	if !ok {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Warp peers are only reachable through the gateway overlay; with platform
	// routing in ip_port mode there is nothing to enroll into. Refuse here so
	// home nodes don't enroll into a dormant overlay (matches the panel, which
	// only offers the mint-key button when routing is gateway+beam).
	if !h.state.gatewayEnabled() {
		sendJSONError(w, "Gateway routing is disabled; enable gateway or both mode before enrolling warp peers.", http.StatusConflict)
		return
	}
	var req struct {
		PublicKey     string   `json:"public_key"`
		TunnelSubnets []string `json:"tunnel_subnets"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PublicKey == "" {
		sendJSONError(w, "Invalid request (public_key required)", http.StatusBadRequest)
		return
	}

	// A tenant whose suspension has outlived the grace loses the tunnel itself,
	// not only what it carries. The hourly enforcement pass drops their peers at
	// the leader; without this gate the client's own re-enroll would put them
	// straight back within minutes and the two would fight hourly, forever.
	//
	// Owner-scoped keys only: a platform key has no billing row, and suspension
	// is a tenant concept. Same fail-open-but-loud posture as LinkBoot - a DB
	// fault must not lock out paying tenants on reconnect, but it has to be
	// visible that the gate was skipped. Within the grace a suspended tenant
	// still enrolls, which is the whole point of the grace.
	if key.OwnerID != "" {
		if b, berr := h.state.Store.GetUserBilling(key.OwnerID); berr != nil {
			log.Printf("warp enroll: billing lookup for owner of key %d failed, suspension gate skipped: %v", key.ID, berr)
		} else if store.OwnerCutOff(b, h.state.SuspendGrace, services.OverLimitGrace, time.Now()) {
			sendJSONError(w, "Account suspended", http.StatusForbidden)
			return
		}
	}

	// Node admission's NETWORK gate belongs here, not on the gRPC node-enroll.
	//
	// A BYON node reaches Core's gRPC through the warp tunnel, so the TCP peer
	// there is the warp LEADER - measured: every denied enrol logged the same
	// 10.20.0.11 regardless of which customer it came from. An IP allowlist
	// evaluated on that address cannot tell two customers apart: enter a real
	// customer IP and everyone is locked out, enter the overlay range and
	// everyone is admitted.
	//
	// This request is different: it arrives over HTTPS, BEFORE any tunnel exists,
	// so clientIP(r) is the customer's real address (and is spoofing-resistant -
	// it only believes X-Forwarded-For from a configured trusted proxy).
	//
	// Node keys only. The setting is "node admission", and a route-only link kit
	// is not a node; gating link kits here would be a surprise to anyone reading
	// the setting's name. The JOIN gate (open / one-shot / disabled) stays on the
	// gRPC path, where it can be consumed exactly once per successful node enrol.
	if h.state.Admission != nil && strings.HasPrefix(key.NodeID, "node-") {
		allowed, reason, aerr := h.state.Admission.CheckNetwork(r.Context(), net.ParseIP(clientIP(r)))
		if aerr != nil {
			// Fail closed: a DB fault must not silently open admission.
			log.Printf("warp enroll: admission check failed for key %d: %v", key.ID, aerr)
			sendJSONError(w, "Could not verify admission", http.StatusInternalServerError)
			return
		}
		if !allowed {
			log.Printf("warp enroll: rejected key %d (%s) from %s", key.ID, reason, clientIP(r))
			sendJSONError(w, "This machine's network is not allowed to join", http.StatusForbidden)
			return
		}
	}

	res, err := h.svc.Enroll(r.Context(), key, req.PublicKey, req.TunnelSubnets)
	if err != nil {
		// 409 only for a genuine connection-limit conflict; everything else is
		// a server-side fault (no region configured, DB, IP allocation, leader
		// key) — surface it as 500 and log it instead of leaking internals.
		if errors.Is(err, store.ErrWarpLimitReached) {
			sendJSONError(w, "Connection limit reached for this key", http.StatusConflict)
			return
		}
		if errors.Is(err, services.ErrInvalidFixedWGIP) || errors.Is(err, store.ErrWarpIPTaken) {
			sendJSONError(w, err.Error(), http.StatusConflict)
			return
		}
		// Named rather than folded into the generic 500, because the fix is a
		// setting and the peer would otherwise retry a config that cannot work
		// every five seconds with nothing on either side saying why.
		if errors.Is(err, services.ErrNoWarpLeader) {
			log.Printf("warp enroll refused (key=%d): %v - add a leader for this region under Settings -> Warp", key.ID, err)
			sendJSONError(w, "No tunnel endpoint is available for your region yet.", http.StatusConflict)
			return
		}
		log.Printf("warp enroll failed (key=%d): %v", key.ID, err)
		sendJSONError(w, "Enrollment failed", http.StatusInternalServerError)
		return
	}
	// The service now fills region subnet, region pubkey and the failover endpoint
	// list, so an idempotent re-enroll can never disagree with a fresh one.
	stampOverlayAddrs(&res)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// stampOverlayAddrs tells the peer's warp where its two local proxy ports must
// forward to. Same resolver as the panel's deploy snippet (warp_deploy_addrs.go),
// so a machine that uses the proxy and a machine with hand-copied addresses can
// never be pointed at different places.
//
// Here rather than in WarpService: the addresses come off AppState, which knows
// Core's own networks, and the service has no view of them.
func stampOverlayAddrs(res *services.EnrollResult) {
	res.CoreGRPCAddr, res.RedisAddr = overlayServiceAddrs(grpcPortFromEnv())
}

// Assignment handles GET /api/warp/assignment?public_key=... - the client's
// lightweight poll for its current endpoint order (warp API-key auth). It never
// changes the peer's region; it only reflects the stored home leader.
func (h *WarpHandler) Assignment(w http.ResponseWriter, r *http.Request) {
	key, ok := r.Context().Value(warpKeyCtx).(store.WarpAPIKey)
	if !ok {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	pubkey := strings.TrimSpace(r.URL.Query().Get("public_key"))
	if pubkey == "" {
		sendJSONError(w, "public_key required", http.StatusBadRequest)
		return
	}
	res, err := h.svc.Assignment(r.Context(), key, pubkey)
	if err != nil {
		if errors.Is(err, services.ErrWarpPeerNotFound) {
			sendJSONError(w, "Peer not found", http.StatusNotFound)
			return
		}
		log.Printf("warp assignment failed (key=%d): %v", key.ID, err)
		sendJSONError(w, "Assignment failed", http.StatusInternalServerError)
		return
	}
	// The 30s poll is also the cheap address-refresh channel: warp reads the two
	// addresses off every assignment response, so an overlay that moved is picked
	// up within a tick without a second endpoint or a push.
	stampOverlayAddrs(&res)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// MintAPIKey (admin) creates a warp enrollment key and returns the plaintext ONCE.
func (h *WarpHandler) MintAPIKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string `json:"name"`
		Policy    string `json:"policy"`
		MaxConns  int    `json:"max_conns"`
		OnNewConn string `json:"on_new_conn"`
		FixedWGIP string `json:"fixed_wg_ip"`
		NodeID    string `json:"node_id"`
		Region    string `json:"region"` // "" = auto-assign at enroll
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if req.Policy != "fixed" && req.Policy != "general" {
		req.Policy = "general"
	}
	if req.OnNewConn != "kill_old" && req.OnNewConn != "block" {
		req.OnNewConn = "block"
	}
	if req.MaxConns < 1 {
		req.MaxConns = 1
	}
	// A fixed key is enforced at 1 regardless of what was stored (see
	// services/warp.go: policy "fixed" sets limit = 1). Storing anything else
	// leaves a row that disagrees with what actually happens at enrol, which is
	// what a "max 5" fixed key looked like in the list while the second machine
	// was being refused.
	if req.Policy == "fixed" {
		req.MaxConns = 1
	}
	// A pinned fixed overlay IP must be a legitimate host inside a known region's
	// subnet. Reject early so a junk key is never stored (the enroll path re-checks).
	if req.FixedWGIP != "" {
		if req.Region == "" {
			sendJSONError(w, "fixed_wg_ip requires a region", http.StatusBadRequest)
			return
		}
		reg, rerr := h.state.Store.GetWarpRegion(req.Region)
		if rerr != nil || reg == nil {
			sendJSONError(w, "Unknown region for fixed_wg_ip", http.StatusBadRequest)
			return
		}
		if verr := services.ValidateFixedWGIP(req.FixedWGIP, reg.Subnet); verr != nil {
			sendJSONError(w, verr.Error(), http.StatusBadRequest)
			return
		}
	}
	plaintext, err := generatePlaintextKey()
	if err != nil {
		sendJSONError(w, "Failed to generate key", http.StatusInternalServerError)
		return
	}
	id, err := h.state.Store.CreateWarpAPIKey(store.WarpAPIKey{
		Name: req.Name, KeyHash: HashAPIKey(plaintext), Policy: req.Policy,
		MaxConns: req.MaxConns, OnNewConn: req.OnNewConn, FixedWGIP: req.FixedWGIP,
		NodeID: req.NodeID, Region: req.Region,
	})
	if err != nil {
		sendJSONError(w, "Failed to create key", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "id": id, "api_key": plaintext,
	})
}

// ListAPIKeys GET /api/admin/warp/keys - the external-node inventory.
//
// Every admin-minted enrollment key with its live peers, so an operator can see
// what they issued and what is actually connected. The key itself is NOT
// returned - only its hash is stored, and that is the point: a key is shown once
// at mint time and never again.
//
// A key with no peers is minted-but-never-used; several peers on one key is the
// "general" policy doing its job.
func (h *WarpHandler) ListAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.state.Store.ListWarpAPIKeys()
	if err != nil {
		sendJSONError(w, "Failed to load enrollment keys", http.StatusInternalServerError)
		return
	}
	type peerView struct {
		Pubkey         string `json:"pubkey"`
		WGIP           string `json:"wg_ip"`
		Region         string `json:"region"`
		AssignedLeader string `json:"assigned_leader"`
		CreatedAt      string `json:"created_at"`
	}
	type keyView struct {
		ID        int        `json:"id"`
		Name      string     `json:"name"`
		Policy    string     `json:"policy"`
		MaxConns  int        `json:"max_conns"`
		OnNewConn string     `json:"on_new_conn"`
		Region    string     `json:"region"`
		NodeID    string     `json:"node_id"`
		FixedWGIP string     `json:"fixed_wg_ip"`
		Revoked   bool       `json:"revoked"`
		CreatedAt string     `json:"created_at"`
		Peers     []peerView `json:"peers"`
	}
	out := make([]keyView, 0, len(keys))
	for _, k := range keys {
		kv := keyView{
			ID: k.ID, Name: k.Name, Policy: k.Policy, MaxConns: k.MaxConns,
			OnNewConn: k.OnNewConn, Region: k.Region, NodeID: k.NodeID,
			FixedWGIP: k.FixedWGIP, Revoked: k.RevokedAt != nil,
			CreatedAt: k.CreatedAt.Format(time.RFC3339),
			Peers:     []peerView{},
		}
		peers, perr := h.state.Store.ListWarpPeersByKey(k.ID)
		if perr == nil {
			for _, p := range peers {
				kv.Peers = append(kv.Peers, peerView{
					Pubkey: p.Pubkey, WGIP: p.WGIP, Region: p.Region,
					AssignedLeader: p.AssignedLeader,
					CreatedAt:      p.CreatedAt.Format(time.RFC3339),
				})
			}
		}
		out = append(out, kv)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "keys": out})
}

// RevokeAPIKey DELETE /api/admin/warp/keys/{id} - revoke an enrollment key AND
// disconnect whatever it already enrolled.
//
// Marking the key revoked alone would only block the next enroll: an established
// WireGuard tunnel carries no memory of the key that created it and would keep
// forwarding indefinitely. So the peers are dropped from every leader of their
// region as well, which is what makes this an actual kill switch.
func (h *WarpHandler) RevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid key id", http.StatusBadRequest)
		return
	}
	key, err := h.state.Store.GetWarpAPIKeyByID(id)
	if err != nil || key == nil {
		sendJSONError(w, "Key not found", http.StatusNotFound)
		return
	}
	// Tenant link kits have their own teardown (routes + Redis ACL); routing one
	// through here would revoke the key and leave those behind.
	if key.OwnerID != "" {
		sendJSONError(w, "This is a tenant link kit — revoke it from Protected Addresses", http.StatusBadRequest)
		return
	}
	if derr := h.state.Store.RevokeWarpAPIKeyByID(id); derr != nil {
		sendJSONError(w, "Failed to revoke key", http.StatusInternalServerError)
		return
	}
	removed := 0
	if h.svc != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		removed = h.svc.DisconnectKeyPeers(ctx, id)
	}
	log.Printf("warp: revoked enrollment key %d (%s), disconnected %d peer(s)", id, key.Name, removed)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "disconnected": removed})
}

// DeleteAPIKey DELETE /api/admin/warp/keys/{id}/purge - remove the key row
// entirely, after disconnecting whatever it enrolled.
//
// Revoke keeps the row so the history stays visible; this is for cleaning up
// test keys and decommissioned machines. Order matters and is not cosmetic:
// warp_peers cascades on delete, so the peers MUST be pushed out to the leaders
// first. Delete the row first and the peer rows vanish with it, leaving a live
// WireGuard peer on every leader and no record that would ever remove it - the
// resync rebuilds from the rows, and there would be none.
func (h *WarpHandler) DeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid key id", http.StatusBadRequest)
		return
	}
	key, err := h.state.Store.GetWarpAPIKeyByID(id)
	if err != nil || key == nil {
		sendJSONError(w, "Key not found", http.StatusNotFound)
		return
	}
	if key.OwnerID != "" {
		sendJSONError(w, "This is a tenant link kit — remove it from Protected Addresses", http.StatusBadRequest)
		return
	}

	removed := 0
	if h.svc != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		removed = h.svc.DisconnectKeyPeers(ctx, id)
	}
	if derr := h.state.Store.DeleteWarpAPIKeyByID(id); derr != nil {
		sendJSONError(w, "Failed to delete key", http.StatusInternalServerError)
		return
	}
	log.Printf("warp: deleted enrollment key %d (%s), disconnected %d peer(s)", id, key.Name, removed)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "disconnected": removed})
}

// MintLinkKit (tenant) creates a route-only "link kit": a warp enrollment key
// bound to the calling user plus an auto-generated link identity (node_id). The
// customer runs warp (joins the overlay) + link (tunnels their LOCAL server out
// through warp) — no managed node. Returns the plaintext warp key and the link
// identity ONCE; it does NOT return the link token. That same key doubles as the
// link's LINK_BOOT_KEY: the link exchanges it at /api/warp/link-boot for its
// derived token at boot, so the token never travels with the kit and the cluster
// secret never leaves Core (a tenant must never be able to derive another
// tenant's link token).
func (h *WarpHandler) MintLinkKit(w http.ResponseWriter, r *http.Request) {
	if !byonActive(h.state, r) {
		sendJSONError(w, "BYON is not enabled", http.StatusForbidden)
		return
	}
	// Route-only needs the overlay: warp enroll + link tunnel only exist in
	// gateway/both routing mode. Match Enroll, which refuses in ip_port mode.
	if !h.state.gatewayEnabled() || h.state.Gateway == nil {
		sendJSONError(w, "Gateway routing is disabled; enable gateway or both mode first.", http.StatusConflict)
		return
	}
	userID := byonCallerID(r)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	// A link kit is the route-only product. See entitlement_gate.go for why the
	// cap below cannot carry this on its own.
	if !h.state.requireEntitlement(r, w, userID, services.EntitlementRouteOnly) {
		return
	}
	// Enforce the tenant's link cap. nil is no cap; 0 is a real cap that nothing
	// gets past, which is what an account holding no route-only product has.
	lim, lerr := services.EffectiveLimits(h.state.Store, userID)
	if lerr != nil {
		sendJSONError(w, "Failed to resolve limits", http.StatusInternalServerError)
		return
	}
	if lim.MaxLinks != nil {
		used, cerr := h.state.Store.CountLinkKitsByOwner(userID)
		if cerr != nil {
			sendJSONError(w, "Failed to count links", http.StatusInternalServerError)
			return
		}
		if services.AtOrOver(lim.MaxLinks, int64(used)) {
			sendJSONError(w, fmt.Sprintf("Link limit reached (%d)", *lim.MaxLinks), http.StatusForbidden)
			return
		}
	}
	var req struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "Route-only link"
	}

	nodeID, err := generateLinkIdentity()
	if err != nil {
		sendJSONError(w, "Failed to generate link identity", http.StatusInternalServerError)
		return
	}
	plaintext, err := generatePlaintextKey()
	if err != nil {
		sendJSONError(w, "Failed to generate key", http.StatusInternalServerError)
		return
	}
	// general policy + single connection: one customer machine, one warp peer.
	// kill_old so a restart re-enrolls cleanly instead of hitting the limit.
	if _, err := h.state.Store.CreateWarpAPIKey(store.WarpAPIKey{
		Name:      name,
		KeyHash:   HashAPIKey(plaintext),
		Policy:    "general",
		MaxConns:  1,
		OnNewConn: "kill_old",
		NodeID:    nodeID,
		OwnerID:   userID,
	}); err != nil {
		sendJSONError(w, "Failed to create link kit", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"warp_key": plaintext,
		"link_id":  nodeID,
		"note":     "Shown once. Paste WARP_API_KEY into the route-only .env.",
	})
}

// LinkBoot POST /api/warp/link-boot - a Link presents its warp key and receives
// its tunnel token plus a Redis credential scoped to its own keys. A route-only
// link kit gets its own derived token and route-only ACL; a BYON node key gets
// the Link of the node it is bound to (nodeLinkBoot). WarpAPIKeyMiddleware has
// already rejected an unknown or revoked key. The response is never logged.
func (h *WarpHandler) LinkBoot(w http.ResponseWriter, r *http.Request) {
	key, ok := r.Context().Value(warpKeyCtx).(store.WarpAPIKey)
	if !ok {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	// A BYON node's warp key must never mint a ROUTE-ONLY link credential: that
	// ACL has none of the keys a node's servers need, and it would give the
	// machine a second tunnel identity beside its node's. It gets its node's own
	// Link instead. An owner-less key is a platform key with no BYON node behind
	// it, and is refused like any other.
	nodeKey := strings.HasPrefix(key.NodeID, "node-") && key.OwnerID != ""
	if !nodeKey && !strings.HasPrefix(key.NodeID, "link-") {
		sendJSONError(w, "Not a route-only link key", http.StatusForbidden)
		return
	}
	if !h.state.gatewayEnabled() || h.state.Gateway == nil {
		sendJSONError(w, "Gateway routing is disabled", http.StatusConflict)
		return
	}
	// A HARD-suspended tenant's link (suspended past the grace) cannot boot.
	// GetUserBilling never returns sql.ErrNoRows; a missing row yields Status
	// "active". A real DB fault fails OPEN so a transient blip does not lock out
	// paying tenants on reconnect, but it must be visible: the suspension gate is
	// silently degraded for that window. Within the grace a suspended tenant still
	// boots, consistent with the soft-enforcement posture.
	b, berr := h.state.Store.GetUserBilling(key.OwnerID)
	if berr != nil {
		log.Printf("link-boot: billing lookup for %s failed, suspension gate skipped: %v", key.NodeID, berr)
	} else if store.OwnerCutOff(b, h.state.SuspendGrace, services.OverLimitGrace, time.Now()) {
		sendJSONError(w, "Account suspended", http.StatusForbidden)
		return
	}
	// Re-check revoked_at with a fresh read immediately before provisioning, to
	// shrink the TOCTOU window against a concurrent RevokeLinkKit: the middleware
	// already checked revoked_at once, but a revoke racing between that check and
	// this point could otherwise have EnsureRouteOnlyLinkACL resurrect the just-
	// deleted ACL user. The reconciler's cleanup sweep (acl_reconciler.go) is the
	// robust backstop regardless (self-heals within ~60s even if this race is lost).
	if fresh, ferr := h.state.Store.GetWarpAPIKeyByNodeID(key.NodeID); ferr != nil {
		log.Printf("link-boot: fresh revoke check for %s failed, proceeding: %v", key.NodeID, ferr)
	} else if fresh.RevokedAt != nil {
		sendJSONError(w, "Key revoked", http.StatusUnauthorized)
		return
	}
	if nodeKey {
		h.nodeLinkBoot(w, key)
		return
	}
	tunnelToken := h.state.Gateway.LinkToken(key.NodeID)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	user, pass, err := h.state.ACLProvisioner.EnsureRouteOnlyLinkACL(ctx, h.state.ClusterSecret, key.NodeID, tunnelToken)
	if err != nil {
		log.Printf("link-boot: provision ACL for %s failed: %v", key.NodeID, err)
		sendJSONError(w, "Failed to provision credentials", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"link_id":    key.NodeID,
		"link_token": tunnelToken,
		"redis_user": user,
		"redis_pass": pass,
		"redis_db":   h.state.Redis.Options().DB,
	})
}

// byonLinkRedisAddr is the Redis address a kit-run BYON Link is told: warp's
// local proxy on the customer's host. 25571 is the proxy port compiled into
// gateway/warp/proxy.go, platform/node/warp_proxy.go and the panel's
// warpDeploy.ts; host.docker.internal is the name the kit maps to the host with
// extra_hosts host-gateway. Not 127.0.0.1: the kit runs this Link on a Docker
// network, where loopback is its own container, and a Link pointed there
// retries forever without saying why.
const byonLinkRedisAddr = "host.docker.internal:25571"

// nodeLinkBoot answers a BYON node key with the Link of the node it is bound to.
//
// Everything in the answer is what the node-managed Link on that machine already
// runs on: the same derived tunnel token and node id, so routes do not move and
// the Hub's discovered row stays the same, and the node-link Redis user Core
// provisions for that node on every handshake (EnsureNodeACL), so nothing new is
// provisioned here. The machine holding this key runs that node and can derive
// the same login from its own secret - the answer hands it nothing it did not
// already have, and CLUSTER_SECRET is only ever used to derive.
func (h *WarpHandler) nodeLinkBoot(w http.ResponseWriter, key store.WarpAPIKey) {
	// 409, not a refusal: in a fresh kit the Link starts beside the node and asks
	// before the node has enrolled, so "not yet" is the normal first answer and
	// the Link waits it out. The other way here is an existing node redeployed
	// with the new kit whose key was never bound in the panel - its Link also
	// waits forever, so the message has to name both causes.
	if key.BoundNodeID == 0 {
		sendJSONError(w, "This key is not bound to an enrolled node yet: a new node is still enrolling, or an existing node's key must be bound to it in the panel (Update this machine). The Link retries until it has.", http.StatusConflict)
		return
	}
	node, err := h.state.Store.GetNodeByID(key.BoundNodeID)
	if err != nil {
		log.Printf("link-boot: node %d bound to key %s could not be loaded: %v", key.BoundNodeID, key.NodeID, err)
		sendJSONError(w, "Failed to load the node", http.StatusInternalServerError)
		return
	}
	// The binding was owner-checked when it was made. Asked again on every boot
	// because a node row's owner can change afterwards, and a key must never hand
	// out the Link of a machine its owner no longer holds.
	if node.OwnerID == nil || *node.OwnerID != key.OwnerID {
		log.Printf("link-boot: key %s is bound to node %d, which its owner does not own; refused", key.NodeID, node.ID)
		sendJSONError(w, "This key does not belong to that machine's owner", http.StatusForbidden)
		return
	}
	secret, ok, err := redisacl.LoadNodeSecret(h.state.Store, h.state.ClusterSecret, node.ID)
	if err != nil {
		log.Printf("link-boot: loading the secret of node %d for key %s failed: %v", node.ID, key.NodeID, err)
		sendJSONError(w, "Failed to load the node's credentials", http.StatusInternalServerError)
		return
	}
	if !ok {
		sendJSONError(w, "This machine has not finished enrolling yet. The Link retries until it has.", http.StatusConflict)
		return
	}
	log.Printf("link-boot: node key %s booted the Link of node %d", key.NodeID, node.ID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":              true,
		"link_token":           h.state.Gateway.LinkToken(node.Token),
		"node_id":              node.Token,
		"link_discovery_proof": h.state.Gateway.DiscoveryProof(node.Token),
		"redis_user":           redisacl.LinkUsername(node.Token),
		"redis_pass":           redisacl.LinkPassword(secret, node.Token),
		"redis_db":             h.state.Redis.Options().DB,
		"redis_addr":           byonLinkRedisAddr,
	})
}

// ListLinkKits (tenant) returns the caller's route-only link kits (metadata only,
// no secrets). The link token is re-derivable from link_id, so it is re-revealed
// via the kit's reveal flow, not listed here.
func (h *WarpHandler) ListLinkKits(w http.ResponseWriter, r *http.Request) {
	if !byonActive(h.state, r) {
		sendJSONError(w, "BYON is not enabled", http.StatusForbidden)
		return
	}
	userID := byonCallerID(r)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	keys, err := h.state.Store.ListWarpAPIKeysByOwner(userID)
	if err != nil {
		sendJSONError(w, "Failed to load link kits", http.StatusInternalServerError)
		return
	}
	type linkKit struct {
		ID        int    `json:"id"`
		Name      string `json:"name"`
		LinkID    string `json:"link_id"`
		CreatedAt string `json:"created_at"`
	}
	// warp_api_keys also holds this tenant's BYON node keys. They are a different
	// product with a different cap, so listing them here would show a node as a
	// route-only location and let it be revoked through the wrong door.
	out := make([]linkKit, 0, len(keys))
	for _, k := range keys {
		if !strings.HasPrefix(k.NodeID, "link-") {
			continue
		}
		out = append(out, linkKit{ID: k.ID, Name: k.Name, LinkID: k.NodeID, CreatedAt: k.CreatedAt.Format("2006-01-02T15:04:05Z07:00")})
	}
	// Surface the effective link cap + current link-kit count so the panel can show
	// "X of Y links used". 0 limit means unlimited.
	lim, _ := services.EffectiveLimits(h.state.Store, userID)
	used, _ := h.state.Store.CountLinkKitsByOwner(userID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "kits": out, "used": used, "limit": lim.MaxLinks})
}

// RevokeLinkKit DELETE /api/warp/link-kits/{linkID} - owner or admin. Tears the
// route-only link down end to end: marks the warp key revoked so it can never
// re-enroll or boot again, drops its scoped Redis ACL user (ACL DELUSER terminates
// its live connections), deletes its tunnel key so the edge closes the tunnel within
// 30s, and removes its Core-owned routes. Order matters twice: the durable revoke
// comes first so a partial failure cannot undo itself, and the ACL user is dropped
// before the tunnel key so a restart cannot re-register (see the design doc).
func (h *WarpHandler) RevokeLinkKit(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	isAdmin, _ := r.Context().Value("isAdmin").(bool)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	linkID := mux.Vars(r)["linkID"]
	if !strings.HasPrefix(linkID, "link-") {
		sendJSONError(w, "Invalid link id", http.StatusBadRequest)
		return
	}
	key, err := h.state.Store.GetWarpAPIKeyByNodeID(linkID)
	if err != nil {
		sendJSONError(w, "Link not found", http.StatusNotFound)
		return
	}
	if !isAdmin && key.OwnerID != userID {
		sendJSONError(w, "Link not found", http.StatusNotFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Durable-first revoke, ACL dropped before the tunnel key, routes cleaned up
	// last - shared with the admin force-suspend path (billing_lifecycle.go
	// SuspendNow) so the ordering has one source of truth. See its doc comment.
	removed, rerr := services.RevokeLinkKitTeardown(ctx, h.state.Store, h.state.Gateway, h.state.Redis, h.state.ACLProvisioner, linkID, key.OwnerID)
	if rerr != nil {
		log.Printf("revoke link %s: %v", linkID, rerr)
		sendJSONError(w, "Failed to revoke link", http.StatusInternalServerError)
		return
	}
	// The overlay membership, on top of what the teardown removes.
	//
	// Deliberately HERE and not inside RevokeLinkKitTeardown, which is shared with
	// the admin force-suspend path (BillingLifecycleService.SuspendNow). Suspension
	// cuts the tunnel at the grace CUTOFF, not at the moment of suspension - that
	// is what suspendTenantWarpPeers is for and it is a decision, not an omission.
	// Putting the disconnect in the shared helper would move that cutoff forward
	// for every suspended tenant. A tenant revoking their OWN kit has no grace to
	// preserve: they asked for the machine to lose access.
	//
	// Without this the teardown took away what the tunnel CARRIES (routes, tunnel
	// key, Redis ACL) and left the tunnel itself, so the machine stayed an overlay
	// member after the panel reported the kit revoked.
	peers := 0
	if h.svc != nil {
		peers = h.svc.DisconnectKeyPeers(ctx, key.ID)
	}
	log.Printf("revoke link %s: revoked, %d route(s) removed, %d peer(s) disconnected", linkID, removed, peers)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// rollWarpKeySecret replaces the secret of one live warp key in place, after
// checking the caller owns it, and returns the new plaintext.
//
// Shared by the node-key and link-kit roll endpoints because the two differ only
// in their id prefix and their wording. Both are the SAME operation on the same
// table, and writing it twice is how the two drift.
//
// It deliberately does NOT disconnect the peer, which is the one thing that
// makes this different from revoke. A roll is "the machine keeps running, and
// the old secret stops opening anything new": the customer edits one line and
// redeploys when it suits them, and the kit's kill_old policy replaces the old
// tunnel at that moment. Revoke is the immediate-cutoff path and already exists
// beside it - a leaked key wants that one, not this.
func (h *WarpHandler) rollWarpKeySecret(w http.ResponseWriter, r *http.Request, id, prefix, notFound string) (string, bool) {
	userID, _ := r.Context().Value("userID").(string)
	isAdmin, _ := r.Context().Value("isAdmin").(bool)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return "", false
	}
	// Same reason RevokeLinkKit and RevokeNodeWarpKey check it: the two kinds of
	// key must not be reachable through each other's endpoint.
	if !strings.HasPrefix(id, prefix) {
		sendJSONError(w, "Invalid id", http.StatusBadRequest)
		return "", false
	}
	key, err := h.state.Store.GetWarpAPIKeyByNodeID(id)
	if err != nil {
		sendJSONError(w, notFound, http.StatusNotFound)
		return "", false
	}
	// Same message for "not yours" as for "does not exist", so a reply cannot
	// confirm that an id belongs to someone.
	if !isAdmin && key.OwnerID != userID {
		sendJSONError(w, notFound, http.StatusNotFound)
		return "", false
	}
	// A revoked key is not rollable: rolling it would quietly bring a revoked
	// identity back to life under a new secret.
	if key.RevokedAt != nil {
		sendJSONError(w, notFound, http.StatusNotFound)
		return "", false
	}
	plaintext, err := generatePlaintextKey()
	if err != nil {
		sendJSONError(w, "Failed to generate key", http.StatusInternalServerError)
		return "", false
	}
	if err := h.state.Store.RollWarpAPIKeyHash(id, HashAPIKey(plaintext)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			sendJSONError(w, notFound, http.StatusNotFound)
			return "", false
		}
		sendJSONError(w, "Failed to roll the key", http.StatusInternalServerError)
		return "", false
	}
	log.Printf("rolled warp key %s (owner %s); identity and peer left in place", id, key.OwnerID)
	return plaintext, true
}

// RollNodeWarpKey POST /api/warp/node-keys/{nodeID}/roll - owner or admin.
// New secret, same machine: node_id and the location name do not move, so the
// tenant's servers keep resolving to the same node row.
func (h *WarpHandler) RollNodeWarpKey(w http.ResponseWriter, r *http.Request) {
	if !byonActive(h.state, r) {
		sendJSONError(w, "BYON is not enabled", http.StatusForbidden)
		return
	}
	nodeID := mux.Vars(r)["nodeID"]
	plaintext, ok := h.rollWarpKeySecret(w, r, nodeID, "node-", "Node key not found")
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"warp_key": plaintext,
		"node_id":  nodeID,
		"note":     "Shown once. Replace API_KEY in the node's warp service and redeploy; nothing else changes.",
	})
}

// RollLinkKit POST /api/warp/link-kits/{linkID}/roll - owner or admin. The
// route-only twin of RollNodeWarpKey; the link trades this key for its own
// tunnel token at boot, so redeploying is what picks it up.
func (h *WarpHandler) RollLinkKit(w http.ResponseWriter, r *http.Request) {
	linkID := mux.Vars(r)["linkID"]
	plaintext, ok := h.rollWarpKeySecret(w, r, linkID, "link-", "Link not found")
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"warp_key": plaintext,
		"link_id":  linkID,
		"note":     "Shown once. Replace WARP_API_KEY in the route-only .env and redeploy.",
	})
}

// ListRegions returns the full warp registry (regions + leaders + liveness +
// peer counts) for the admin panel.
func (h *WarpHandler) ListRegions(w http.ResponseWriter, r *http.Request) {
	regions, err := h.svc.RegionsOverview(r.Context())
	if err != nil {
		sendJSONError(w, "Failed to load regions", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "regions": regions})
}

// UpsertRegion (admin) creates or updates a region's subnet + enabled flag.
func (h *WarpHandler) UpsertRegion(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region  string `json:"region"`
		Subnet  string `json:"subnet"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Region == "" || req.Subnet == "" {
		sendJSONError(w, "region and subnet are required", http.StatusBadRequest)
		return
	}
	if _, _, err := net.ParseCIDR(req.Subnet); err != nil {
		sendJSONError(w, "subnet must be a CIDR (e.g. 10.99.1.0/24)", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.UpsertWarpRegion(req.Region, req.Subnet, req.Enabled); err != nil {
		sendJSONError(w, "Failed to save region", http.StatusInternalServerError)
		return
	}
	// Mirror to Redis for the warp leader's boot resolve. Best-effort: the DB write
	// above is the source of truth, and the boot backfill self-heals a missed write.
	if err := services.PublishRegionSubnet(r.Context(), h.state.Redis, req.Region, req.Subnet); err != nil {
		log.Printf("warp region subnet mirror failed for %s: %v", req.Region, err)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// DeleteRegion (admin) removes a region (cascades its leaders). Refused while
// peers are still enrolled in it.
//
// The refusal is not tidiness, it is the only exit that exists. warp_leaders has
// an ON DELETE CASCADE to warp_regions, so deleting the region takes every leader
// row with it - while warp_peers.region is a plain TEXT column with no foreign
// key, so the peer rows SURVIVE, pointing at a region that is gone.
//
// From that moment nothing can remove those tunnels. Every leader command goes
// through pushToRegion, which enumerates ListWarpLeaders and matches on region;
// with no leader rows left it matches nothing and silently sends nothing. So
// DisconnectKeyPeers, revoke, delete-key and the suspension cutoff all report
// success and push no command, while the leader PROCESSES keep running with their
// full WireGuard peer set. The only way back is re-creating the region under the
// identical id.
//
// This is the same hazard DeleteAPIKey spells out ("the peers MUST be pushed out
// to the leaders first ... leaving a live WireGuard peer on every leader and no
// record that would ever remove it"), reached from the other direction, and the
// same guard the platform-regions sibling applies (handlers/regions.go refuses to
// delete a region that still has servers or nodes).
//
// Fails CLOSED on a count error, for the reason regions.go states: discarding it
// would let a database fault read as "empty" and delete the region anyway.
func (h *WarpHandler) DeleteRegion(w http.ResponseWriter, r *http.Request) {
	region := mux.Vars(r)["region"]
	peers, err := h.state.Store.ListWarpPeersByRegion(region)
	if err != nil {
		sendJSONError(w, "Could not verify the region is empty", http.StatusInternalServerError)
		return
	}
	if len(peers) > 0 {
		sendJSONError(w, fmt.Sprintf(
			"Region still has %d enrolled peer(s). Revoke or delete their enrollment keys first - deleting the region removes its leaders, and Core can no longer reach a leader it has no row for, so those tunnels would stay up with nothing able to remove them.",
			len(peers)), http.StatusConflict)
		return
	}
	if err := h.state.Store.DeleteWarpRegion(region); err != nil {
		sendJSONError(w, "Failed to delete region", http.StatusInternalServerError)
		return
	}
	log.Printf("warp: deleted region %s (no enrolled peers)", region)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// UpsertLeader (admin) creates or updates a leader endpoint within a region.
func (h *WarpHandler) UpsertLeader(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LeaderID string `json:"leaderId"`
		Region   string `json:"region"`
		Endpoint string `json:"endpoint"`
		Enabled  bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.LeaderID == "" || req.Region == "" || req.Endpoint == "" {
		sendJSONError(w, "leaderId, region and endpoint are required", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.UpsertWarpLeader(req.LeaderID, req.Region, req.Endpoint, req.Enabled); err != nil {
		sendJSONError(w, "Failed to save leader (does the region exist?)", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// DeleteLeader (admin) removes a leader endpoint.
func (h *WarpHandler) DeleteLeader(w http.ResponseWriter, r *http.Request) {
	leaderID := mux.Vars(r)["leaderId"]
	if err := h.state.Store.DeleteWarpLeader(leaderID); err != nil {
		sendJSONError(w, "Failed to delete leader", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// MintNodeWarpKey POST /api/warp/node-keys - tenant self-service. Mints the warp
// enrollment key a BYON machine needs to join the overlay before it can enroll as
// a node.
//
// This is the sibling of MintLinkKit and deliberately mirrors it: same BYON gate,
// same gateway-routing gate, same owner scoping, same shown-once secret. Two
// things differ, and both matter:
//
//   - The identity carries the "node-" prefix, not "link-". LinkBoot refuses a
//     non-link key, so a node key can never mint a link credential; and
//     CountLinkKitsByOwner counts only "link-%", so this never eats the tenant's
//     route-only allowance.
//   - It is capped on max_nodes, counting EXISTING nodes plus unredeemed keys. A
//     minted key is a node that has not connected yet, so capping on connected
//     nodes alone would let a one-node plan mint keys without limit.
//
// It exists because a BYON machine needs BOTH a warp key (to reach the overlay)
// and a node enroll token (to become a node), and only the second was
// self-service - so the deploy snippet handed the tenant a placeholder and left
// the operator to pass the other secret by hand, per customer.
func (h *WarpHandler) MintNodeWarpKey(w http.ResponseWriter, r *http.Request) {
	if !byonActive(h.state, r) {
		sendJSONError(w, "BYON is not enabled", http.StatusForbidden)
		return
	}
	// The overlay only exists in gateway/both routing mode; matches Enroll.
	if !h.state.gatewayEnabled() || h.state.Gateway == nil {
		sendJSONError(w, "Gateway routing is disabled; enable gateway or both mode first.", http.StatusConflict)
		return
	}
	userID := byonCallerID(r)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	// A warp key attaches a machine of their own, which is the BYON product.
	if !h.state.requireEntitlement(r, w, userID, services.EntitlementByon) {
		return
	}

	lim, lerr := services.EffectiveLimits(h.state.Store, userID)
	if lerr != nil {
		sendJSONError(w, "Failed to resolve limits", http.StatusInternalServerError)
		return
	}
	if lim.MaxNodes != nil {
		// Both kinds of pending identity, not just this endpoint's own keys: an
		// enroll token is the OTHER half of the machine an unredeemed key
		// belongs to, and counting one but not the other let the two mint gates
		// hand out more slots between them than the cap allows. Asked as "what
		// would they hold after this", so the second half of a machine already
		// counted is not refused. See services.NodeSlots.
		slots, serr := services.CountNodeSlots(h.state.Store, userID)
		if serr != nil {
			sendJSONError(w, "Failed to count nodes", http.StatusInternalServerError)
			return
		}
		if services.Exceeds(lim.MaxNodes, slots.UsedWithWarpKey()) {
			sendJSONError(w, fmt.Sprintf("Node limit reached (%d). Remove a machine, or revoke an unused key or enroll token, before adding another.", *lim.MaxNodes), http.StatusForbidden)
			return
		}
	}

	var req struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	// Required, and to a fixed shape. It used to fall back to "BYON node" for
	// everyone who left it blank, which made a list of machines unreadable the
	// moment there was more than one - and the name is also what the deploy
	// snippet slugs into NODE_ID, so it has to be safe there.
	name := strings.TrimSpace(req.Name)
	if !validate.IsLocationName(name) {
		sendJSONError(w, "Name this location: 4 to 20 characters, letters, digits and hyphens, not starting or ending with a hyphen.", http.StatusBadRequest)
		return
	}

	nodeID, err := generateNodeWarpIdentity()
	if err != nil {
		sendJSONError(w, "Failed to generate node identity", http.StatusInternalServerError)
		return
	}
	plaintext, err := generatePlaintextKey()
	if err != nil {
		sendJSONError(w, "Failed to generate key", http.StatusInternalServerError)
		return
	}
	// One customer machine, one warp peer; kill_old so a restart re-enrolls
	// cleanly instead of hitting the connection limit. Same as a link kit.
	if _, err := h.state.Store.CreateWarpAPIKey(store.WarpAPIKey{
		Name:      name,
		KeyHash:   HashAPIKey(plaintext),
		Policy:    "general",
		MaxConns:  1,
		OnNewConn: "kill_old",
		NodeID:    nodeID,
		OwnerID:   userID,
	}); err != nil {
		sendJSONError(w, "Failed to create the node key", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"warp_key": plaintext,
		"node_id":  nodeID,
		"note":     "Shown once. Paste it as API_KEY in the node's warp service.",
	})
}

// ListNodeWarpKeys GET /api/warp/node-keys - the caller's own BYON node keys,
// metadata only (the secret is stored as a hash and is gone after minting).
func (h *WarpHandler) ListNodeWarpKeys(w http.ResponseWriter, r *http.Request) {
	if !byonActive(h.state, r) {
		sendJSONError(w, "BYON is not enabled", http.StatusForbidden)
		return
	}
	userID := byonCallerID(r)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	keys, err := h.state.Store.ListWarpAPIKeysByOwner(userID)
	if err != nil {
		sendJSONError(w, "Failed to load node keys", http.StatusInternalServerError)
		return
	}
	type nodeKey struct {
		ID        int    `json:"id"`
		Name      string `json:"name"`
		NodeID    string `json:"node_id"`
		CreatedAt string `json:"created_at"`
		// The machine the key belongs to; absent until it is bound.
		BoundNodeID int `json:"bound_node_id,omitempty"`
	}
	out := make([]nodeKey, 0, len(keys))
	for _, k := range keys {
		if !strings.HasPrefix(k.NodeID, "node-") {
			continue
		}
		out = append(out, nodeKey{ID: k.ID, Name: k.Name, NodeID: k.NodeID, CreatedAt: k.CreatedAt.Format("2006-01-02T15:04:05Z07:00"), BoundNodeID: k.BoundNodeID})
	}
	lim, _ := services.EffectiveLimits(h.state.Store, userID)
	// Through the same counter the mint gates use, or the panel shows a number
	// the endpoint does not enforce and the cap looks arbitrary the moment it is
	// hit. It did: this counted nodes plus every unrevoked key in the list -
	// which includes keys already redeemed by a machine that is now a node, and
	// omits pending enroll tokens entirely.
	used, _ := services.NodeSlotsUsed(h.state.Store, userID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "keys": out,
		"used": used, "limit": lim.MaxNodes,
	})
}

// BindNodeWarpKey POST /api/warp/node-keys/{nodeID}/bind - owner only. Ties one
// of the caller's BYON node keys to one of the caller's machines, for a machine
// that enrolled before keys were bound at enrol. Once bound, link-boot answers
// the key with that node's Link, so the machine's kit can run the Link beside
// the node instead of inside it. Binding changes nothing on the machine until it
// is redeployed with that kit.
//
// Owner-checked on BOTH ends, admins included: a key and a machine of two
// different owners must never be joined. Operator machines (owner NULL) stay
// node-managed - their keys are admin keys with no owner to check against, and
// link-boot refuses an owner-less key.
func (h *WarpHandler) BindNodeWarpKey(w http.ResponseWriter, r *http.Request) {
	if !byonActive(h.state, r) {
		sendJSONError(w, "BYON is not enabled", http.StatusForbidden)
		return
	}
	userID := byonCallerID(r)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Node int `json:"node"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Node <= 0 {
		sendJSONError(w, "Choose the machine this key belongs to", http.StatusBadRequest)
		return
	}
	key, status, msg := ownNodeKey(h.state.Store, mux.Vars(r)["nodeID"], userID)
	if key == nil {
		sendJSONError(w, msg, status)
		return
	}
	// Same answer for "not yours" as for "does not exist", as for the key.
	node, err := h.state.Store.GetNodeByID(req.Node)
	if err != nil || node.OwnerID == nil || *node.OwnerID != userID {
		sendJSONError(w, "Machine not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if key.BoundNodeID == node.ID {
		// Already the answer; a second click is not an error.
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
		return
	}
	if key.BoundNodeID != 0 {
		sendJSONError(w, "This key already belongs to another machine", http.StatusConflict)
		return
	}
	// Asked before the write so the refusal can say why. The unique index on
	// bound_node_id is what holds it under a race.
	keys, err := h.state.Store.ListWarpAPIKeysByOwner(userID)
	if err != nil {
		sendJSONError(w, "Failed to load node keys", http.StatusInternalServerError)
		return
	}
	for _, k := range keys {
		if k.BoundNodeID == node.ID {
			sendJSONError(w, "This machine already has a key for its Link", http.StatusConflict)
			return
		}
	}
	bound, err := h.state.Store.BindWarpAPIKey(key.ID, node.ID)
	if errors.Is(err, store.ErrWarpKeyNodeTaken) {
		sendJSONError(w, "This machine already has a key for its Link", http.StatusConflict)
		return
	}
	if err != nil {
		log.Printf("bind node key %s to node %d: %v", key.NodeID, node.ID, err)
		sendJSONError(w, "Failed to bind the key", http.StatusInternalServerError)
		return
	}
	if !bound {
		sendJSONError(w, "This key changed in the meantime. Reload and try again.", http.StatusConflict)
		return
	}
	log.Printf("bind node key %s to node %d (owner %s)", key.NodeID, node.ID, userID)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// ownNodeKey resolves one of the caller's live BYON node keys by its identity, or
// the status and message to refuse with. "Not yours" reads exactly like "does
// not exist", so the answer never confirms that someone else's identity is real.
func ownNodeKey(st store.Store, nodeID, userID string) (*store.WarpAPIKey, int, string) {
	if !strings.HasPrefix(nodeID, "node-") {
		return nil, http.StatusBadRequest, "Not a node key"
	}
	k, err := st.GetWarpAPIKeyByNodeID(nodeID)
	if err != nil || k.OwnerID == "" || k.OwnerID != userID {
		return nil, http.StatusNotFound, "Node key not found"
	}
	if k.RevokedAt != nil {
		return nil, http.StatusConflict, "This node key has been revoked"
	}
	return k, 0, ""
}

// RevokeNodeWarpKey DELETE /api/warp/node-keys/{nodeID} - owner or admin.
//
// Also the way out of a dead end: a minted key counts against max_nodes, and the
// secret is shown once. A tenant who loses it before using it would otherwise sit
// at their cap forever with nothing to revoke it through.
//
// Revoking blocks future warp enrollment for that identity AND drops whatever it
// already enrolled, which is the same pairing RevokeAPIKey and DeleteAPIKey
// perform and the suspension cutoff performs.
//
// An earlier version of this comment claimed a connected machine "keeps its
// current tunnel until it reconnects". Nothing implemented that, and nothing
// could: a re-enroll under a revoked key is refused at the middleware, and the
// refusal leaves the existing warp_peers row and the leader's WireGuard peer
// exactly where they are. The tunnel therefore stayed up indefinitely. That is
// precisely what DisconnectKeyPeers' own doc says makes revoke "a security
// control that is not one", and what suspendTenantWarpPeers says about a client
// that "happily keeps a working peer forever".
//
// This endpoint still owns the KEY, not the machine: the node row, its servers
// and its data are untouched, and removing the node is a separate action. What
// changed is that revoking now actually takes the overlay membership away, which
// is the one thing a tenant revoking a lost key is asking for.
//
// Best-effort by construction (DisconnectKeyPeers is per peer and per leader, and
// a leader that is down converges on its next resync). The durable revoke runs
// FIRST so a failure below cannot undo itself - same ordering as
// RevokeLinkKitTeardown.
func (h *WarpHandler) RevokeNodeWarpKey(w http.ResponseWriter, r *http.Request) {
	if !byonActive(h.state, r) {
		sendJSONError(w, "BYON is not enabled", http.StatusForbidden)
		return
	}
	userID, _ := r.Context().Value("userID").(string)
	isAdmin, _ := r.Context().Value("isAdmin").(bool)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	nodeID := mux.Vars(r)["nodeID"]
	// Prefix-checked for the same reason RevokeLinkKit checks its own: the two
	// kinds of key must not be revocable through each other's endpoint.
	if !strings.HasPrefix(nodeID, "node-") {
		sendJSONError(w, "Invalid node key id", http.StatusBadRequest)
		return
	}
	key, err := h.state.Store.GetWarpAPIKeyByNodeID(nodeID)
	if err != nil {
		sendJSONError(w, "Node key not found", http.StatusNotFound)
		return
	}
	// Same message for "not yours" as for "does not exist": a different one would
	// confirm the id belongs to someone.
	if !isAdmin && key.OwnerID != userID {
		sendJSONError(w, "Node key not found", http.StatusNotFound)
		return
	}
	if err := h.state.Store.RevokeWarpAPIKeyByNodeID(nodeID); err != nil {
		sendJSONError(w, "Failed to revoke the node key", http.StatusInternalServerError)
		return
	}
	removed := 0
	if h.svc != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		removed = h.svc.DisconnectKeyPeers(ctx, key.ID)
	}
	log.Printf("revoke node warp key %s (owner %s), disconnected %d peer(s)", nodeID, key.OwnerID, removed)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "disconnected": removed})
}
