package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
	"dylaris-pkg/validate"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

// Install / list / uninstall Modrinth-sourced mods per server.
// We dispatch download work to the node via the existing Queue (mailbox)
// pipeline so the node-side worker can lift its own implementation when
// it picks up the command — no new proto round trip needed for V1.

type ServerModsHandler struct {
	state *AppState
	// scanDeps replaces the node and Modrinth round trips of the modpack scan
	// in tests; nil means the real ones.
	scanDeps func(srv *models.Server) modpackScanDeps
}

func NewServerModsHandler(state *AppState) *ServerModsHandler {
	return &ServerModsHandler{state: state}
}

type installModRequest struct {
	ProjectID   string `json:"projectId"`
	ProjectSlug string `json:"projectSlug"`
	VersionID   string `json:"versionId"`
	Title       string `json:"title"`
	FileName    string `json:"fileName"`
	DownloadURL string `json:"downloadUrl"`
	SHA512      string `json:"sha512"`
	TargetDir   string `json:"targetDir"` // "mods" or "plugins" — derived from loader if empty
}

// modrinthAllowedHosts pins where we'll dispatch downloads from. Modrinth
// only serves files from cdn.modrinth.com — anything else in a "downloadUrl"
// field would be a maliciously-mutated payload.
var modrinthAllowedHosts = map[string]bool{
	"cdn.modrinth.com": true,
}

// getServer fetches the server the route's RequireCap already gated access
// to. The pure authorization check used to live here (checkServerAccess); the
// router's mods.read/mods.write/mods.delete RequireCap now enforces it before
// this handler ever runs.
func (h *ServerModsHandler) getServer(serverID int) (*models.Server, bool) {
	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		return nil, false
	}
	return srv, true
}

// List GET /api/servers/{id}/mods - the mods installed on the server's ACTIVE
// sub-server, not on every sub-server it has.
func (h *ServerModsHandler) List(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	srv, ok := h.getServer(serverID)
	if !ok {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	mods, err := h.state.Store.ListServerMods(serverID, srv.ActiveSubServer)
	if err != nil {
		sendJSONError(w, "Failed to list mods", http.StatusInternalServerError)
		return
	}
	h.fillProjectMeta(r.Context(), mods)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"mods":    mods,
	})
}

// fetchModProjects is a variable so tests can stand in for Modrinth.
var fetchModProjects = services.FetchProjects

// fetchInstallVersion is a variable so tests can stand in for Modrinth.
var fetchInstallVersion = services.FetchModrinthVersionPaced

// modMetaWait bounds the lookup: it sits on the mod list, which used to be a
// database read, and a slow Modrinth must not hold the Content tab open.
var modMetaWait = 3 * time.Second

// fillProjectMeta gives rows without a slug the project's title and slug, and
// writes them back. Identifying a jar by its hash only learns the VERSION, so
// those rows carried the version name as their title and no slug at all, and
// every update re-sent both. A failed lookup leaves the rows as they are.
//
// ponytail: a project Modrinth no longer has is asked for on every listing; a
// "looked up, gone" marker if that ever shows in the rate budget.
func (h *ServerModsHandler) fillProjectMeta(ctx context.Context, mods []models.ServerMod) {
	var ids []string
	for _, m := range mods {
		if m.ModrinthProjectID != "" && m.ModrinthProjectSlug == "" {
			ids = append(ids, m.ModrinthProjectID)
		}
	}
	if len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, modMetaWait)
	defer cancel()
	meta, err := fetchModProjects(ctx, ids)
	if err != nil {
		log.Printf("[mods] project titles for %d identified mods: %v", len(ids), err)
		return
	}
	for i := range mods {
		p, ok := meta[mods[i].ModrinthProjectID]
		if !ok || p.Slug == "" || mods[i].ModrinthProjectSlug != "" {
			continue
		}
		if err := h.state.Store.SetServerModProjectMeta(mods[i].ID, p.Slug, p.Title); err != nil {
			log.Printf("[mods] storing the project title of mod %d: %v", mods[i].ID, err)
			continue
		}
		mods[i].ModrinthProjectSlug, mods[i].Title = p.Slug, p.Title
	}
}

