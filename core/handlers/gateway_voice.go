package handlers

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// voiceNodeSince is the node release that speaks voicewire v2. 2026.10.22 ran
// v1, which could not name a Dylaris server (it assumed RFC uuids), so it is
// too old even though it has a forwarder.
const voiceNodeSince = "2026.10.22.2"

type voiceView struct {
	Enabled bool   `json:"enabled"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Mod     string `json:"mod"`
	// Allowed is the resolved permission. The panel shows the switch when it
	// is true, or when voice is already on so it can be turned off.
	Allowed bool `json:"allowed"`
}

func newVoiceView(vp *store.VoicePort, allowed bool) voiceView {
	if vp == nil {
		return voiceView{Allowed: allowed}
	}
	return voiceView{Enabled: true, Host: vp.Host, Port: vp.Port, Mod: vp.Mod, Allowed: allowed}
}

// voiceAllowed is ResolveVoiceAllowed failing closed: a read error is "no".
func (h *GatewayHandler) voiceAllowed(srv *models.Server) bool {
	allowed, _, err := services.ResolveVoiceAllowed(h.state.Store, srv)
	if err != nil {
		log.Printf("voice: server %d: permission check: %v", srv.ID, err)
		return false
	}
	return allowed
}

// GetServerVoice GET /api/servers/{id}/voice - the server's voice port, if
// any, and whether it may turn voice on.
func (h *GatewayHandler) GetServerVoice(w http.ResponseWriter, r *http.Request) {
	srv, err := h.state.Store.GetServerByID(mustAtoi(mux.Vars(r)["id"]))
	if err != nil || srv == nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	vp, err := h.state.Store.GetVoicePort(srv.ID)
	if err != nil {
		log.Printf("voice: server %d: %v", srv.ID, err)
		sendJSONError(w, "Failed to read the voice chat setting", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "voice": newVoiceView(vp, h.voiceAllowed(srv))})
}

// SetServerVoice PUT /api/servers/{id}/voice/switch - turns proximity voice
// chat on or off for one server. Body {"enabled": bool}.
//
// On: every precondition is checked before anything is written, then the port
// is allocated, the mod config written on the node, and the mapping published.
// A failure after the allocation rolls the row (and keys) back, unless the
// server already had voice before this request.
//
// Off is never refused: not by the permission, not by a suspension, not by
// routing that is no longer the gateway (the route is registered without
// RequireGatewayEnabled, the gate is checked here for on only). Turning
// something off is how an owner gets out of a state, never into one.
func (h *GatewayHandler) SetServerVoice(w http.ResponseWriter, r *http.Request) {
	srv, err := h.state.Store.GetServerByID(mustAtoi(mux.Vars(r)["id"]))
	if err != nil || srv == nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	enabled, ok := decodeProxyProtocolBody(r)
	if !ok {
		sendJSONError(w, `Invalid request: expected {"enabled": true|false}`, http.StatusBadRequest)
		return
	}
	if enabled {
		if !h.state.gatewayEnabled() {
			gatewayDisabledResponse(w)
			return
		}
		if refuseIfSuspended(w, r, h.state, srv) {
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if !enabled {
		changed, err := DisableServerVoice(ctx, h.state, srv)
		if err != nil {
			log.Printf("voice: server %d: disable: %v", srv.ID, err)
			sendJSONError(w, "Failed to turn voice chat off", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "voice": newVoiceView(nil, h.voiceAllowed(srv)), "restartRequired": changed})
		return
	}

	if !h.voiceAllowed(srv) {
		sendJSONErrorCode(w, "Voice chat is not available for this server.", "voice_not_allowed", http.StatusConflict)
		return
	}
	node, err := h.state.Store.GetNodeByID(srv.NodeID)
	if err != nil || node == nil || !nodeReleaseAtLeast(ctx, h.state, node.Token, voiceNodeSince) {
		sendJSONErrorCode(w, "This server's node is too old for voice chat. Update the node, then try again.", "node_too_old", http.StatusConflict)
		return
	}
	routes, err := services.ReadRoutesFromRedis(ctx, h.state.Redis)
	if err != nil {
		log.Printf("voice: server %d: read routes: %v", srv.ID, err)
		sendJSONError(w, "Could not read this server's routes. Try again.", http.StatusBadGateway)
		return
	}
	host, region, ok := services.VoiceRouteFor(routes, services.GetEdgesFromRedis(ctx, h.state.Redis), srv.UUID)
	if !ok {
		sendJSONErrorCode(w, "Voice chat needs an address on one of the platform's domains. Add a route first.", "no_routes", http.StatusConflict)
		return
	}
	relays, err := services.LiveVoiceRelays(ctx, h.state.Redis)
	if err != nil {
		log.Printf("voice: server %d: %v", srv.ID, err)
		sendJSONError(w, "Could not check the voice relays. Try again.", http.StatusBadGateway)
		return
	}
	relayUp := false
	for _, rl := range relays {
		relayUp = relayUp || rl.Region == region
	}
	if !relayUp {
		sendJSONErrorCode(w, "No voice relay is running in this server's region yet.", "no_relay", http.StatusConflict)
		return
	}
	if srv.ActiveSubServer == "" {
		sendJSONErrorCode(w, "Install the server first.", "no_sub_server", http.StatusConflict)
		return
	}
	rh := NewRconHandler(h.state)
	mod, rel, content, err := rh.findVoiceConfig(srv.NodeID, srv.UUID, srv.ActiveSubServer)
	switch {
	case errors.Is(err, errVoiceModMissing):
		sendJSONErrorCode(w, "Install Simple Voice Chat or Plasmo Voice and start the server once.", "voice_mod_missing", http.StatusConflict)
		return
	case errors.Is(err, errVoiceModAmbiguous):
		sendJSONErrorCode(w, "Both Simple Voice Chat and Plasmo Voice are installed. Remove one, then try again.", "voice_mod_ambiguous", http.StatusConflict)
		return
	case err != nil:
		log.Printf("voice: server %d: %v", srv.ID, err)
		sendJSONError(w, "Could not read the server's files on its node. Try again.", http.StatusBadGateway)
		return
	}

	prior, err := h.state.Store.GetVoicePort(srv.ID)
	if err != nil {
		log.Printf("voice: server %d: %v", srv.ID, err)
		sendJSONError(w, "Failed to turn voice chat on", http.StatusInternalServerError)
		return
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		sendJSONError(w, "Failed to turn voice chat on", http.StatusInternalServerError)
		return
	}
	got, err := h.state.Store.AllocateVoicePort(store.VoicePort{
		ServerID: srv.ID, Region: region, Mod: mod, InternalPort: services.VoiceInternalPort, UplinkKey: key, Host: host,
	}, services.VoicePortMin, services.VoicePortMax)
	if errors.Is(err, store.ErrVoicePoolFull) {
		sendJSONErrorCode(w, "Every voice port in this region is taken.", "voice_pool_full", http.StatusConflict)
		return
	}
	if err != nil {
		log.Printf("voice: server %d: allocate: %v", srv.ID, err)
		sendJSONError(w, "Failed to turn voice chat on", http.StatusInternalServerError)
		return
	}

	merged := svcVoiceEnable(content, got.Host, got.Port)
	if mod == "plasmo" {
		merged = plasmoVoiceEnable(content, got.Host, got.Port)
	}
	if merged != content {
		if err := rh.writeNodeFileString(srv.NodeID, srv.UUID, srv.ActiveSubServer+"/"+rel, merged); err != nil {
			log.Printf("voice: server %d: write %s: %v", srv.ID, rel, err)
			h.rollbackVoice(ctx, srv, prior, false)
			sendJSONError(w, "The node could not write the voice chat config. Try again.", http.StatusBadGateway)
			return
		}
	}
	// A server that moved region took a new port; the old one's keys go first,
	// because they share the server key with the new ones.
	if prior != nil && (prior.Region != got.Region || prior.Port != got.Port) {
		if err := services.UnpublishVoice(ctx, h.state.Redis, *prior); err != nil {
			log.Printf("voice: server %d: %v", srv.ID, err)
		}
	}
	if err := services.PublishVoice(ctx, h.state.Redis, got); err != nil {
		log.Printf("voice: server %d: %v", srv.ID, err)
		h.rollbackVoice(ctx, srv, prior, true)
		sendJSONError(w, "Voice chat could not be published to the gateway. Try again.", http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "voice": newVoiceView(&got, true), "restartRequired": true})
}

// rollbackVoice undoes a failed enable through the disable path. A server that
// had voice before the request keeps its row; the reconcile republishes it.
//
// writeBack=false is for a failed config write: this request changed nothing
// on the node, so writing the disabled config would only strip what the owner
// set by hand (a Plasmo [host.public], a voice_host), on a node that just
// failed a write.
func (h *GatewayHandler) rollbackVoice(ctx context.Context, srv *models.Server, prior *store.VoicePort, writeBack bool) {
	if prior != nil {
		return
	}
	var err error
	if writeBack {
		_, err = DisableServerVoice(ctx, h.state, srv)
	} else {
		_, err = dropServerVoice(ctx, h.state, srv)
	}
	if err != nil {
		log.Printf("voice: server %d: roll back: %v", srv.ID, err)
	}
}

// DisableServerVoice turns voice off for srv: the row first, then the keys,
// then the mod config written back. It is the disable path of the switch API
// and the enable's rollback; the reconcile uses RevokeServerVoice.
// changed=false means the server had no voice row and nothing was touched.
//
// Only the row is load-bearing. Without the row and keys voice simply stops,
// so a failed unpublish (the reconcile sweeps the keys) or a failed config
// write-back is logged, not returned.
func DisableServerVoice(ctx context.Context, st *AppState, srv *models.Server) (changed bool, err error) {
	changed, err = dropServerVoice(ctx, st, srv)
	if !changed || err != nil || srv.ActiveSubServer == "" {
		return changed, err
	}
	writeBackVoiceConfig(st, srv)
	return true, nil
}

// voiceWriteBackTimeout bounds how long a revocation waits on the node.
var voiceWriteBackTimeout = 30 * time.Second

// RevokeServerVoice is the reconcile's disable: the row and keys now, the
// config write-back in the background. The node file helpers take no context
// and a BYON node may never answer, so waiting on it here would stall the one
// goroutine that also sweeps orphans, republishes and meters.
//
// ponytail: a write-back past the timeout is abandoned, not cancelled; its
// goroutine ends when the node answers or disconnects. Thread a context
// through readNodeFileString if that ever piles up.
func RevokeServerVoice(ctx context.Context, st *AppState, srv *models.Server) error {
	changed, err := dropServerVoice(ctx, st, srv)
	if !changed || err != nil || srv.ActiveSubServer == "" {
		return err
	}
	timeout := voiceWriteBackTimeout
	done := make(chan struct{})
	go func() {
		defer close(done)
		writeBackVoiceConfig(st, srv)
	}()
	go func() {
		select {
		case <-done:
		case <-time.After(timeout):
			log.Printf("voice: server %d: config write-back abandoned after %s, the node did not answer", srv.ID, timeout)
		}
	}()
	return nil
}

// writeBackVoiceConfig strips the voice address from the mod config on the
// node. Failures are logged: voice already stopped with the row.
func writeBackVoiceConfig(st *AppState, srv *models.Server) {
	rh := NewRconHandler(st)
	mod, rel, content, err := rh.findVoiceConfig(srv.NodeID, srv.UUID, srv.ActiveSubServer)
	if err != nil {
		log.Printf("voice: server %d: config write-back skipped: %v", srv.ID, err)
		return
	}
	back := svcVoiceDisable(content)
	if mod == "plasmo" {
		back = plasmoVoiceDisable(content)
	}
	if back != content {
		// Asked again right before the write: on a slow node this runs long
		// after the disable, and if voice was turned on meanwhile the "off"
		// config would leave a voice port the mod no longer uses.
		if p, err := st.Store.GetVoicePort(srv.ID); err != nil || p != nil {
			log.Printf("voice: server %d: config write-back skipped, voice is on again (or unreadable: %v)", srv.ID, err)
			return
		}
		if err := rh.writeNodeFileString(srv.NodeID, srv.UUID, srv.ActiveSubServer+"/"+rel, back); err != nil {
			log.Printf("voice: server %d: config write-back of %s failed: %v", srv.ID, rel, err)
		}
	}
}

// ListVoicePermissions GET /api/admin/voice-permissions - every scope row,
// plus the global setting they override.
func (h *GatewayHandler) ListVoicePermissions(w http.ResponseWriter, r *http.Request) {
	rows, err := h.state.Store.ListVoicePermissions()
	if err != nil {
		sendJSONError(w, "Failed to list voice permissions", http.StatusInternalServerError)
		return
	}
	global := h.state.FeatureFlags != nil && h.state.FeatureFlags.IsVoiceChatEnabled(r.Context())
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "permissions": rows, "global": global})
}

// SetVoicePermission PUT /api/admin/voice-permissions - body {scope, allowed}.
// A deny takes effect on servers that already have voice at the next reconcile.
func (h *GatewayHandler) SetVoicePermission(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope   string `json:"scope"`
		Allowed *bool  `json:"allowed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Allowed == nil || !services.ValidVoicePermissionScope(req.Scope) {
		sendJSONError(w, `Invalid request: expected {"scope": "server:<id>|user:<id>|role:<id>", "allowed": true|false}`, http.StatusBadRequest)
		return
	}
	if err := h.state.Store.SetVoicePermission(req.Scope, *req.Allowed); err != nil {
		sendJSONError(w, "Failed to save the voice permission", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// DeleteVoicePermission DELETE /api/admin/voice-permissions?scope=<scope> -
// removes the row, so the scope defers to the next one down again.
func (h *GatewayHandler) DeleteVoicePermission(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if !services.ValidVoicePermissionScope(scope) {
		sendJSONError(w, "Invalid scope", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.DeleteVoicePermission(scope); err != nil {
		sendJSONError(w, "Failed to delete the voice permission", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// dropServerVoice is the first half of DisableServerVoice: the row, then the
// keys. No node call.
func dropServerVoice(ctx context.Context, st *AppState, srv *models.Server) (changed bool, err error) {
	vp, err := st.Store.GetVoicePort(srv.ID)
	if err != nil || vp == nil {
		return false, err
	}
	if err := st.Store.DeleteVoicePort(srv.ID); err != nil {
		return false, err
	}
	if st.Redis != nil {
		if err := services.UnpublishVoice(ctx, st.Redis, *vp); err != nil {
			log.Printf("voice: server %d: %v", srv.ID, err)
		}
	}
	return true, nil
}
