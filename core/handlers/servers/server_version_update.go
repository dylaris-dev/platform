package servers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"dylaris-core/handlers/guard"
	"dylaris-core/services/modcompat"

	"dylaris-pkg/validate"
	pb "dylaris-proto/node"

	"dylaris-core/handlers/httpx"
	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

// Moving a modded server to another Minecraft version, and the two things that
// have to happen around it: seeing which jars nothing in the database claims,
// and copying the server first so the move is reversible.

// serverJarDirs are the two directories a loader jar can live in. Which one is
// in use depends on the loader; both are inspected because a server that
// changed loader can have leftovers in the other.
var serverJarDirs = []string{"mods", "plugins"}

// unmanagedFile is a jar on disk with no server_mods row naming it.
type unmanagedFile struct {
	Directory string `json:"directory"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
}

// UnmanagedMods GET /api/servers/{id}/mods/unmanaged
//
// A jar placed by hand (SFTP, beam, the file manager) has no row saying which
// Modrinth project it is, so a version move cannot carry it and cannot even
// tell whether it would survive. Naming those files is the difference between
// a migration that reports what it will do and one that quietly leaves the
// server unable to start.
func (h *ServerModsHandler) UnmanagedMods(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	srv, ok := h.getServer(serverID)
	if !ok {
		httpx.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	if srv.ActiveSubServer == "" {
		httpx.SendJSONError(w, "No active sub-server", http.StatusBadRequest)
		return
	}
	known, err := h.d.Store.ListServerMods(serverID, srv.ActiveSubServer)
	if err != nil {
		httpx.SendJSONError(w, "Failed to list mods", http.StatusInternalServerError)
		return
	}
	// A missing directory lists as EMPTY (listServerDirFor turns the node's 404
	// into that), so any error here is a real failure to look. Reporting that as "nothing
	// unmanaged" would hide exactly the thing this endpoint exists to reveal, so
	// a failure on every directory is surfaced instead of swallowed.
	listings := map[string][]dirEntry{}
	var lastErr error
	reached := 0
	for _, dir := range serverJarDirs {
		listed, err := h.listServerDir(srv.NodeID, srv.UUID, path.Join(srv.ActiveSubServer, dir))
		if err != nil {
			lastErr = err
			continue
		}
		reached++
		entries := make([]dirEntry, 0, len(listed))
		for _, f := range listed {
			entries = append(entries, dirEntry{Name: f.Name, IsDir: f.IsDir, Size: f.Size})
		}
		listings[dir] = entries
	}
	if reached == 0 && lastErr != nil {
		httpx.SendJSONError(w, "Could not read this server's files from its node: "+lastErr.Error(), http.StatusBadGateway)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"files":   unmanagedJars(known, srv.InstallerType, listings),
	})
}

// dirEntry is one listed filesystem entry, decoupled from the proto type so the
// diff below can be exercised without a node.
type dirEntry struct {
	Name  string
	IsDir bool
	Size  int64
}

// unmanagedJars subtracts the jars the database claims from the jars on disk.
//
// The claim is keyed by the SAME directory resolution the uninstall path uses
// (uninstallTargetDir: the recorded target_dir, falling back to the loader
// derivation only for rows written before that column existed). Re-deriving it
// from the loader here instead would report a plugin recorded in "plugins" as
// unmanaged the moment the server was re-declared as fabric, and the operator
// would be told to identify a jar the panel installed itself.
func unmanagedJars(known []models.ServerMod, installerType string, listings map[string][]dirEntry) []unmanagedFile {
	claimed := map[string]bool{}
	for i := range known {
		dir := uninstallTargetDir(&known[i], installerType)
		claimed[dir+"/"+known[i].FileName] = true
	}
	files := []unmanagedFile{}
	for _, dir := range serverJarDirs {
		for _, f := range listings[dir] {
			if f.IsDir || !strings.HasSuffix(strings.ToLower(f.Name), ".jar") {
				continue
			}
			if claimed[dir+"/"+f.Name] {
				continue
			}
			files = append(files, unmanagedFile{Directory: dir, Name: f.Name, Size: f.Size})
		}
	}
	return files
}

type identifyModsRequest struct {
	Files []struct {
		Directory string `json:"directory"`
		Name      string `json:"name"`
	} `json:"files"`
}

type identifyResult struct {
	Directory string `json:"directory"`
	Name      string `json:"name"`
	Matched   bool   `json:"matched"`
	Title     string `json:"title,omitempty"`
	ProjectID string `json:"projectId,omitempty"`
	VersionID string `json:"versionId,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// IdentifyMods POST /api/servers/{id}/mods/identify
//
// Asks the node to hash the named files and looks each hash up on Modrinth. A
// hit becomes a normal server_mods row, after which the file migrates like any
// other linked mod. A miss stays unidentified and stays reported: a jar that is
// not on Modrinth (a private build, a renamed fork) cannot be linked, and
// pretending otherwise would be worse than saying so.
func (h *ServerModsHandler) IdentifyMods(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	srv, ok := h.getServer(serverID)
	if !ok {
		httpx.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	if srv.ActiveSubServer == "" {
		httpx.SendJSONError(w, "No active sub-server", http.StatusBadRequest)
		return
	}
	var req identifyModsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if len(req.Files) == 0 {
		httpx.SendJSONError(w, "files required", http.StatusBadRequest)
		return
	}

	// Group by directory: one hash request per directory, not per file.
	byDir := map[string][]string{}
	for _, f := range req.Files {
		if f.Directory != "mods" && f.Directory != "plugins" {
			httpx.SendJSONError(w, "directory must be 'mods' or 'plugins'", http.StatusBadRequest)
			return
		}
		if !validate.IsPlainFileName(f.Name) {
			httpx.SendJSONError(w, "file names must be plain file names, without any path", http.StatusBadRequest)
			return
		}
		byDir[f.Directory] = append(byDir[f.Directory], f.Name)
	}

	// Which projects this sub-server already has a row for, and under which
	// file name.
	//
	// server_mods conflicts on (server_id, sub_server_name, modrinth_project_id),
	// so identifying a stray jar as a project that is ALREADY installed did not
	// add a row - it rewrote the existing one onto the stray's file name. The
	// managed jar then read as unmanaged, and the next version move removed the
	// stray while leaving the real one behind: two copies of one mod, and a
	// server that will not start.
	claimedBy := map[string]string{}
	if existing, lerr := h.d.Store.ListServerMods(serverID, srv.ActiveSubServer); lerr == nil {
		for _, m := range existing {
			if m.ModrinthProjectID != "" {
				claimedBy[m.ModrinthProjectID] = m.FileName
			}
		}
	}

	userID, _ := r.Context().Value("userID").(string)
	results := []identifyResult{}
	linked := 0
	for dir, names := range byDir {
		hashes, err := h.hashServerFiles(srv.NodeID, srv.UUID, path.Join(srv.ActiveSubServer, dir), names)
		if err != nil {
			httpx.SendJSONError(w, "Node could not hash these files: "+err.Error(), http.StatusBadGateway)
			return
		}
		var sha1s []string
		for _, fh := range hashes {
			if fh.Error == "" {
				sha1s = append(sha1s, fh.Sha1)
			}
		}
		known, lookupErr := services.ModrinthByHashes(sha1s)
		for _, fh := range hashes {
			res := identifyResult{Directory: dir, Name: fh.Name}
			if fh.Error != "" {
				res.Reason = fh.Error
				results = append(results, res)
				continue
			}
			if lookupErr != nil {
				// Not the same answer as "unknown", and it must not read like
				// one: an operator told a jar is not on Modrinth may well
				// delete it.
				res.Reason = "Modrinth could not be reached, so this file was not identified: " + lookupErr.Error()
				results = append(results, res)
				continue
			}
			v, found := known[fh.Sha1]
			if !found || v.ID == "" {
				res.Reason = "no Modrinth version has this file's hash"
				results = append(results, res)
				continue
			}
			if other, dup := duplicateOfInstalled(claimedBy, v.ProjectID, fh.Name); dup {
				res.Title = v.Name
				res.ProjectID = v.ProjectID
				res.Reason = "already installed as " + other + ", so this is a second copy: remove one of them"
				results = append(results, res)
				continue
			}
			mod := &models.ServerMod{
				ServerID:          serverID,
				SubServerName:     srv.ActiveSubServer,
				ModrinthProjectID: v.ProjectID,
				ModrinthVersionID: v.ID,
				Title:             v.Name,
				FileName:          fh.Name,
				TargetDir:         dir,
				SHA512:            fh.Sha512,
			}
			if userID != "" {
				u := userID
				mod.InstalledBy = &u
			}
			if _, err := h.d.Store.UpsertServerMod(mod); err != nil {
				res.Reason = "identified, but the database write failed: " + err.Error()
				results = append(results, res)
				continue
			}
			linked++
			// Claim it, so a SECOND stray copy of the same project inside this
			// one request is refused too rather than overwriting the row that
			// was just written.
			claimedBy[v.ProjectID] = fh.Name
			res.Matched = true
			res.Title = v.Name
			res.ProjectID = v.ProjectID
			res.VersionID = v.ID
			results = append(results, res)
		}
	}

	if linked > 0 {
		// A hash names the version, not the project: fetch the project's title
		// and slug for the new rows in one call, and answer with them.
		if mods, lerr := h.d.Store.ListServerMods(serverID, srv.ActiveSubServer); lerr == nil {
			h.fillProjectMeta(r.Context(), mods)
			titles := map[string]string{}
			for _, m := range mods {
				titles[m.ModrinthProjectID] = m.Title
			}
			for i := range results {
				if t := titles[results[i].ProjectID]; results[i].Matched && t != "" {
					results[i].Title = t
				}
			}
		}
		h.d.Events.Publish(r.Context(), "server_mods.changed", map[string]interface{}{"serverId": serverID})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "linked": linked, "results": results})
}

type versionUpdateRequest struct {
	Minecraft string `json:"minecraft"`
	// InstallerVersion is the loader/build version for the target Minecraft
	// version. Empty means the installer resolves latest for that version.
	InstallerVersion string `json:"installerVersion"`
	Loader           string `json:"loader"`
	JavaImage        string `json:"javaImage"`
	// DropUnavailable has to be set explicitly to proceed when some mods have
	// no version for the target. Without it the request is refused and the
	// mods that would be lost are named.
	DropUnavailable bool `json:"dropUnavailable"`
	// AfterCopy names the sub-server copy this move was asked to follow
	// ("copy first"). Until that copy is in place the move is answered
	// copy_running, and copy_failed when it will not appear.
	AfterCopy string `json:"afterCopy"`
}

// VersionUpdate POST /api/servers/{id}/version-update
func (h *ServerModsHandler) VersionUpdate(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	srv, ok := h.getServer(serverID)
	if !ok {
		httpx.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	if guard.RefuseIfSuspended(w, r, h.d.Store, h.d.SuspendGrace, srv) {
		return
	}
	var req versionUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.AfterCopy = strings.TrimSpace(req.AfterCopy)
	if req.AfterCopy != "" && !validate.IsSubServerName(req.AfterCopy) {
		httpx.SendJSONError(w, "afterCopy must be a single plain directory name", http.StatusBadRequest)
		return
	}
	if h.refuseUntilCopyLanded(w, r, srv, req.AfterCopy) {
		return
	}
	// The move stops the server and rewrites its jars, the same tree a restore,
	// a reinstall or a migration is working in.
	if guard.RefuseIfServerBusy(w, r, h.d.Redis, h.d.GRPCRegistry, srv) {
		return
	}
	req.Minecraft = strings.TrimSpace(req.Minecraft)
	if req.Minecraft == "" {
		httpx.SendJSONError(w, "minecraft is required", http.StatusBadRequest)
		return
	}
	if srv.ActiveSubServer == "" {
		httpx.SendJSONError(w, "No active sub-server to update", http.StatusBadRequest)
		return
	}
	if srv.Status == "pending_setup" {
		httpx.SendJSONError(w, "Set the server up before changing its Minecraft version", http.StatusBadRequest)
		return
	}
	// An install queued but not yet started holds no busy key on the node.
	if srv.Status == "installing" {
		httpx.SendJSONErrorCode(w, "The server is being installed. Try again when it has finished.", "server_installing", http.StatusConflict)
		return
	}
	loader := strings.ToLower(strings.TrimSpace(req.Loader))
	if loader == "" {
		loader = strings.ToLower(srv.InstallerType)
	}
	if !validate.IsModrinthLoader(loader) {
		httpx.SendJSONError(w, "This server's loader is not one Modrinth knows, so its mods cannot be resolved for another version. Declare the loader first.", http.StatusUnprocessableEntity)
		return
	}
	// spigot, bukkit, purpur and quilt are Modrinth tags with no installer
	// behind them: the node would swap the mods, clear the jars and then fail.
	if !validate.IsInstallerType(loader) {
		httpx.SendJSONErrorCode(w, fmt.Sprintf("%s cannot be installed by the platform, so the server cannot be moved to another version this way. Install the new version through setup instead.", loader),
			"loader_not_installable", http.StatusUnprocessableEntity)
		return
	}
	// A move sends the target Minecraft version as the software version, and a
	// proxy is versioned by its own release: the node stopped the proxy, asked
	// for Velocity 1.21.4 and failed with every player already dropped.
	if services.IsProxyLoader(loader) {
		httpx.SendJSONErrorCode(w, fmt.Sprintf("A %s proxy is versioned by its own release, not by Minecraft's. Reinstall it through setup to change its version.", loader),
			"proxy_not_movable", http.StatusUnprocessableEntity)
		return
	}
	// The update reinstalls the server like a setup does, so it needs the same
	// answer setup gets: with Real player IP on, a target the node cannot
	// configure to expect the header would refuse every player.
	proxyKeep, refused := proxyProtocolInstallGate(w, h.d, srv, loader, req.Minecraft)
	if refused {
		return
	}

	mods, err := h.d.Store.ListServerMods(serverID, srv.ActiveSubServer)
	if err != nil {
		httpx.SendJSONError(w, "Failed to list mods", http.StatusInternalServerError)
		return
	}
	// The update writes every mod's new jar into the loaded directory, so a
	// switched-off one would come back on with its old jar left in .disabled/.
	//
	// A mod still being installed is refused too: its row already names the
	// jar the node has not written yet, so the move would leave the old one on
	// disk beside the target version's. install_mod takes no node_busy, so
	// RefuseIfServerBusy does not see it.
	var off, installing []string
	for _, m := range mods {
		if m.Disabled {
			off = append(off, m.Title+" ("+m.FileName+")")
		}
		if m.Status == models.ServerModInstalling {
			installing = append(installing, m.Title)
		}
	}
	if len(off) > 0 {
		httpx.SendJSONErrorCode(w, fmt.Sprintf("Switch these mods on or remove them first: %s.", strings.Join(off, ", ")),
			"mods_switched_off", http.StatusConflict)
		return
	}
	if len(installing) > 0 {
		httpx.SendJSONErrorCode(w, fmt.Sprintf("These mods are still being installed: %s. Wait until they have finished, then move the server.", strings.Join(installing, ", ")),
			"mods_installing", http.StatusConflict)
		return
	}
	install, unavailable, err := resolveServerModTargets(mods, loader, req.Minecraft)
	if err != nil {
		httpx.SendJSONError(w, "Could not resolve target versions from Modrinth: "+err.Error(), http.StatusBadGateway)
		return
	}
	if len(unavailable) > 0 && !req.DropUnavailable {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":     false,
			"message":     fmt.Sprintf("%d installed mods have no version for Minecraft %s. Set dropUnavailable to update without them.", len(unavailable), req.Minecraft),
			"unavailable": unavailable,
		})
		return
	}

	node, err := h.d.Store.GetNodeByID(srv.NodeID)
	if err != nil {
		httpx.SendJSONError(w, "Node unavailable", http.StatusServiceUnavailable)
		return
	}
	if h.d.Queue == nil {
		httpx.SendJSONError(w, "Queue unavailable", http.StatusServiceUnavailable)
		return
	}

	// Every mod is replaced, including the ones staying on the same version:
	// the node drops the old jar and writes the resolved one, so a same-version
	// resolve is a no-op rewrite rather than a special case.
	//
	// removeFrom names the directory each jar is in now, which is not always
	// the one the move installs into (a Paper server re-declared as Fabric
	// keeps its plugins/ rows). remove stays for a node that predates it.
	remove := make([]string, 0, len(mods))
	removeFrom := make([]map[string]string, 0, len(mods))
	for i := range mods {
		remove = append(remove, mods[i].FileName)
		removeFrom = append(removeFrom, map[string]string{"dir": uninstallTargetDir(&mods[i], srv.InstallerType), "fileName": mods[i].FileName})
	}
	targetDir := modcompat.DefaultTargetDirForLoader(loader)

	if !javaImageAllowed(h.d.Store, req.JavaImage) {
		httpx.SendJSONError(w, javaImageRefused, http.StatusBadRequest)
		return
	}
	javaImage := resolveJavaImage(req.JavaImage, srv.GameImage)
	if javaImage == "" {
		httpx.SendJSONError(w, "javaImage is required", http.StatusBadRequest)
		return
	}

	configPayload := map[string]interface{}{
		"uuid":            srv.UUID,
		"activeSubServer": srv.ActiveSubServer,
		"targetDir":       targetDir,
		"remove":          remove,
		"removeFrom":      removeFrom,
		"install":         install,
		"docker": map[string]interface{}{
			"image":         javaImage,
			"ram":           srv.Memory,
			"ramPaddingMB":  services.EffectiveRAMPaddingMB(h.d.Store, srv.RAMPaddingMB, node.RAMPaddingMB),
			"cpuLimit":      srv.CPULimit,
			"cpusetCpus":    models.EffectiveCpuset(srv.CPUPinningMode, srv.Cpuset, node.CpusetCpus),
			"timezone":      srv.Timezone,
			"extraJvmFlags": strings.TrimSpace(defaultJvmFlags + " " + srv.ExtraJvmFlags),
		},
	}
	installerPayload := map[string]interface{}{
		"type":    loader,
		"version": req.Minecraft,
		"loader":  req.InstallerVersion,
	}
	// keep means the node is asked to write the setting, as reinstall does: a
	// loader change into Paper otherwise starts on Paper's own config, which
	// does not expect the header the gateway keeps sending.
	if proxyKeep {
		installerPayload["proxyProtocol"] = true
	}

	// Recorded before anything is written, as in reinstall.
	if proxyKeep && h.d.Redis != nil {
		if err := services.TrackProxyProtocolInstall(r.Context(), h.d.Redis, srv.UUID, srv.ActiveSubServer, false); err != nil {
			log.Printf("VersionUpdate: server %d: cannot record the real-IP request: %v", serverID, err)
			httpx.SendJSONError(w, "Could not prepare the version update; try again in a moment", http.StatusServiceUnavailable)
			return
		}
	}

	// An undo record, written BEFORE the optimistic writes below.
	//
	// The node stages every jar and verifies it before touching anything, so a
	// download it cannot complete aborts the whole move with the server still
	// running its old version - nothing on disk changes. The writes below have
	// already happened by then, which would leave the DB claiming a Minecraft
	// version and a set of mod versions that are not on the machine. That is
	// not cosmetic: minecraft_version is what the next mod install and the next
	// availability check resolve against, so a wrong one silently installs
	// incompatible jars.
	//
	// So the pre-move state is parked where the node's give-up signal can be
	// paired with it, and services.StatusWatcher puts it back. One hour is far
	// longer than any move takes and short enough that a stale record cannot
	// undo a LATER, successful move.
	prevInstall, ierr := h.d.Store.GetSubServerInstall(serverID, srv.ActiveSubServer)
	if h.d.Redis != nil {
		undo := buildVersionUpdateUndo(srv, mods)
		undo.Install, undo.InstallRecorded = prevInstall, ierr == nil
		if rec, merr := json.Marshal(undo); merr == nil {
			h.d.Redis.Set(r.Context(), services.VersionUpdateUndoKey(srv.UUID), rec, time.Hour)
		}
	}

	// The database is written BEFORE the dispatch, the same ordering reinstall
	// uses: a node that starts the move and a panel that still shows the old
	// version would disagree about what is running, and the version is what the
	// next mod install resolves against.
	//
	// minecraft_version gets the EXACT target (1.21.4), not a version line, because
	// that column is what the Content tab filters Modrinth by and what the next
	// availability check reads as "current". build_number gets the loader build,
	// matching the column name and what an orphan import writes into it.
	if err := h.d.Store.UpdateServerSetup(serverID, javaImage, "", srv.ActiveSubServer, srv.ExtraJvmFlags, loader, req.Minecraft, req.InstallerVersion); err != nil {
		httpx.SendJSONError(w, "Failed to update the server record", http.StatusInternalServerError)
		return
	}
	h.d.Store.UpdateServerStatus(serverID, "installing")

	if err := h.d.Queue.SendCommand(context.Background(), node.Token, "update_server_version", configPayload, installerPayload); err != nil {
		// No node got the command, so none will report a failure for the
		// status watcher to undo: put the columns back here, or the server
		// claims the target version and sits "installing" for good.
		_ = h.d.Store.UpdateServerSetup(serverID, srv.GameImage, srv.StartCommand, srv.ActiveSubServer, srv.ExtraJvmFlags, srv.InstallerType, srv.MinecraftVersion, srv.BuildNumber)
		h.d.Store.UpdateServerStatus(serverID, srv.Status)
		if h.d.Redis != nil {
			h.d.Redis.Del(context.Background(), services.VersionUpdateUndoKey(srv.UUID))
			if proxyKeep {
				services.ForgetProxyProtocolInstall(context.WithoutCancel(r.Context()), h.d.Redis, srv.UUID)
			}
		}
		httpx.SendJSONError(w, "Failed to queue the version update", http.StatusInternalServerError)
		return
	}

	// The mod rows follow the jars: the dropped ones are gone from disk, and
	// the kept ones now point at a different version and file name.
	dropped := map[int]bool{}
	for _, u := range unavailable {
		dropped[u.ModID] = true
	}
	for _, m := range mods {
		if dropped[m.ID] {
			_ = h.d.Store.DeleteServerMod(m.ID, serverID)
		}
	}
	for _, in := range install {
		row := movedModRow(in, targetDir)
		if _, err := h.d.Store.UpsertServerMod(&row); err != nil {
			// The jar is on its way regardless; a stale row is visible and
			// fixable, so this is logged through the response rather than
			// failing a move that is already dispatched.
			continue
		}
	}

	// The install record follows the move, as it follows a reinstall: a switch
	// back to this sub-server takes its Minecraft version from there, and it
	// kept naming the version this move replaced. The pack reference stays,
	// since the move keeps the pack's files. Not written when the old record
	// could not be read, or the pack fields would be lost.
	if ierr == nil {
		next := models.SubServerInstall{}
		if prevInstall != nil {
			next = *prevInstall
		}
		next.ServerID, next.SubServerName = serverID, srv.ActiveSubServer
		next.InstallerType, next.McVersion = loader, req.Minecraft
		next.BuildVersion, next.Loader = req.InstallerVersion, req.InstallerVersion
		if err := h.d.Store.UpsertSubServerInstall(next); err != nil {
			log.Printf("VersionUpdate: could not update the install record for server %d/%s: %v", serverID, srv.ActiveSubServer, err)
		}
	}

	h.d.Events.Publish(r.Context(), "server_mods.changed", map[string]interface{}{"serverId": serverID})
	h.d.Events.Publish(r.Context(), "servers.changed", nil)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"message":   fmt.Sprintf("Moving to Minecraft %s. %d mods carried over, %d removed.", req.Minecraft, len(install), len(unavailable)),
		"installed": len(install),
		"removed":   unavailable,
	})
}

