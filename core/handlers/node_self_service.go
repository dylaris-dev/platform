package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/redisacl"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// A tenant decommissioning their OWN machine.
//
// There was no way to. Deleting a node is DELETE /api/nodes/{id} behind the
// nodes.delete capability, which no customer holds - and that handler has no
// ownership check of its own, so opening it to tenants would have let any of
// them delete any node in the fleet. The result was a dead end that looked like
// a limit: a tenant who wanted to move their node to a different machine could
// not remove the old one, their node count stayed at the cap, and the screen
// told them to buy a second location to solve a problem that was not capacity.
//
// So this is a separate, narrower surface rather than a relaxed gate on the
// existing one. /me means MINE: it answers only for a node whose owner_id is
// the caller, admin or not. An admin managing somebody else's machine still
// goes through the capability-gated route, which is untouched.

// nodeServerContents is one server that would be destroyed, as the confirmation
// screen lists it.
//
// It carries what Core can VOUCH for. The running container's name is
// deliberately not among them: the node composes it, this process does not, and
// a name guessed wrong is worst exactly here - in the dialog whose whole job is
// to tell someone precisely what they are about to lose.
type nodeServerContents struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	UUID string `json:"uuid"`
	// SubServers is every install on the server, by name. Read from the
	// database rather than from the node, so the list is still correct while
	// the machine being removed is already offline - which is the normal case
	// when someone is decommissioning it.
	SubServers []string `json:"subServers"`
	// ActiveSubServer is the one currently booted, so the reader can tell the
	// world they were playing on from the ones they forgot about.
	ActiveSubServer string `json:"activeSubServer,omitempty"`
}

// myNode resolves the node in the path and confirms the caller OWNS it.
//
// Ownership, not canManageNode: that helper answers yes for an admin on every
// PLATFORM node, and on a route called /me that would quietly mean "any node in
// the fleet". Here the only question is whether this row belongs to the person
// asking.
func (h *NodeHandler) myNode(w http.ResponseWriter, r *http.Request) (*models.Node, bool) {
	if h.state.Store == nil {
		sendJSONError(w, "DB error", 503)
		return nil, false
	}
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid node id", 400)
		return nil, false
	}
	node, err := h.state.Store.GetNodeByID(id)
	if err != nil || node == nil {
		sendJSONError(w, "Node not found", 404)
		return nil, false
	}
	uid := byonCallerID(r)
	if uid == "" || node.OwnerID == nil || *node.OwnerID != uid {
		// 404 rather than 403: on a /me route, "not yours" and "does not exist"
		// are the same answer, and the difference would confirm the existence of
		// other tenants' nodes to anyone who guessed an id.
		sendJSONError(w, "Node not found", 404)
		return nil, false
	}
	return node, true
}

// GetMyNodeContents GET /api/me/nodes/{id}/contents - what removing this
// machine would destroy.
//
// Its own endpoint because the confirmation has to be built BEFORE anything is
// deleted, and because the panel shows it in a second dialog that names every
// world by name. A count would not be enough: "3 servers" is a number somebody
// clicks past, and the sub-server they forgot they had is the one they wanted.
func (h *NodeHandler) GetMyNodeContents(w http.ResponseWriter, r *http.Request) {
	node, ok := h.myNode(w, r)
	if !ok {
		return
	}
	servers, err := h.state.Store.ListServersByNode(node.ID)
	if err != nil {
		sendJSONError(w, "Failed to load servers", 500)
		return
	}
	out := make([]nodeServerContents, 0, len(servers))
	for _, s := range servers {
		item := nodeServerContents{
			ID: s.ID, Name: s.Name, UUID: s.UUID,
			ActiveSubServer: s.ActiveSubServer,
			SubServers:      []string{},
		}
		// A failure here must not hide the server itself. The list is a
		// courtesy; the server going away is the fact, and dropping the row
		// because its installs could not be read would understate the loss.
		installs, ierr := h.state.Store.ListSubServerInstalls(s.ID)
		if ierr != nil {
			log.Printf("node contents: installs for server %d: %v", s.ID, ierr)
		}
		for _, in := range installs {
			item.SubServers = append(item.SubServers, in.SubServerName)
		}
		out = append(out, item)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"node":    map[string]interface{}{"id": node.ID, "name": node.Name, "status": node.Status},
		"servers": out,
	})
}

