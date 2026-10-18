package handlers

import (
	"context"
	"fmt"
	"log"
	"path"
	"strings"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	pb "dylaris-proto/node"
)

// A Technic pack or an uploaded server pack carries no manifest Core can read,
// so its members are found the way IdentifyMods finds them: hash the jars in
// mods/ on the node and ask Modrinth. That costs a node round trip and a
// Modrinth call, so it runs once, lazily, the first time the Content tab asks
// for the snapshot, and its outcome is kept in sub_server_installs.

const (
	modpackScanPending = "pending"
	modpackScanDone    = "done"

	// modpackScanCooldown is also how long a failed scan waits before the next
	// panel load may try again: the lock is not released on failure.
	modpackScanCooldown = 5 * time.Minute
	// modpackScanWait bounds the panel request. A scan still running then
	// finishes in the background and the next load reads its result.
	modpackScanWait = 60 * time.Second
)

// modpackScanInstallers are the installers whose packs have no manifest.
var modpackScanInstallers = map[string]bool{"technic": true, "upload": true, "upload-zip": true}

// nextModpackScan is the scan state an install leaves behind. A reinstall is
// sent the launcher ("forge") rather than "upload", so it keeps a scan that
// was already owed or done instead of clearing it, and redoes it: a reinstall
// can change what is in mods/.
func nextModpackScan(installerType, current string, reinstall bool) string {
	if modpackScanInstallers[installerType] || (reinstall && current != "") {
		return modpackScanPending
	}
	return ""
}

func modpackScanLockKey(serverID int, subServer string) string {
	return fmt.Sprintf("dylaris:modpack:scan:%d:%s", serverID, subServer)
}

// scheduleModpackScan records the scan state for an install and drops any lock
// a previous attempt left, so the fresh install is scanned on the next load
// rather than after the cooldown.
func (h *ServerHandler) scheduleModpackScan(serverID int, subServer, installerType string, reinstall bool) {
	current, _, _, err := h.state.Store.GetModpackScan(serverID, subServer)
	if err != nil {
		log.Printf("modpack scan: reading state for server %d sub %q: %v", serverID, subServer, err)
		return
	}
	if err := h.state.Store.SetModpackScan(serverID, subServer, nextModpackScan(installerType, current, reinstall), 0); err != nil {
		log.Printf("modpack scan: scheduling for server %d sub %q: %v", serverID, subServer, err)
	}
	if h.state.Redis != nil {
		h.state.Redis.Del(context.Background(), modpackScanLockKey(serverID, subServer))
	}
}

// isModpackServer answers the panel's "is this a modpack" question. Jars count
// even when Modrinth knew none of them: a pack of private builds is still a
// pack, and its members are just not cross-checkable.
func isModpackServer(contents int, installerType string, jars int) bool {
	return contents > 0 || installerType == "modpack" || installerType == "pack" || jars > 0
}

// modpackScanDeps are the node and Modrinth round trips, swappable in tests.
type modpackScanDeps struct {
	list   func(dir string) ([]*pb.FileInfo, error)
	hash   func(dir string, names []string) ([]*pb.FileHash, error)
	lookup func(sha1s []string) (map[string]services.ModrinthVersion, error)
}

func (h *ServerModsHandler) modpackScanDepsFor(srv *models.Server) modpackScanDeps {
	if h.scanDeps != nil {
		return h.scanDeps(srv)
	}
	return modpackScanDeps{
		list: func(dir string) ([]*pb.FileInfo, error) { return h.listServerDir(srv.NodeID, srv.UUID, dir) },
		hash: func(dir string, names []string) ([]*pb.FileHash, error) {
			return h.hashServerFiles(srv.NodeID, srv.UUID, dir, names)
		},
		lookup: services.ModrinthByHashes,
	}
}