// movedModRow is a mod's row as the move leaves it. The install id and the
// previous jar belong to the install before the move: kept, a late report about
// that attempt still matched the row and put the pre-move file name back.
func movedModRow(in serverModTarget, targetDir string) models.ServerMod {
	row := in.mod
	row.ModrinthVersionID = in.VersionID
	row.FileName = in.FileName
	row.SHA512 = in.SHA512
	row.TargetDir = targetDir
	row.InstallID = ""
	row.PreviousFileName, row.PreviousTargetDir, row.PreviousSHA512 = "", "", ""
	return row
}

// refuseUntilCopyLanded holds a move back while a copy of the server is queued
// or running, and refuses one that was asked to follow a copy that did not
// land. The panel's "copy first" sent the move the moment the copy was QUEUED:
// a running copy answered it with a generic busy 409, and a copy still waiting
// in the stream let both run on one tree, leaving the rollback copy a mix of
// old and new software. copy_running is the answer to wait on and ask again.
func (h *ServerModsHandler) refuseUntilCopyLanded(w http.ResponseWriter, r *http.Request, srv *models.Server, afterCopy string) bool {
	if h.d.Redis == nil {
		return false
	}
	ctx := r.Context()
	pending, _ := h.d.Redis.Get(ctx, services.CopyPendingKey(srv.UUID)).Result()
	if pending == "" && afterCopy == "" {
		return false
	}
	verdict := ""
	if busy, _ := h.d.Redis.Get(ctx, services.NodeBusyKey(srv.UUID)).Result(); busy != "" {
		verdict = "copy_running"
	} else {
		entries, err := guard.ListServerRootNames(h.d.GRPCRegistry, srv.NodeID, srv.UUID)
		if err != nil {
			httpx.SendJSONError(w, "The node did not answer, so it is not known whether the copy has finished: "+err.Error(), http.StatusBadGateway)
			return true
		}
		verdict = copyBeforeMoveVerdict(pending, afterCopy, entries)
		if verdict == "" && pending != "" {
			h.d.Redis.Del(ctx, services.CopyPendingKey(srv.UUID))
		}
	}
	switch verdict {
	case "copy_running":
		httpx.SendJSONErrorCode(w, "A copy of this server is still being made. The move starts once it has finished.", verdict, http.StatusConflict)
		return true
	case "copy_failed":
		httpx.SendJSONErrorCode(w, "The copy to "+afterCopy+" did not finish, so the move was not started.", verdict, http.StatusConflict)
		return true
	}
	return false
}

