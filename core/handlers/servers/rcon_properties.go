package servers

import (
	"dylaris-core/handlers/nodeops"
	"dylaris-core/services/voicecfg"
	"fmt"
	"strconv"
)

// rconNeedsStamping reports whether a sub-server that is about to become the
// running one should have the stored RCON config written into its
// server.properties first.
//
// The reason this is needed at all: rcon_enabled/port/password are ONE row per
// SERVER, but enable-rcon lives in EACH SUB-SERVER's server.properties, and
// SetConfig only ever writes the sub-server that was active when it ran. So
// switching sub-servers (or starting one installed later) left the file behind
// while Core went on reporting RCON as enabled with nothing pending - the panel
// unlocked all four Players tabs on that word and every action then failed with
// a raw dial error. Found live: DB said enabled/no-restart, creative's file said
// enable-rcon=false, survival's said true.
//
// The password check is what keeps this from stamping three keys onto every
// server on the platform at every start: a server that has never configured
// RCON has no password, and its file is left alone.
func rconNeedsStamping(subServer string, enabled bool, password string) bool {
	if subServer == "" {
		return false
	}
	return enabled || password != ""
}

// applyRconToServerProperties merges the RCON settings into the active
// sub-server's server.properties on the node, so MC actually opens (or, when
// disabled, stops re-opening on next launch) the RCON port. Read-modify-write
// over the gRPC file API: read the current file, set the three rcon keys, write
// it back.
//
// Safety: a genuinely missing file (node returns code 404) means the server has
// never generated server.properties yet; we then write a fresh 3-key file and
// MC fills the rest on first boot. Any OTHER read error aborts WITHOUT writing,
// so a transient node/storage failure can never clobber an existing file (world
// settings, motd, ...) with a 3-key stub. The caller must not flip the DB flag
// unless this returns nil, keeping the DB and server.properties consistent.
func (h *RconHandler) applyRconToServerProperties(nodeID int, serverUUID, activeSubServer string, enabled bool, port int, password string) error {
	if activeSubServer == "" {
		return fmt.Errorf("server has no active sub-server yet - install or start it before configuring RCON")
	}
	if port == 0 {
		port = nodeops.DefaultRconPort
	}
	relPath := activeSubServer + "/" + nodeops.ServerPropertiesName

	existing, found, err := nodeops.ReadNodeFileString(h.d.GRPCRegistry, nodeID, serverUUID, relPath)
	if err != nil {
		return err
	}
	if !found {
		existing = ""
	}

	enabledStr := "false"
	if enabled {
		enabledStr = "true"
	}
	merged := voicecfg.MergeServerProperties(existing, map[string]string{
		"enable-rcon":   enabledStr,
		"rcon.port":     strconv.Itoa(port),
		"rcon.password": password,
	})

	return nodeops.WriteNodeFileString(h.d.GRPCRegistry, nodeID, serverUUID, relPath, merged)
}
