package handlers

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"dylaris-core/models"
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

	if withServers {
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

	// The Redis ACL user and the node's keys are all keyed by its token, which
	// is why the row was read first. Best-effort, and logged rather than
	// swallowed: leftovers here are a credential that outlives the thing it
	// belonged to.
	h.cleanupDeletedNode(r, node.Token)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