// ModpackContents returns the modpack snapshot for the active sub-server: the
// Modrinth-identified members of the pack this server was installed from. Empty
// when the server is not a modpack server. Same mods.read gate as the mods
// endpoints. Backs the panel's advisory cross-check + modpack banner.
//
// isModpack is separate from the contents because a pack can have members
// Modrinth does not know (a Technic pack of private builds), and the panel
// should still warn before one of them is removed.
func (h *ServerModsHandler) ModpackContents(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	srv, ok := h.getServer(serverID)
	if !ok {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	// The scan is advisory: a failure to read its state just means no scan.
	scan, jars, _, scanErr := h.state.Store.GetModpackScan(serverID, srv.ActiveSubServer)
	if scanErr == nil && scan == modpackScanPending && h.maybeScanModpack(srv) {
		_, jars, _, scanErr = h.state.Store.GetModpackScan(serverID, srv.ActiveSubServer)
	}
	if scanErr != nil {
		jars = 0
	}
	contents, err := h.state.Store.ListServerModpackContents(serverID, srv.ActiveSubServer)
	if err != nil {
		sendJSONError(w, "Failed to list modpack contents", http.StatusInternalServerError)
		return
	}
	installerType := ""
	if rec, err := h.state.Store.GetSubServerInstall(serverID, srv.ActiveSubServer); err == nil && rec != nil {
		installerType = rec.InstallerType
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"contents":  contents,
		"isModpack": isModpackServer(len(contents), installerType, jars),
	})
}