// copyBeforeMoveVerdict is refuseUntilCopyLanded's decision once the node is
// known not to be working on the server: "" lets the move go, copy_running
// means a queued copy has not landed yet, copy_failed means the copy the move
// was to follow is neither queued nor there. A copy is in place once its
// directory exists: the node builds it under a staging name and renames it in
// as its last step.
func copyBeforeMoveVerdict(pending, afterCopy string, entries []*pb.FileInfo) string {
	if pending != "" && !subServerNameTaken(entries, pending) {
		return "copy_running"
	}
	if afterCopy != "" && !subServerNameTaken(entries, afterCopy) {
		return "copy_failed"
	}
	return ""
}

// duplicateOfInstalled reports whether identifying fileName as projectID would
// collide with a mod this sub-server already has, and under which name.
//
// server_mods conflicts on (server_id, sub_server_name, modrinth_project_id), so
// an upsert here does not add a row - it REWRITES the existing one onto the new
// file name. Identifying a stray copy of an installed mod therefore pointed the
// managed row at the stray, the real jar read as unmanaged, and the next version
// move removed the stray while leaving the real one behind: two copies of one
// mod and a server that will not start.
//
// Re-identifying the SAME file is not a duplicate. It refreshes the row, which
// is what someone repairing a broken link is asking for.
func duplicateOfInstalled(claimedBy map[string]string, projectID, fileName string) (string, bool) {
	other, taken := claimedBy[projectID]
	if !taken || other == fileName {
		return "", false
	}
	return other, true
}

