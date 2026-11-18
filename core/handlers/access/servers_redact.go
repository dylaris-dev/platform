package access

import (
	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"
)

// RedactNodeAddress blanks the node's public address on servers where the
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
//   - SFTP file access, where it is the host the customer types - per server,
//     for a caller holding sftp.access there (mayUseSFTP, nil = nobody), and
//     never on an external node;
//   - a node the caller owns, which is their own machine.
//
// hostPort is deliberately left alone: without an address it names nothing, and
// the panel's direct-connect line already requires both.
//
// gatewayEnabled is the platform's routing-mode check, passed in rather than
// read here so the one definition (services.GatewayRoutingEnabled) stays the only
// one; it is asked only after the cheap early returns, as before.
func RedactNodeAddress(st store.Store, gatewayEnabled func() bool, mayUseSFTP func(serverID int) bool, servers []models.Server, isAdmin bool, userID string) {
	if st == nil || len(servers) == 0 || isAdmin {
		return
	}
	if !gatewayEnabled() {
		return
	}
	// The file mode is platform-wide, so it alone said nothing about THIS
	// caller: a console-only guest or a demo viewer got the address too. It is
	// kept per row for the caller who may use SFTP there, on a node SFTP serves.
	mode, _ := st.GetSetting("file_access_mode")
	sftpMode := (mode == "sftp" || mode == "both") && mayUseSFTP != nil
	// A list row carries its node's owner from the list query
	// (NodeOwnerLoaded); any other row costs one lookup per node rather than
	// per server, since a list is usually several servers on one machine.
	ownNode := map[int]bool{}
	sftpNode := map[int]bool{}
	for i := range servers {
		id := servers[i].NodeID
		own, seen := ownNode[id]
		if !seen && servers[i].NodeOwnerLoaded {
			o := servers[i].NodeOwnerID
			own = o != nil && *o == userID && userID != ""
			ownNode[id] = own
		} else if !seen {
			node, err := st.GetNodeByID(id)
			own = err == nil && node != nil && node.OwnerID != nil && *node.OwnerID == userID && userID != ""
			ownNode[id] = own
		}
		if own {
			continue
		}
		if sftpMode && mayUseSFTP(servers[i].ID) {
			// External nodes never serve SFTP, the same rule GetSftpCredentials
			// applies; an unreadable node is withheld.
			serves, known := sftpNode[id]
			if !known {
				node, err := st.GetNodeByID(id)
				serves = err == nil && node != nil && !node.IsExternal()
				sftpNode[id] = serves
			}
			if serves {
				continue
			}
		}
		servers[i].NodeAddress = ""
	}
}

// RedactNodeAddressOne is RedactNodeAddress for a single server.
func RedactNodeAddressOne(st store.Store, gatewayEnabled func() bool, mayUseSFTP func(serverID int) bool, srv *models.Server, isAdmin bool, userID string) {
	if srv == nil {
		return
	}
	one := []models.Server{*srv}
	RedactNodeAddress(st, gatewayEnabled, mayUseSFTP, one, isAdmin, userID)
	srv.NodeAddress = one[0].NodeAddress
}

// SFTPAccess is RedactNodeAddress's mayUseSFTP from the resolver: whether the
// caller holds sftp.access on a server, resolved once per server. nil without a
// resolver, which keeps the address from nobody but admins and node owners.
func SFTPAccess(az *authz.Resolver, id authz.Identity) func(serverID int) bool {
	if az == nil {
		return nil
	}
	seen := map[int]bool{}
	return func(serverID int) bool {
		ok, done := seen[serverID]
		if !done {
			res, err := az.Resolve(id, serverID)
			ok = err == nil && res != nil && res.HasCap(sftpAccessCap)
			seen[serverID] = ok
		}
		return ok
	}
}

const sftpAccessCap = "sftp.access"

// SettingsFieldsCap governs the server fields only the Setup view needs.
const SettingsFieldsCap = "server.settings.write"

// RedactSettingsFields blanks what a member without SettingsFieldsCap has no
// screen for. The JVM flags are free text that can carry a secret
// (-Dsome.token=...), the start command is built from them, and the cpuset is
// the host's CPU layout. They went to every member with any capability on the
// server, a console-only guest included.
func RedactSettingsFields(s *models.Server) {
	s.StartCommand, s.ExtraJvmFlags, s.Cpuset, s.CPUPinningMode = "", "", "", ""
}