// DeleteMyNode DELETE /api/me/nodes/{id}?servers=delete - removes the caller's
// own machine.
//
// servers=delete takes the servers with it; anything else refuses while any
// remain. The refusal is the default deliberately: the two reasons to remove a
// machine are "I am moving it" and "I am done with it", and only the second
// wants the worlds gone.
//
// Being ONLINE does not block it, which is where this parts company with
// force-delete. That guard protects an operator from stranding containers on a
// machine they do not control; here the caller owns the hardware, and refusing
// would mean the only way to release a node slot is to first go and switch the
// machine off. The containers keep running until they stop them, and since
// containers carry the id of the node that made them, a later node on the same
// machine will not adopt them.
func (h *NodeHandler) DeleteMyNode(w http.ResponseWriter, r *http.Request) {
	node, ok := h.myNode(w, r)
	if !ok {
		return
	}
	withServers := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("servers")), "delete")
	warpKeys := h.boundWarpKeys(node)

	// Read before the delete: afterwards there is no row left to name the
	// addresses and Redis keys of these servers. A failed read is a silent leak,
	// so it is logged rather than dropped.
	var servers []models.Server
	if withServers {
		var lerr error
		if servers, lerr = h.state.Store.ListServersByNode(node.ID); lerr != nil {
			log.Printf("DeleteMyNode: listing the servers of node %d failed, their addresses and Redis keys cannot be cleaned up: %v", node.ID, lerr)
		}
		purgeBackupArchivesForServers(h.state, services.ServerIDs(servers))
		if err := h.state.Store.DeleteServersByNode(node.ID); err != nil {
			sendJSONError(w, "Failed to delete the servers on this machine", 500)
			return
		}
	}

	if err := h.state.Store.DeleteNode(node.ID); err != nil {
		if errors.Is(err, store.ErrNodeHasServers) {
			sendJSONError(w, "This machine still has servers on it. Delete them with it, or move them to another machine first.", 409)
			return
		}
		sendJSONError(w, "Delete failed", 500)
		return
	}

	// What went with the machine. Nothing else removes it - DeleteServersByNode is
	// raw SQL and the hub keeps its own copy of the routes - so the customer's own
	// address kept answering after they removed the machine it pointed at.
	//
	// Background, not the request context: a browser that timed out must not be
	// the reason an address outlives the machine behind it.
	matched := services.RemoveDeletedServers(context.Background(), h.state.Gateway, h.state.Redis, services.ServerUUIDs(servers))
	log.Printf("DeleteMyNode: node %d — cleaned up %d route(s) across %d server(s)", node.ID, matched, len(servers))

	// The Redis ACL user and the node's keys are all keyed by its token, which
	// is why the row was read first. Best-effort, and logged rather than
	// swallowed: leftovers here are a credential that outlives the thing it
	// belonged to.
	h.cleanupDeletedNode(r, node, warpKeys)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// ResetMyNodePairing POST /api/me/nodes/{id}/reset-pairing - the owner's own
// Reset pairing, for a machine whose key is wedged or may have leaked.
//
// Before this, the only way out was to remove the machine and add it again: a
// new identity, a node slot, and its servers orphaned. The operator's route
// needs nodes.write, which no customer holds, and refuses a customer's machine
// even to an admin.
func (h *NodeHandler) ResetMyNodePairing(w http.ResponseWriter, r *http.Request) {
	node, ok := h.myNode(w, r)
	if !ok {
		return
	}
	if err := resetNodePairing(r.Context(), h.state, node, byonCallerID(r)); err != nil {
		log.Printf("reset-my-pairing: %v", err)
		sendJSONError(w, "Failed to reset pairing", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"note": "The machine's key is refused and its secret cleared. It keeps retrying with a new key; " +
			"admit it here once it shows up. It then restarts its servers, which disconnects their players.",
	})
}

