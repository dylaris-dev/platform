package handlers

import (
	"dylaris-core/models"
)

// redactNodeAddress blanks the node's public address on servers where the
// caller has no use for it.
//
// The address used to ride along on every server object, so anyone who could
// SEE a server learned the public IP of the machine it runs on - measured on
// production with a second account holding nothing but console.read on one
// server. The panel already knew better and renders it only where it means
// something (a direct-connect address when routing is not through the gateway,
// and the SFTP host), which is exactly the shape of a leak: the UI hid what the
// API handed out.
//
// It matters because in gateway routing the containers bind no host port and
// the edge is the only way in. The node's address is then not an address a
// player needs - it is the one an attacker would rather have, since traffic
// sent straight at it never passes the edge.
//
// Kept where it is genuinely needed:
//   - an admin, who administers the machines anyway;
//   - routing that is not the gateway, where the address IS how players connect;
//   - SFTP file access, where it is the host the customer types;
//   - a node the caller owns, which is their own machine.
//
// hostPort is deliberately left alone: without an address it names nothing, and
// the panel's direct-connect line already requires both.
func redactNodeAddress(state *AppState, servers []models.Server, isAdmin bool, userID string) {
	if state == nil || state.Store == nil || len(servers) == 0 || isAdmin {
		return
	}
	if !state.gatewayEnabled() {
		return
	}
	if mode, _ := state.Store.GetSetting("file_access_mode"); mode == "sftp" || mode == "both" {
		return
	}
	// One lookup per node rather than per server: a list is usually several
	// servers on the same machine.
	ownNode := map[int]bool{}
	for i := range servers {
		id := servers[i].NodeID
		own, seen := ownNode[id]
		if !seen {
			node, err := state.Store.GetNodeByID(id)
			own = err == nil && node != nil && node.OwnerID != nil && *node.OwnerID == userID && userID != ""
			ownNode[id] = own
		}
		if !own {
			servers[i].NodeAddress = ""
		}
	}
}

// redactNodeAddressOne is redactNodeAddress for a single server.
func redactNodeAddressOne(state *AppState, srv *models.Server, isAdmin bool, userID string) {
	if srv == nil {
		return
	}
	one := []models.Server{*srv}
	redactNodeAddress(state, one, isAdmin, userID)
	srv.NodeAddress = one[0].NodeAddress
}