// buildVersionUpdateUndo captures everything the optimistic writes are about to
// overwrite. Only the columns a version move touches: this is a compensating
// write for one command, not a snapshot of the server.
func buildVersionUpdateUndo(srv *models.Server, mods []models.ServerMod) services.VersionUpdateUndo {
	undo := services.VersionUpdateUndo{
		InstallerType:    srv.InstallerType,
		MinecraftVersion: srv.MinecraftVersion,
		BuildNumber:      srv.BuildNumber,
		SubServerName:    srv.ActiveSubServer,
		GameImage:        srv.GameImage,
		Mods:             make([]services.VersionUpdateUndoMod, 0, len(mods)),
	}
	for _, m := range mods {
		undo.Mods = append(undo.Mods, services.VersionUpdateUndoMod{
			ModrinthProjectID:   m.ModrinthProjectID,
			ModrinthProjectSlug: m.ModrinthProjectSlug,
			ModrinthVersionID:   m.ModrinthVersionID,
			Title:               m.Title,
			FileName:            m.FileName,
			SHA512:              m.SHA512,
			TargetDir:           m.TargetDir,
		})
	}
	return undo
}

// serverModTarget is one mod's resolved jar for the target version.
type serverModTarget struct {
	FileName    string `json:"fileName"`
	DownloadURL string `json:"downloadUrl"`
	SHA512      string `json:"sha512"`
	VersionID   string `json:"-"`
	mod         models.ServerMod
}