// Install POST /api/servers/{id}/mods - queues a mod install onto the active
// sub-server. The reply says queued, and the mod only takes effect after a
// restart.
func (h *ServerModsHandler) Install(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	srv, ok := h.getServer(serverID)
	if !ok {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	if refuseIfSuspended(w, r, h.state, srv) {
		return
	}
	var req installModRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.ProjectID = strings.TrimSpace(req.ProjectID)
	req.VersionID = strings.TrimSpace(req.VersionID)
	req.DownloadURL = strings.TrimSpace(req.DownloadURL)
	req.FileName = strings.TrimSpace(req.FileName)
	if req.ProjectID == "" || req.VersionID == "" || req.DownloadURL == "" || req.FileName == "" {
		sendJSONError(w, "projectId, versionId, downloadUrl, fileName required", http.StatusBadRequest)
		return
	}
	// A server that knows its loader and Minecraft version only takes a build
	// made for both, and the file comes from Modrinth, not the request: a Forge
	// jar on a NeoForge server, or a 26.3 build on 1.21.1, installs fine and
	// then fails at boot. An imported server that has declared neither keeps
	// the unchecked install.
	if modFitKnown(srv.InstallerType, srv.MinecraftVersion) {
		ctx, cancel := context.WithTimeout(r.Context(), modPlanWait)
		v, err := fetchInstallVersion(ctx, req.VersionID)
		cancel()
		if err != nil {
			if services.ModrinthNotFound(err) {
				sendJSONError(w, "Modrinth has no version "+req.VersionID, http.StatusBadRequest)
				return
			}
			var upstream *services.ModrinthHTTPError
			if errors.As(err, &upstream) && upstream.Status == http.StatusTooManyRequests {
				sendCooldown(w, "Modrinth", upstreamCooldownDefault)
				return
			}
			log.Printf("install mod: fetching version %s from Modrinth: %v", req.VersionID, err)
			sendJSONError(w, "Could not reach Modrinth to check this build against the server. Try again in a moment.", http.StatusBadGateway)
			return
		}
		if v.ProjectID != req.ProjectID {
			sendJSONError(w, "versionId does not belong to projectId", http.StatusBadRequest)
			return
		}
		if ok, reason := modVersionFits(srv.InstallerType, srv.MinecraftVersion, v.Loaders, v.GameVersions); !ok {
			sendJSONError(w, reason, http.StatusConflict)
			return
		}
		f := v.PrimaryFile()
		req.FileName, req.DownloadURL, req.SHA512 = f.Filename, f.URL, f.Hashes["sha512"]
	}
	// The node reduces this to its basename and refuses what is left over, which
	// is why nothing ever escaped the mods directory. What it could not do is
	// tell the caller: Core queued whatever arrived and answered 200, so
	// "../../../escape.jar" was accepted, written as "escape.jar", and recorded
	// under a name that is not the file on disk - and uninstall aims at the
	// recorded one. Refusing here makes the 200 mean what it says.
	if !validate.IsPlainFileName(req.FileName) {
		sendJSONError(w, "fileName must be a plain file name, without any path", http.StatusBadRequest)
		return
	}
	// Trust-but-verify: the panel sends a downloadUrl it pulled from Modrinth,
	// but a tampered POST could send anything. Enforce that the host is a
	// known Modrinth CDN.
	if !isAllowedModrinthURL(req.DownloadURL) {
		sendJSONError(w, "downloadUrl must point to cdn.modrinth.com", http.StatusBadRequest)
		return
	}
	if srv.ActiveSubServer == "" {
		sendJSONError(w, "No active sub-server — set one up first", http.StatusBadRequest)
		return
	}
	targetDir := req.TargetDir
	if targetDir == "" {
		targetDir = defaultTargetDirForLoader(srv.InstallerType)
	}
	if targetDir != "mods" && targetDir != "plugins" {
		sendJSONError(w, "targetDir must be 'mods' or 'plugins'", http.StatusBadRequest)
		return
	}
	// Filename sanitation: no slashes, no upward traversal.
	cleanName := path.Base(req.FileName)
	if cleanName == "" || cleanName == "." || cleanName == ".." || strings.ContainsAny(cleanName, "\\/") {
		sendJSONError(w, "invalid fileName", http.StatusBadRequest)
		return
	}

	node, err := h.state.Store.GetNodeByID(srv.NodeID)
	if err != nil {
		sendJSONError(w, "Node unavailable", http.StatusServiceUnavailable)
		return
	}
	if h.state.Queue == nil {
		sendJSONError(w, "Queue unavailable", http.StatusServiceUnavailable)
		return
	}

	// What this install REPLACES, read BEFORE the upsert overwrites it.
	//
	// The row is keyed on the project, so updating a mod rewrote file_name onto
	// the new jar's name and the old one became unnameable - which is how a
	// server came to carry both builds with only the newer one removable. The
	// node needs the old name to delete it, and the history table needs the
	// whole row to offer a way back.
	//
	// A lookup failure is not fatal to the install: the worst case is the old
	// jar surviving, which is exactly today's behaviour, and refusing the whole
	// install over it would be a worse trade.
	prev, prevErr := h.state.Store.GetServerModByProject(serverID, srv.ActiveSubServer, req.ProjectID)
	if prevErr != nil {
		log.Printf("install mod: reading the previous row for project %s failed, the old jar may survive: %v",
			req.ProjectID, prevErr)
		prev = nil
	}
	previousFileName := ""
	if prev != nil {
		previousFileName = prev.FileName
	}

	// One attempt, one id. The node echoes it back and Core only accepts a
	// report that still names the attempt the row holds, so a slow answer about
	// a superseded install cannot overwrite the state of the one that replaced
	// it - two clicks in a row is all that takes.
	installID := uuid.NewString()

	userID, _ := r.Context().Value("userID").(string)
	var installedBy *string
	if userID != "" {
		v := userID
		installedBy = &v
	}
	mod := &models.ServerMod{
		ServerID:            serverID,
		SubServerName:       srv.ActiveSubServer,
		ModrinthProjectID:   req.ProjectID,
		ModrinthProjectSlug: req.ProjectSlug,
		ModrinthVersionID:   req.VersionID,
		Title:               req.Title,
		FileName:            cleanName,
		// The directory the node is told to write into. Uninstall reads it
		// back rather than recomputing, so a later loader change cannot point the
		// removal at a directory the jar was never in.
		TargetDir:   targetDir,
		SHA512:      req.SHA512,
		InstalledBy: installedBy,
		// Not "installed": nothing has been installed yet. The node reports the
		// outcome on its own channel and Core moves the row from here. A row
		// used to be written as a fact before dispatch and stayed one whatever
		// the node found, so a 404 and a hash mismatch both read as success.
		// installing only when the node is new enough to answer; see
		// installStatusFor for why an old one is recorded the way it always was.
		Status:    installStatusFor(r.Context(), h.state, node.Token),
		InstallID: installID,
	}
	// Filed BEFORE the upsert, which is the write that destroys it. A history
	// failure is logged rather than fatal: losing the way back is smaller than
	// losing the install.
	//
	// Only when the version actually changes. Re-installing the same build is a
	// repair, not a step worth being able to undo.
	if prev != nil && prev.ModrinthVersionID != req.VersionID {
		if err := h.state.Store.RecordServerModHistory(serverID, srv.ActiveSubServer, prev); err != nil {
			log.Printf("install mod: recording history for project %s failed, rollback will not offer this version: %v",
				req.ProjectID, err)
		}
	}
	// The row, with this attempt's id, is written BEFORE the node is told: the
	// result handler only applies a report naming the attempt the row holds, and
	// a node that answered before the upsert landed had its report dropped,
	// leaving the row "installing" for good.
	if _, err := h.state.Store.UpsertServerMod(mod); err != nil {
		sendJSONError(w, "Could not record the install: "+err.Error(), http.StatusInternalServerError)
		return
	}

	configPayload := map[string]interface{}{
		"uuid":             srv.UUID,
		"activeSubServer":  srv.ActiveSubServer,
		"targetDir":        targetDir,
		"fileName":         cleanName,
		"downloadUrl":      req.DownloadURL,
		"sha512":           req.SHA512,
		"previousFileName": previousFileName,
		"installId":        installID,
		"serverId":         serverID,
		"projectId":        req.ProjectID,
	}
	if err := h.state.Queue.SendCommand(context.Background(), node.Token, "install_mod", configPayload, nil); err != nil {
		// The row exists now and names an install the node never received. Failed
		// is what it is, and the panel already says "the server is still running
		// whatever it had before" for a failed row.
		if _, serr := h.state.Store.SetServerModStatus(serverID, srv.ActiveSubServer, req.ProjectID, installID,
			models.ServerModFailed, "Core could not queue the install for the node."); serr != nil {
			log.Printf("install mod: marking the undispatched install of %s failed: %v", req.ProjectID, serr)
		}
		h.state.Events.Publish(r.Context(), "server_mods.changed", map[string]interface{}{"serverId": serverID})
		sendJSONError(w, "Failed to queue install", http.StatusInternalServerError)
		return
	}

	h.state.Events.Publish(r.Context(), "server_mods.changed", map[string]interface{}{
		"serverId": serverID,
	})

	// status tells the panel whether to wait for the node's answer: "installing"
	// means a report will arrive, "installed" means this node is too old to send
	// one and there is nothing to wait for.
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Mod install queued. Restart the server to apply.",
		"status":  mod.Status,
	})
}

