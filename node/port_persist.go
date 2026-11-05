package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// The port ledger lives in Redis, which is in-memory only, and its other record
// is the running containers. A move from Compose to Swarm, an uninstall and
// reinstall, or a Redis restart while the containers are being recreated loses
// both, and the reconciler then handed out ports again in whatever order the
// servers came up: two servers swapped addresses, silently. The saved
// .node_config.json is on the data volume and survives all of that, so it
// carries the port too.

// savedHostPorts maps serverUUID -> the host port its saved config holds,
// for ports inside [rangeStart, rangeEnd] that may be published at all. The
// file sits in the tenant's /data, so a planted port must not reserve a port
// outside the game range or one of the node's own listeners.
func savedHostPorts(storagePaths []string, rangeStart, rangeEnd int) map[string]int {
	out := map[string]int{}
	for _, sp := range storagePaths {
		entries, err := os.ReadDir(sp)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || e.Name() == "" || e.Name()[0] == '.' {
				continue
			}
			data, err := readServerRootFile(filepath.Join(sp, e.Name()), ".node_config.json")
			if err != nil {
				continue
			}
			var saved ServerConfig
			if json.Unmarshal(data, &saved) != nil {
				continue
			}
			p := saved.Docker.HostPort
			if p < rangeStart || p > rangeEnd || hostPortProblem(p) != "" {
				continue
			}
			// Keyed by the directory, not the file's uuid: the file sits in the
			// tenant's /data (see pinSavedConfig).
			out[e.Name()] = p
		}
	}
	return out
}

// restorePortLedger runs once at startup, after the running containers were
// adopted (they hold their ports for real, so they win a conflict). The saved
// ports are reserved next, so no server is handed a port another one had
// before its container went away. Then the ports of servers that are gone are
// released, and last, every port in the ledger is written back to its saved
// config, which backfills configs saved before they carried one.
//
// containers holds the uuid of every mc_ container on the host, nil when they
// could not be listed.
func (dm *DockerManager) restorePortLedger(containers map[string]bool) {
	dm.portMgr.AdoptExistingBindings(savedHostPorts(dm.storageMgr.Paths(), dm.portMgr.rangeStart, dm.portMgr.rangeEnd))
	dm.releaseGonePorts(containers)
	for uuid, port := range dm.portMgr.Snapshot() {
		dm.rememberHostPort(uuid, port)
	}
}

// releaseGonePorts drops ledger entries of servers with neither a container
// nor a directory on any storage path. A delete whose release never reached
// Redis leaves one, the keys have no TTL, and no container can ever bind that
// port to make AdoptContainerBindings free it, so it was lost to the range for
// good. Unknown containers, or a configured storage path that was skipped or
// cannot be read, may hide the server, so then nothing is released.
func (dm *DockerManager) releaseGonePorts(containers map[string]bool) {
	if containers == nil || dm.storageMgr.skipped > 0 {
		return
	}
	paths := dm.storageMgr.Paths()
	for _, sp := range paths {
		if _, err := os.ReadDir(sp); err != nil {
			return
		}
	}
	for uuid := range dm.portMgr.Snapshot() {
		if containers[uuid] {
			continue
		}
		gone := true
		for _, sp := range paths {
			if fi, err := os.Stat(filepath.Join(sp, uuid)); err == nil && fi.IsDir() {
				gone = false
				break
			}
		}
		if gone {
			log.Printf("PortManager: server %s has no container and no directory; releasing its port", uuid)
			dm.portMgr.ReleasePort(uuid)
		}
	}
}

// rememberHostPort writes the server's host port into its saved config.
func (dm *DockerManager) rememberHostPort(uuid string, port int) {
	if port > 0 {
		setSavedDockerField(dm.resolveLocalServerPath(uuid), "hostPort", port)
	}
}

// setSavedDockerField sets one field under "docker" in a server's saved config.
// Only that field is touched, through a generic map so fields this build does
// not know survive. No saved config means nothing to update: a partial file
// would read to the reconciler as a server it can rebuild.
func setSavedDockerField(dir, field string, value any) {
	data, err := readServerRootFile(dir, ".node_config.json")
	if err != nil {
		return
	}
	var saved map[string]any
	if json.Unmarshal(data, &saved) != nil {
		return
	}
	docker, _ := saved["docker"].(map[string]any)
	if docker == nil {
		docker = map[string]any{}
		saved["docker"] = docker
	}
	if cur, ok := docker[field]; ok && fmt.Sprint(cur) == fmt.Sprint(value) {
		return
	}
	docker[field] = value
	out, err := json.Marshal(saved)
	if err != nil {
		return
	}
	if err := writeServerRootFile(dir, ".node_config.json", out); err != nil {
		log.Printf("saved config %s: set %s: %v", dir, field, err)
	}
}

// ledgerHostPort is the port the running node holds for a server, or 0.
func ledgerHostPort(uuid string) int {
	if dockerManager == nil || dockerManager.portMgr == nil {
		return 0
	}
	return dockerManager.portMgr.GetPort(uuid)
}