type unavailableMod struct {
	ModID   int    `json:"modId"`
	Title   string `json:"title"`
	Slug    string `json:"slug"`
	Current string `json:"currentVersionId,omitempty"`
}

// resolveServerModTargets finds each installed mod's version for the target
// Minecraft version.
//
// The batch hash endpoint takes the stored sha512 (one request per 100 mods).
// A mod whose stored hash Modrinth cannot place falls back to the project's own
// version list; if that finds nothing either, the mod is genuinely not
// available on the target.
func resolveServerModTargets(mods []models.ServerMod, loader, targetMC string) ([]serverModTarget, []unavailableMod, error) {
	hashes := make([]string, 0, len(mods))
	for _, m := range mods {
		if m.SHA512 != "" {
			hashes = append(hashes, m.SHA512)
		}
	}
	byHash := map[string]services.ModrinthVersion{}
	for start := 0; start < len(hashes); start += 100 {
		end := start + 100
		if end > len(hashes) {
			end = len(hashes)
		}
		res, err := services.CheckLatestVersions(hashes[start:end], "sha512", []string{loader}, []string{targetMC})
		if err != nil {
			return nil, nil, err
		}
		for k, v := range res {
			byHash[k] = v
		}
	}

	install := []serverModTarget{}
	unavailable := []unavailableMod{}
	for _, m := range mods {
		v, ok := byHash[m.SHA512]
		if !ok || v.ID == "" {
			if m.ModrinthProjectID != "" {
				if fallback, err := services.LatestProjectVersionFor(m.ModrinthProjectID, targetMC, loader); err == nil && fallback != nil {
					v, ok = *fallback, true
				}
			}
		}
		file := v.PrimaryFile()
		if !ok || v.ID == "" || file.URL == "" {
			unavailable = append(unavailable, unavailableMod{
				ModID: m.ID, Title: m.Title, Slug: m.ModrinthProjectSlug, Current: m.ModrinthVersionID,
			})
			continue
		}
		install = append(install, serverModTarget{
			FileName:    file.Filename,
			DownloadURL: file.URL,
			SHA512:      file.Hashes["sha512"],
			VersionID:   v.ID,
			mod:         m,
		})
	}
	return install, unavailable, nil
}