// maybeScanModpack runs the owed scan when nobody else is, and waits for it up
// to modpackScanWait. It reports whether the scan finished in that time.
func (h *ServerModsHandler) maybeScanModpack(srv *models.Server) bool {
	if srv.Status == "installing" || h.state.Redis == nil {
		return false
	}
	key := modpackScanLockKey(srv.ID, srv.ActiveSubServer)
	ok, err := h.state.Redis.SetNX(context.Background(), key, "1", modpackScanCooldown).Result()
	if err != nil || !ok {
		return false
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := scanModpackJars(h.state.Store, srv, h.modpackScanDepsFor(srv)); err != nil {
			log.Printf("modpack scan for server %d sub %q: %v", srv.ID, srv.ActiveSubServer, err)
		}
	}()
	select {
	case <-done:
		return true
	case <-time.After(modpackScanWait):
		return false
	}
}

// scanModpackJars hashes the unmanaged jars in mods/ and records what Modrinth
// knows of them. plugins/ is never scanned: a plugin server is not a modpack.
// Jars with a server_mods row were installed through the Content tab, not by
// the pack. Any error leaves the scan pending for the next attempt.
func scanModpackJars(st store.Store, srv *models.Server, deps modpackScanDeps) error {
	sub := srv.ActiveSubServer
	// The generation this scan works for; a reinstall during it owes a new one.
	_, _, gen, err := st.GetModpackScan(srv.ID, sub)
	if err != nil {
		return fmt.Errorf("reading scan state: %w", err)
	}
	dir := path.Join(sub, "mods")
	entries, err := deps.list(dir)
	if err != nil {
		return fmt.Errorf("listing mods: %w", err)
	}
	managed := map[string]bool{}
	mods, err := st.ListServerMods(srv.ID, sub)
	if err != nil {
		return fmt.Errorf("listing managed mods: %w", err)
	}
	for _, m := range mods {
		if m.TargetDir != "plugins" {
			managed[m.FileName] = true
		}
	}
	var names []string
	for _, e := range entries {
		if e != nil && !e.IsDir && isModJarName(e.Name) && !managed[e.Name] {
			names = append(names, e.Name)
		}
	}
	var rows []models.ServerModpackContent
	if len(names) > 0 {
		hashes, err := deps.hash(dir, names)
		if err != nil {
			return fmt.Errorf("hashing mods: %w", err)
		}
		var sha1s []string
		for _, fh := range hashes {
			if fh.Error == "" {
				sha1s = append(sha1s, fh.Sha1)
			}
		}
		known, err := deps.lookup(sha1s)
		if err != nil {
			return fmt.Errorf("modrinth lookup: %w", err)
		}
		rows = rowsFromHashes(srv.ID, sub, hashes, known)
	}
	if _, err := st.CompleteModpackScan(srv.ID, sub, gen, rows, len(names)); err != nil {
		return fmt.Errorf("recording scan: %w", err)
	}
	return nil
}

// isModJarName counts a jar the pack ships disabled too: it is still the
// pack's, and removing it is as much a change to the pack.
func isModJarName(name string) bool {
	n := strings.ToLower(name)
	return strings.HasSuffix(n, ".jar") || strings.HasSuffix(n, ".jar.disabled")
}

// rowsFromHashes maps hashed jars to snapshot rows. A pack that ships one
// project twice keeps the first jar, matching the snapshot's one-row-per-project
// key. Modrinth's version answer has no side, so every row is "both".
func rowsFromHashes(serverID int, subServer string, hashes []*pb.FileHash, known map[string]services.ModrinthVersion) []models.ServerModpackContent {
	rows := []models.ServerModpackContent{}
	seen := map[string]bool{}
	for _, fh := range hashes {
		if fh == nil || fh.Error != "" {
			continue
		}
		v, ok := known[fh.Sha1]
		if !ok || v.ProjectID == "" || seen[v.ProjectID] {
			continue
		}
		seen[v.ProjectID] = true
		rows = append(rows, models.ServerModpackContent{
			ServerID:              serverID,
			SubServerName:         subServer,
			ModrinthProjectID:     v.ProjectID,
			ModrinthVersionID:     v.ID,
			ModrinthVersionNumber: v.VersionNum,
			FileName:              fh.Name,
			Side:                  models.SideBoth,
		})
	}
	return rows
}
