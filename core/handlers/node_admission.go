package handlers

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"

	"dylaris-core/models"
	"dylaris-core/services/redisacl"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// NodeAdmissionHandler serves the admin admission config (join/IP mode + CIDRs)
// and the per-node reset-pairing and roll-secret actions. All endpoints are
// admin-only.
type NodeAdmissionHandler struct {
	state *AppState
}

func NewNodeAdmissionHandler(state *AppState) *NodeAdmissionHandler {
	return &NodeAdmissionHandler{state: state}
}

// admissionPayload is the wire shape for GET/PUT of the join + IP mode.
type admissionPayload struct {
	JoinMode string `json:"joinMode"`
	IPMode   string `json:"ipMode"`
}

// GetAdmission GET /api/admin/settings/node-admission — current modes + CIDRs.
func (h *NodeAdmissionHandler) GetAdmission(w http.ResponseWriter, r *http.Request) {
	joinMode, err := h.state.Store.GetSetting("node_join_mode")
	if err != nil || joinMode == "" {
		joinMode = "open"
	}
	ipMode, err := h.state.Store.GetSetting("node_admission_ip_mode")
	if err != nil || ipMode == "" {
		ipMode = "allow"
	}
	cidrs, err := h.state.Store.ListAdmissionCIDRs()
	if err != nil {
		sendJSONError(w, "Failed to load CIDRs", http.StatusInternalServerError)
		return
	}
	if cidrs == nil {
		cidrs = []store.AdmissionCIDR{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"joinMode": joinMode,
		"ipMode":   ipMode,
		"cidrs":    cidrs,
	})
}