type copySubServerRequest struct {
	TargetName string `json:"targetName"`
}

// copiedModRow is a source row as the copy's own, with the outcome settled: no
// report will ever name the copy's sub-server, so an attempt id or previous_*
// carried over could only be applied to the wrong row or never.
func copiedModRow(m models.ServerMod, target string) models.ServerMod {
	m.ID = 0
	m.SubServerName = target
	m.InstallID = ""
	m.PreviousFileName, m.PreviousTargetDir, m.PreviousSHA512 = "", "", ""
	if m.Status == models.ServerModInstalling {
		// Only a stale one gets here; unknown counts as installed, as for an old node.
		m.Status = models.ServerModInstalled
	}
	return m
}

// CopySubServer POST /api/servers/{id}/copy-sub-server
//
// Duplicates the active sub-server so a version move can be tried on the copy
// while the original stays as it is. The server has to be stopped: a running
// server's world files are being written while the copy walks them.
func (h *ServerModsHandler) CopySubServer(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	srv, ok := h.getServer(serverID)
	if !ok {
		httpx.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	if guard.RefuseIfSuspended(w, r, h.d.Store, h.d.SuspendGrace, srv) {
		return
	}
	if guard.RefuseIfServerBusy(w, r, h.d.Redis, h.d.GRPCRegistry, srv) {
		return
	}
	var req copySubServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.TargetName = strings.TrimSpace(req.TargetName)
	if !validate.IsSubServerName(req.TargetName) {
		httpx.SendJSONError(w, "targetName must be a single plain directory name", http.StatusBadRequest)
		return
	}
	if srv.ActiveSubServer == "" {
		httpx.SendJSONError(w, "No active sub-server to copy", http.StatusBadRequest)
		return
	}
	if req.TargetName == srv.ActiveSubServer {
		httpx.SendJSONError(w, "The copy needs a different name from the sub-server it copies", http.StatusBadRequest)
		return
	}
	if srv.Status == "online" || srv.Status == "installing" {
		httpx.SendJSONError(w, "Stop the server before copying it: a running server's world files are copied mid-write", http.StatusConflict)
		return
	}
	// Booting, restarting or still saving on the way down: the world files are
	// being written just the same, and a boot now reads "starting" for minutes.
	if inPowerTransition(srv.Status) {
		httpx.SendJSONError(w, "The server is "+srv.Status+". Copy it once it has stopped: its world files are being written", http.StatusConflict)
		return
	}
	// A row still installing would be cloned with its attempt id, and the node
	// reports for the SOURCE only, so the copy's row would stay "installing"
	// for good and its version move would be refused every time.
	mods, _ := h.d.Store.ListServerMods(serverID, srv.ActiveSubServer)
	for i := range mods {
		if modInstallInFlight(&mods[i], time.Now()) {
			httpx.SendJSONErrorCode(w, "A mod is still being installed: "+mods[i].Title+". Copy the server when it has finished.",
				"mods_installing", http.StatusConflict)
			return
		}
	}

	node, err := h.d.Store.GetNodeByID(srv.NodeID)
	if err != nil {
		httpx.SendJSONError(w, "Node unavailable", http.StatusServiceUnavailable)
		return
	}
	if h.d.Queue == nil {
		httpx.SendJSONError(w, "Queue unavailable", http.StatusServiceUnavailable)
		return
	}

	// The node refuses to copy onto an existing directory, which protects the
	// FILES. It does not protect the mod inventory: the rows below are written
	// whatever the node decides, so without this check copying onto the name of
	// an existing sub-server would merge this one's mods into that one's list
	// while the copy itself never happened - and the response would still say
	// the copy was queued.
	//
	// A node that cannot be reached is a refusal rather than an assumption. The
	// whole point of the check is that the answer is not knowable from here.
	entries, lerr := guard.ListServerRootNames(h.d.GRPCRegistry, srv.NodeID, srv.UUID)
	if lerr != nil {
		httpx.SendJSONError(w, "The node did not answer, so it is not known whether that name is already taken: "+lerr.Error(), http.StatusBadGateway)
		return
	}
	// A copy CREATES a sub-server, so it is held to the same cap as creating one
	// through setup. It never was, which is half of how a platform limited to
	// three ended up with seven. The listing is already in hand, so this costs
	// nothing extra.
	if guard.RefuseIfOverSubServerLimit(w, guard.SubServerLimit(h.d.Store), guard.CountSubServerDirs(entries)) {
		return
	}
	if subServerNameTaken(entries, req.TargetName) {
		httpx.SendJSONError(w, "A sub-server called "+req.TargetName+" already exists. Pick another name.", http.StatusConflict)
		return
	}

	// Before the dispatch, so a version move can never see the copy as neither
	// queued nor running (refuseUntilCopyLanded).
	if h.d.Redis != nil {
		h.d.Redis.Set(r.Context(), services.CopyPendingKey(srv.UUID), req.TargetName, services.CopyPendingTTL)
	}
	if err := h.d.Queue.SendCommand(context.Background(), node.Token, "copy_sub_server", map[string]interface{}{
		"uuid":            srv.UUID,
		"sourceSubServer": srv.ActiveSubServer,
		"targetSubServer": req.TargetName,
	}, nil); err != nil {
		if h.d.Redis != nil {
			h.d.Redis.Del(r.Context(), services.CopyPendingKey(srv.UUID))
		}
		httpx.SendJSONError(w, "Failed to queue the copy", http.StatusInternalServerError)
		return
	}

	// The mod inventory is per sub-server, so the copy needs its own rows or it
	// would look like a server with no mods installed while the jars are right
	// there on disk.
	for _, m := range mods {
		copied := copiedModRow(m, req.TargetName)
		_, _ = h.d.Store.UpsertServerMod(&copied)
	}

	// Same argument as the mods above, for how it was INSTALLED. Without this the
	// copy has no record, so the setup screen falls back to the servers row -
	// which describes whichever sub-server is active, not this one - and every
	// save on the copy is classified as an installer change, putting the "what
	// should I clear" dialog in front of an operator who only wanted to edit a
	// JVM flag.
	if rec, err := h.d.Store.GetSubServerInstall(serverID, srv.ActiveSubServer); err == nil && rec != nil {
		copied := *rec
		copied.SubServerName = req.TargetName
		if err := h.d.Store.UpsertSubServerInstall(copied); err != nil {
			log.Printf("CopySubServer: could not copy the install record for server %d/%s: %v",
				serverID, req.TargetName, err)
		}
	}
	if scan, jars, _, err := h.d.Store.GetModpackScan(serverID, srv.ActiveSubServer); err == nil && scan != "" {
		if err := h.d.Store.SetModpackScan(serverID, req.TargetName, scan, jars); err != nil {
			log.Printf("CopySubServer: could not copy the modpack scan state for server %d/%s: %v",
				serverID, req.TargetName, err)
		}
	}

	// And the modpack snapshot, for the same reason again: it backs the Content
	// tab's cross-check, so a copy without it shows "no modpack members" next to
	// a directory full of the pack's jars.
	if contents, cerr := h.d.Store.ListServerModpackContents(serverID, srv.ActiveSubServer); cerr == nil && len(contents) > 0 {
		for i := range contents {
			contents[i].SubServerName = req.TargetName
		}
		if err := h.d.Store.ReplaceServerModpackContents(serverID, req.TargetName, contents); err != nil {
			log.Printf("CopySubServer: could not copy the modpack snapshot for server %d/%s: %v",
				serverID, req.TargetName, err)
		}
	}

	h.d.Events.Publish(r.Context(), "servers.changed", nil)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Copy queued. It appears as a sub-server once the files are in place.",
	})
}

