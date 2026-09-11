package handlers

import (
	"encoding/json"
	"fmt"
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

// revokeNodeLogin takes away what the node logs in with. A key node is one that
// holds or has held an Ed25519 key.
//
// A key node has its key moved aside: its next connect is told to generate a new
// one, and the new one gets in only through a cluster proof (a platform node's)
// or an admission.
//
// rotate says whether the SERVICE secret goes as well. Every Redis password, the
// heartbeat signature, the beam proofs and the migration token derive from it, so
// Reset pairing - the answer to a node that may be compromised - passes true:
// leaving the secret would leave every one of those with whoever holds it. The
// cost is paid knowingly: the re-admitted node is handed a new secret, restarts,
// and recreates its MC containers, disconnecting their players. Roll key and an
// approval pass false. They are routine, the key is the login they replace, and a
// key node keeps its secret and its players. A node that never had a key logs in
// with the secret itself, so for it the secret is cleared whatever rotate says.
//
// Reset's writes - key, secret, and any admission already armed for the node -
// are ONE statement (ResetNodeLogin): apart, a failure between them left the
// Roll key state behind a Reset, or a Roll key's admission open after it. For
// Roll and an
// approval the order is harmless: a node without a key has nothing to move
// aside, so the only write that changes its row is the clear.
func (h *NodeAdmissionHandler) revokeNodeLogin(nodeID int, rotate bool) error {
	if rotate {
		if err := h.state.Store.ResetNodeLogin(nodeID); err != nil {
			return fmt.Errorf("reset login of node %d: %w", nodeID, err)
		}
		return nil
	}
	keyNode, err := h.state.Store.RejectNodePublicKey(nodeID)
	if err != nil {
		return fmt.Errorf("reject key of node %d: %w", nodeID, err)
	}
	if !keyNode {
		if err := h.state.Store.SetNodeSecretEnc(nodeID, ""); err != nil {
			return fmt.Errorf("clear secret of node %d: %w", nodeID, err)
		}
	}
	return nil
}

// ResetPairing POST /api/admin/nodes/{id}/reset-pairing - REVOKE: refuse the
// node's key, clear its secret and hard-cut its live Redis ACL. The node row /
// owner / servers / backups are untouched; the re-admitted node gets a new
// secret and restarts its game servers. See revokeNodeLogin for why.
//
// It also disarms any admission armed for the node before it, such as the one a
// Roll key arms: otherwise whoever sits at that address re-pairs with a fresh
// key right after the Reset and is handed the new secret. An Admit given after
// the Reset arms it again, which is how a reset node comes back.
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

	// Revoke the login: the next reconnect goes down the branch that accepts a
	// cluster proof or an admission granted here.
	if err := h.revokeNodeLogin(node.ID, true); err != nil {
		log.Printf("reset-pairing: %v", err)
		sendJSONError(w, "Failed to reset pairing", http.StatusInternalServerError)
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
		"note":    "The node's key is refused, its secret cleared, and any re-admission armed earlier by Roll key cancelled. A node holding the cluster secret re-pairs itself within seconds; any other node will appear under Connection attempts, where you can admit it. Once back it gets a new secret and restarts its game servers, which disconnects their players.",
	})
}

// RollSecret POST /api/admin/nodes/{id}/roll-secret - replace the node's key
// (the secret, for a node that has no key) and let it straight back in from the
// address it last authenticated from. The route keeps its old name because the
// panel and the audit trail already use it. Unlike Reset pairing it keeps a key
// node's service secret, so the node's game servers are not restarted (see
// revokeNodeLogin).
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

	// Same order as ApproveJoinAttempt: the login goes first, because a node
	// whose key or secret Core still accepts is answered with a challenge and
	// never reaches the branch that consumes an admission.
	if err := h.revokeNodeLogin(node.ID, false); err != nil {
		log.Printf("roll-secret: %v", err)
		sendJSONError(w, "Failed to revoke the node's key", http.StatusInternalServerError)
		return
	}
	if h.state.Redis != nil {
		redisacl.NewProvisioner(h.state.Redis).RemoveNodeACL(r.Context(), node.Token)
	}
	// Audited once the login is gone, before arming: the revocation has
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
		// The login is already gone by this point, so the node has changed
		// state either way - log why arming failed, or this 500 explains
		// nothing afterwards.
		log.Printf("roll-secret: node %d: arm join approval failed (armed=%v): %v", node.ID, armed, err)
		sendJSONError(w, "The node's key is revoked, but its re-admission could not be armed. It will appear under Connection attempts, where you can admit it.", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"note":    "Key rolled. The node's Redis access is cut at once. The re-admission is armed for 15 minutes; a node that is online reconnects with a new key well within that window, without restarting its game servers (a node too old to have a key gets a new secret instead, which does restart them).",
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
// Two acts, in this order. The node's login is revoked (revokeNodeLogin), which
// is what sends its next connect down the branch that checks admissions at all;
// then the admission is armed. Doing it the other way round would arm a door
// that the next connect walks straight past, because a node whose key or secret
// Core still accepts is answered with a challenge and never reaches that branch.
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
	if err := h.revokeNodeLogin(node.ID, false); err != nil {
		log.Printf("approve-join: %v", err)
		sendJSONError(w, "Failed to revoke the node's key", http.StatusInternalServerError)
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