// ModHistory GET /api/servers/{id}/mods/history - the superseded versions of
// this sub-server's mods, newest first, so a bad update can be undone.
func (h *ServerModsHandler) ModHistory(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	srv, ok := h.getServer(serverID)
	if !ok {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	entries, err := h.state.Store.ListServerModHistory(serverID, srv.ActiveSubServer)
	if err != nil {
		sendJSONError(w, "Could not read the mod history", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "history": entries})
}

// SetUpdatesPaused PUT /api/servers/{id}/mods/{modId}/updates-paused - keeps
// one mod out of "Update all", or lets it back in. Body {"paused": bool}.
func (h *ServerModsHandler) SetUpdatesPaused(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	if _, ok := h.getServer(serverID); !ok {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	var req struct {
		Paused *bool `json:"paused"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req); err != nil || req.Paused == nil {
		sendJSONError(w, "paused (true or false) is required", http.StatusBadRequest)
		return
	}
	modID, _ := strconv.Atoi(mux.Vars(r)["modId"])
	if err := h.state.Store.SetServerModUpdatesPaused(modID, serverID, *req.Paused); err != nil {
		if errors.Is(err, store.ErrServerModNotFound) {
			sendJSONError(w, "Mod not found", http.StatusNotFound)
			return
		}
		sendJSONError(w, "Could not save the setting", http.StatusInternalServerError)
		return
	}
	h.state.Events.Publish(r.Context(), "server_mods.changed", map[string]interface{}{
		"serverId": serverID,
	})
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// Uninstall DELETE /api/servers/{id}/mods/{modId} - queues removal of one mod
// from the active sub-server.
func (h *ServerModsHandler) Uninstall(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	srv, ok := h.getServer(serverID)
	if !ok {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	if refuseIfSuspended(w, r, h.state, srv) {
		return
	}
	modID, _ := strconv.Atoi(mux.Vars(r)["modId"])
	mods, _ := h.state.Store.ListServerMods(serverID, srv.ActiveSubServer)
	var target *models.ServerMod
	for i := range mods {
		if mods[i].ID == modID {
			target = &mods[i]
			break
		}
	}
	if target == nil {
		sendJSONError(w, "Mod not found", http.StatusNotFound)
		return
	}

	node, err := h.state.Store.GetNodeByID(srv.NodeID)
	if err != nil {
		sendJSONError(w, "Node unavailable", http.StatusServiceUnavailable)
		return
	}
	targetDir := uninstallTargetDir(target, srv.InstallerType)
	// The DB row is the only handle the panel has on this jar, so it must not be
	// dropped until the node has actually been told to delete the file. Install
	// already refuses on both of these; this path used to discard the send error
	// and delete the row regardless, which left the jar loading on the server with
	// nothing in the UI left to remove it with.
	if h.state.Queue == nil {
		sendJSONError(w, "Queue unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := h.state.Queue.SendCommand(context.Background(), node.Token, "remove_mod", map[string]interface{}{
		"uuid":            srv.UUID,
		"activeSubServer": srv.ActiveSubServer,
		"targetDir":       targetDir,
		"fileName":        target.FileName,
	}, nil); err != nil {
		sendJSONError(w, "Failed to queue removal", http.StatusInternalServerError)
		return
	}
	if err := h.state.Store.DeleteServerMod(modID, serverID); err != nil {
		sendJSONError(w, "Failed to remove mod entry", http.StatusInternalServerError)
		return
	}

	h.state.Events.Publish(r.Context(), "server_mods.changed", map[string]interface{}{
		"serverId": serverID,
	})

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func isAllowedModrinthURL(u string) bool {
	// Cheap check — full url.Parse adds error-handling weight; we only
	// need scheme+host validation here.
	if !strings.HasPrefix(u, "https://") {
		return false
	}
	rest := strings.TrimPrefix(u, "https://")
	slash := strings.IndexByte(rest, '/')
	if slash < 1 {
		return false
	}
	host := rest[:slash]
	return modrinthAllowedHosts[host]
}

// uninstallTargetDir picks the directory to delete a mod's jar from.
//
// It is the directory the install RECORDED, not one re-derived from the server's
// loader, because the loader can change after the install: PATCH
// /api/servers/{id}/loader-metadata exists for exactly that (declaring the real
// loader of an imported server). A paper server whose plugin went into "plugins"
// and was then re-declared as fabric had its removal aimed at "mods", where the
// file has never been; the node deletes nothing, Core drops the row anyway, and
// the jar keeps loading with no entry in the UI left to remove it with.
//
// Rows written before target_dir existed hold "", and those were placed by the
// derived value, so falling back to it is what they need.
func uninstallTargetDir(m *models.ServerMod, installerType string) string {
	if m.TargetDir == "mods" || m.TargetDir == "plugins" {
		return m.TargetDir
	}
	return defaultTargetDirForLoader(installerType)
}

// defaultTargetDirForLoader returns "mods" for fabric/forge/quilt/neoforge,
// "plugins" for paper/spigot/bukkit/purpur. Unknown loader → "mods" (safer
// default since dropping a jar in "mods" never breaks a paper server, but
// dropping a fabric mod in "plugins" leaves it silently inactive).
func defaultTargetDirForLoader(loader string) string {
	switch strings.ToLower(loader) {
	case "paper", "spigot", "bukkit", "purpur", "pufferfish", "velocity", "waterfall", "bungeecord":
		return "plugins"
	default:
		return "mods"
	}
}