// subServerNameTaken reports whether a server directory already holds an entry
// by that name.
//
// Case-INSENSITIVE, and deliberately stricter than the filesystem underneath:
// on Linux "Server" and "server" are two directories the node would happily
// create, and two sub-servers whose names differ only in case is a trap for
// every screen that lists them. Refusing the near-collision costs an operator
// one rename; allowing it costs everyone reading the list afterwards.
//
// Pure, so the rule can be exercised without a node on the other end - which is
// the whole reason the check exists: what is on that disk is not knowable here.
func subServerNameTaken(entries []*pb.FileInfo, name string) bool {
	return guard.SubServerNameTaken(entries, name)
}

// --- node round trips ------------------------------------------------------

// listServerDir is the shared implementation; the sub-server COUNT has to be
// read the same way here and in SetupServer, which belongs to another type.
func (h *ServerModsHandler) listServerDir(nodeID int, serverUUID, dir string) ([]*pb.FileInfo, error) {
	return guard.ListServerDirFor(h.d.GRPCRegistry, nodeID, serverUUID, dir)
}

// hashFilesBatch must not exceed the node's own hashFilesMaxCount (200 in
// node/grpc_hash_files.go). Core cannot import that constant, so it is repeated
// here rather than left to be discovered: without the batching, identifying the
// jars of an imported server - which is the whole reason this endpoint exists,
// and routinely means a few hundred files - was refused wholesale by the node
// with "too many files in one request".
const hashFilesBatch = 200

