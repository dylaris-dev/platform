package servers

import (
	"dylaris-core/handlers/nodeops"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// mergeServerProperties returns the server.properties content with each key in
// kv set to its value, preserving every other line, comment, and the existing
// ordering. A key already present has only its value replaced; a key not
// present is appended (appended keys are emitted in sorted order for a
// deterministic result). The Minecraft server.properties format is a flat
// key=value file (one pair per line, '#' or '!' introduces a comment), which
// is what this parses - no line continuations, no escaping.
//
// This is how RCON config reaches the actual server: Core owns the DB flags,
// but MC only opens the RCON port if enable-rcon/rcon.port/rcon.password live
// in server.properties, so on enable we merge them in and the file (durable in
// the server directory) keeps them across restarts.
func mergeServerProperties(existing string, kv map[string]string) string {
	remaining := make(map[string]string, len(kv))
	for k, v := range kv {
		remaining[k] = v
	}

	// Normalize CRLF so we split cleanly; MC writes LF.
	lines := strings.Split(strings.ReplaceAll(existing, "\r\n", "\n"), "\n")

	// A trailing newline yields a final empty element; drop it so we don't
	// grow a blank line on every write, then re-add exactly one at the end.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	out := make([]string, 0, len(lines)+len(kv))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "!") {
			out = append(out, line)
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			out = append(out, line)
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if newVal, ok := remaining[key]; ok {
			out = append(out, key+"="+newVal)
			delete(remaining, key)
			continue
		}
		out = append(out, line)
	}

	appendKeys := make([]string, 0, len(remaining))
	for k := range remaining {
		appendKeys = append(appendKeys, k)
	}
	sort.Strings(appendKeys)
	for _, k := range appendKeys {
		out = append(out, k+"="+remaining[k])
	}

	return strings.Join(out, "\n") + "\n"
}

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

	existing, found, err := nodeops.ReadNodeFileString(h.state.GRPCRegistry, nodeID, serverUUID, relPath)
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
	merged := mergeServerProperties(existing, map[string]string{
		"enable-rcon":   enabledStr,
		"rcon.port":     strconv.Itoa(port),
		"rcon.password": password,
	})

	return nodeops.WriteNodeFileString(h.state.GRPCRegistry, nodeID, serverUUID, relPath, merged)
}
