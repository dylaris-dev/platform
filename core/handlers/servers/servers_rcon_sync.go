package servers

import (
	"log"

	"dylaris-core/handlers"

	"dylaris-core/models"
)

// syncRconToSubServer stamps the server's stored RCON config into subServer's
// server.properties, so the file agrees with what Core reports and the Players
// tabs act on. Returns whether the sub-server can be considered in step: true
// also when there was nothing to stamp, false only when a write was needed and
// did not land.
//
// Best-effort by design: it runs on the (re)start and switch paths, and a node
// that cannot be written to must not stop a server from starting. What it must
// not do is let the caller then report the config as applied - hence the bool.
func (h *ServerHandler) syncRconToSubServer(srv *models.Server, subServer string) bool {
	// Ahead of the DB read, not folded into rconNeedsStamping below: a power
	// action on a server that was never installed has no properties file to
	// write, and no reason to ask the database about one.
	if subServer == "" {
		return true
	}
	if h.state == nil || h.state.Store == nil {
		return false
	}
	enabled, port, password, err := h.state.Store.GetServerRconConfig(srv.ID)
	if err != nil {
		log.Printf("rcon sync for server %d: reading the stored config failed: %v", srv.ID, err)
		return false
	}
	if !handlers.RconNeedsStamping(subServer, enabled, password) {
		return true
	}
	if err := handlers.NewRconHandler(h.state).ApplyProps(srv.NodeID, srv.UUID, subServer, enabled, port, password); err != nil {
		log.Printf("rcon sync for server %d (%s): writing server.properties failed: %v", srv.ID, subServer, err)
		return false
	}
	return true
}