// SetAdmission PUT /api/admin/settings/node-admission — write join + IP mode.
func (h *NodeAdmissionHandler) SetAdmission(w http.ResponseWriter, r *http.Request) {
	var req admissionPayload
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	validJoin := map[string]bool{"disabled": true, "open": true, "one-shot": true}
	validIP := map[string]bool{"allow": true, "deny": true}
	if !validJoin[req.JoinMode] || !validIP[req.IPMode] {
		sendJSONError(w, "Invalid joinMode or ipMode", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.SetSetting("node_join_mode", req.JoinMode); err != nil {
		sendJSONError(w, "Save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.state.Store.SetSetting("node_admission_ip_mode", req.IPMode); err != nil {
		sendJSONError(w, "Save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if uid := byonCallerID(r); uid != "" {
		_ = h.state.Store.InsertAuditIdentity(&models.AuditEventIdentity{
			EventType:   "node.admission_changed",
			ActorUserID: &uid,
			Metadata:    map[string]interface{}{"joinMode": req.JoinMode, "ipMode": req.IPMode},
		})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"joinMode": req.JoinMode,
		"ipMode":   req.IPMode,
	})
}

// AddCIDR POST /api/admin/settings/node-admission/cidrs — add one allowlist CIDR.
func (h *NodeAdmissionHandler) AddCIDR(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CIDR  string `json:"cidr"`
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	_, netw, perr := net.ParseCIDR(strings.TrimSpace(req.CIDR))
	if perr != nil {
		sendJSONError(w, "Invalid CIDR", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.AddAdmissionCIDR(netw.String(), strings.TrimSpace(req.Label)); err != nil {
		sendJSONError(w, "Failed to add CIDR", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "cidr": netw.String()})
}

// DeleteCIDR DELETE /api/admin/settings/node-admission/cidrs/{id} - drops one
// entry from the node admission allowlist.
func (h *NodeAdmissionHandler) DeleteCIDR(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if err := h.state.Store.DeleteAdmissionCIDR(id); err != nil {
		sendJSONError(w, "Failed to delete CIDR", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// ResetPairing POST /api/admin/nodes/{id}/reset-pairing — REVOKE: clear the
// node's secret and hard-cut its live Redis ACL. The node row / owner / servers
// / backups are untouched, and the next connect re-provisions the ACL under a
// fresh secret.
//
// It no longer mints anything. This used to hand back a single-use
// NODE_RECOVERY_TOKEN that an operator had to write into the node's environment
// and restart it for - which on a Swarm stack is a stack edit and a redeploy to
// re-admit one host, and which had to be started from a screen that never showed
// the node was being refused. A node that holds CLUSTER_SECRET was never
// affected either way: it re-pairs by itself in seconds via the cluster proof.
//
// What replaced it: the node keeps dialling, Core records the refusal, and an
// operator admits it from the same screen the refusal is listed on. See
// ApproveJoinAttempt below.
func (h *NodeAdmissionHandler) ResetPairing(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(mux.Vars(r)["id"])
	node, err := h.state.Store.GetNodeByID(id)
	if err != nil || node == nil {
		sendJSONError(w, "Node not found", http.StatusNotFound)
		return
	}
	uid := byonCallerID(r)

	// Invalidate the current secret: HasSecret -> false sends the next reconnect
	// down the first-issuance branch, which accepts a cluster proof or an
	// admission granted here.
	if err := h.state.Store.SetNodeSecretEnc(node.ID, ""); err != nil {
		sendJSONError(w, "Failed to reset secret", http.StatusInternalServerError)
		return
	}
	// Hard-cut the live Redis ACL so a possibly-compromised node loses access at
	// once (ACL DELUSER disconnects live clients) instead of only at its next
	// reconnect. Best-effort. Recovery re-provisions all three users under the new
	// secret via EnsureNodeACL when the node re-pairs.
	if h.state.Redis != nil {
		redisacl.NewProvisioner(h.state.Redis).RemoveNodeACL(r.Context(), node.Token)
	}
	if uid != "" {
		_ = h.state.Store.InsertAuditIdentity(&models.AuditEventIdentity{
			EventType:   "node.pairing_reset",
			ActorUserID: &uid,
			Metadata:    map[string]interface{}{"nodeId": node.ID, "nodeToken": node.Token},
		})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"note":    "The node's secret is cleared. A node holding the cluster secret re-pairs itself within seconds; any other node will appear under Connection attempts, where you can admit it.",
	})
}

// RollSecret POST /api/admin/nodes/{id}/roll-secret - replace the node's secret
// and let it straight back in from the address it last authenticated from.
//
// Rotation existed only as a side effect of ResetPairing and of an approval.
// This is ResetPairing followed by the approval an operator would otherwise give
// under Connection attempts, armed ahead of time: a node without CLUSTER_SECRET
// re-pairs within a minute instead of waiting to be noticed. A node holding
// CLUSTER_SECRET re-pairs through its cluster proof as it always did, and the
// armed admission is dropped with its row once the node is back.
//
// The admission is the SAME one an approval arms - one-shot, fifteen minutes,
// bound to an address Core read off a socket - so rolling a key never opens a
// wider door than admitting a node does. With no recorded address there is
// nothing to bind it to, and the node is left exactly as it was: an unbound
// admission would admit its identity from anywhere, and clearing the secret
// without one is what Reset pairing already offers.
func (h *NodeAdmissionHandler) RollSecret(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(mux.Vars(r)["id"])
	node, err := h.state.Store.GetNodeByID(id)
	if err != nil || node == nil {
		sendJSONError(w, "Node not found", http.StatusNotFound)
		return
	}
	fromIP, err := h.state.Store.GetNodeLastAuthPeerIP(node.ID)
	if err != nil {
		log.Printf("roll-secret: node %d: read last auth address: %v", node.ID, err)
		sendJSONError(w, "Database error", http.StatusInternalServerError)
		return
	}
	if fromIP == "" {
		sendJSONError(w, "Core has not recorded an address this node authenticated from, so there is nothing to bind its re-admission to. Nothing was changed. Use Reset pairing instead, then admit the node under Connection attempts.", http.StatusConflict)
		return
	}
	uid := byonCallerID(r)

	// Same order as ApproveJoinAttempt: the secret goes first, because a node
	// whose secret Core still holds is answered with a challenge and never
	// reaches the branch that consumes an admission.
	if err := h.state.Store.SetNodeSecretEnc(node.ID, ""); err != nil {
		log.Printf("roll-secret: node %d: clear secret: %v", node.ID, err)
		sendJSONError(w, "Failed to reset secret", http.StatusInternalServerError)
		return
	}
	if h.state.Redis != nil {
		redisacl.NewProvisioner(h.state.Redis).RemoveNodeACL(r.Context(), node.Token)
	}
	// Audited once the secret is gone, before arming: the revocation has
	// happened whether or not the admission below lands.
	if uid != "" {
		_ = h.state.Store.InsertAuditIdentity(&models.AuditEventIdentity{
			EventType:   "node.secret_rolled",
			ActorUserID: &uid,
			Metadata:    map[string]interface{}{"nodeId": node.ID, "nodeToken": node.Token},
		})
	}
	armed, err := h.state.Store.ArmNodeJoinApproval(node.Token, fromIP, uid)
	if err != nil || !armed {
		// The secret is already gone by this point, so the node has changed
		// state either way - log why arming failed, or this 500 explains
		// nothing afterwards.
		log.Printf("roll-secret: node %d: arm join approval failed (armed=%v): %v", node.ID, armed, err)
		sendJSONError(w, "The node's secret is cleared, but its re-admission could not be armed. It will appear under Connection attempts, where you can admit it.", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"note":    "Key rolled. The node's Redis access is cut at once. The re-admission is armed for 15 minutes; a node that is online reconnects with a new secret well within that window.",
	})
}

// ListJoinAttempts GET /api/admin/nodes/join-attempts — the connections Core is
// REFUSING, so an operator can see them at all.
//
// Every field except peerIp is what the caller SAID about itself, sent before
// any proof is checked. The panel labels them as reported for that reason; the
// address is the one thing on the row that cannot be chosen by the caller.
func (h *NodeAdmissionHandler) ListJoinAttempts(w http.ResponseWriter, r *http.Request) {
	attempts, err := h.state.Store.ListNodeJoinAttempts()
	if err != nil {
		sendJSONError(w, "Database error", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "attempts": attempts})
}

// ApproveJoinAttempt POST /api/admin/nodes/join-attempts/{token}/approve — let
// this machine back in, without touching the machine.
//
// Two acts, in this order. The node's secret is cleared, which is what sends its
// next connect down the first-issuance branch at all; then the admission is
// armed. Doing it the other way round would arm a door that the next connect
// walks straight past, because a node whose secret Core still holds is answered
// with a challenge and never reaches the branch that checks admissions.
//
// The armed window is short and tied to the address the attempt came from. The
// identity on a refused attempt is self-claimed - anyone who learns a node's id
// can knock - so an approval that stood indefinitely, or for any source address,
// would be an invitation to whoever knocks next.
func (h *NodeAdmissionHandler) ApproveJoinAttempt(w http.ResponseWriter, r *http.Request) {
	token := mux.Vars(r)["token"]
	node, err := h.state.Store.GetNodeByToken(token)
	if err != nil || node == nil {
		sendJSONError(w, "No node with that identity", http.StatusNotFound)
		return
	}
	if err := h.state.Store.SetNodeSecretEnc(node.ID, ""); err != nil {
		sendJSONError(w, "Failed to reset secret", http.StatusInternalServerError)
		return
	}
	if h.state.Redis != nil {
		redisacl.NewProvisioner(h.state.Redis).RemoveNodeACL(r.Context(), node.Token)
	}
	uid := byonCallerID(r)
	armed, err := h.state.Store.ApproveNodeJoinAttempt(token, uid)
	if err != nil {
		sendJSONError(w, "Database error", http.StatusInternalServerError)
		return
	}
	if !armed {
		// No row, or a row with no observed address. Both mean there is nothing
		// to bind the admission to, and admitting an identity from anywhere is
		// exactly what this must not do.
		sendJSONError(w, "That attempt is no longer listed, or Core never saw an address for it. Wait for the node to try again.", http.StatusConflict)
		return
	}
	if uid != "" {
		_ = h.state.Store.InsertAuditIdentity(&models.AuditEventIdentity{
			EventType:   "node.join_approved",
			ActorUserID: &uid,
			Metadata:    map[string]interface{}{"nodeId": node.ID, "nodeToken": node.Token},
		})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"note":    "Admitted. The node retries every 30 seconds, so it should be back within a minute.",
	})
}

// DismissJoinAttempt DELETE /api/admin/nodes/join-attempts/{token} — drop a row
// an operator has decided is not theirs to act on. It comes back if the machine
// keeps trying, which is the point: dismissing is not blocking.
func (h *NodeAdmissionHandler) DismissJoinAttempt(w http.ResponseWriter, r *http.Request) {
	if err := h.state.Store.DeleteNodeJoinAttempt(mux.Vars(r)["token"]); err != nil {
		sendJSONError(w, "Database error", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}