func (h *ServerModsHandler) hashServerFiles(nodeID int, serverUUID, dir string, names []string) ([]*pb.FileHash, error) {
	if len(names) > hashFilesBatch {
		out := make([]*pb.FileHash, 0, len(names))
		for start := 0; start < len(names); start += hashFilesBatch {
			end := start + hashFilesBatch
			if end > len(names) {
				end = len(names)
			}
			part, err := h.hashServerFiles(nodeID, serverUUID, dir, names[start:end])
			if err != nil {
				return nil, err
			}
			out = append(out, part...)
		}
		return out, nil
	}
	if h.d.GRPCRegistry == nil {
		return nil, fmt.Errorf("node connection not available")
	}
	resp, err := h.d.GRPCRegistry.SendRequest(nodeID, &pb.NodeMessage{
		RequestId:  uuid.NewString(),
		ServerUuid: serverUUID,
		Payload:    &pb.NodeMessage_HashFilesReq{HashFilesReq: &pb.HashFilesReq{Path: dir, Names: names}},
	}, 120*time.Second)
	if err != nil {
		return nil, err
	}
	if e := resp.GetError(); e != nil {
		return nil, fmt.Errorf("%s", e.Message)
	}
	out := resp.GetHashFilesResp()
	if out == nil {
		return nil, fmt.Errorf("unexpected response from node")
	}
	return out.Files, nil
}