// GetMyNodeJoinAttempt GET /api/me/nodes/{id}/join-attempt - the connection
// Core is refusing for this machine, if any, with the fingerprint of the key it
// presented. null when nothing is waiting.
func (h *NodeHandler) GetMyNodeJoinAttempt(w http.ResponseWriter, r *http.Request) {
	node, ok := h.myNode(w, r)
	if !ok {
		return
	}
	a, err := h.state.Store.GetNodeJoinAttempt(node.Token)
	if err != nil {
		sendJSONError(w, "Database error", http.StatusInternalServerError)
		return
	}
	var out interface{}
	if a != nil {
		// Only what the owner needs to recognise their machine. The token stays
		// out: it is the node's id, and this is a response a browser caches.
		out = map[string]interface{}{
			"presentedKey":  a.PresentedKey,
			"hostname":      a.Hostname,
			"reason":        a.Reason,
			"attempts":      a.Attempts,
			"lastSeenAt":    a.LastSeenAt,
			"approvedUntil": a.ApprovedUntil,
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "attempt": out})
}

// normalizeFingerprint accepts the fingerprint as a person copies it: any case,
// with or without the dashes the node prints between groups.
func normalizeFingerprint(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "")
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return s
}

// AdmitMyNode POST /api/me/nodes/{id}/admit {fingerprint} - let this machine
// back in with the key its owner read off the machine's own log.
//
// Bound to the KEY and not to the address or to what is knocking: see
// ApproveNodeJoinAttemptForKey. The panel shows what is knocking only as a
// hint; the owner types or pastes the fingerprint from the machine itself.
func (h *NodeHandler) AdmitMyNode(w http.ResponseWriter, r *http.Request) {
	node, ok := h.myNode(w, r)
	if !ok {
		return
	}
	var req struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Fingerprint) == "" {
		sendJSONError(w, "Say which key to admit", http.StatusBadRequest)
		return
	}
	fp := normalizeFingerprint(req.Fingerprint)
	if len(fp) != 16 && len(fp) != 64 {
		sendJSONError(w, "Paste the fingerprint your machine logs: the line starting with \"nodekey: this node's key fingerprint is\".", http.StatusBadRequest)
		return
	}
	// Same order as the operator's Admit: take the current login away first, or
	// the next connect is answered with a challenge and never reaches the check.
	if err := (&NodeAdmissionHandler{state: h.state}).revokeNodeLogin(node.ID, false); err != nil {
		log.Printf("admit-my-node: %v", err)
		sendJSONError(w, "Failed to admit the machine", http.StatusInternalServerError)
		return
	}
	if h.state.Redis != nil {
		redisacl.NewProvisioner(h.state.Redis).RemoveNodeACL(r.Context(), node.Token)
	}
	uid := byonCallerID(r)
	armed, err := h.state.Store.ApproveNodeJoinAttemptForKey(node.Token, fp, uid)
	if err != nil {
		sendJSONError(w, "Database error", http.StatusInternalServerError)
		return
	}
	if !armed {
		sendJSONError(w, "The admission could not be armed. Try again.", http.StatusInternalServerError)
		return
	}
	_ = h.state.Store.InsertAuditIdentity(&models.AuditEventIdentity{
		EventType:   "node.join_approved",
		ActorUserID: &uid,
		Metadata:    map[string]interface{}{"nodeId": node.ID, "nodeToken": node.Token, "keyFingerprint": fp, "by": "owner"},
	})
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"note":    "Admitted. The machine retries every 30 seconds, so it should be back within a minute.",
	})
}
