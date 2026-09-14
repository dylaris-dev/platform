package handlers

import (
	"context"
	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-pkg/validate"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

// CreateServer (Step 1): Creates container with resources, status=pending_setup
func (h *ServerHandler) CreateServer(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}

	var req CreateServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}

	// The client supplies the UUID and it becomes the %s in every
	// dylaris:server:<uuid>:* Redis key and the node's server directory name, so
	// reject anything unsafe to interpolate there (also rejects an empty UUID,
	// which used to create an unroutable server).
	if !validate.IsServerUUID(req.UUID) {
		sendJSONError(w, "Invalid server UUID", http.StatusBadRequest)
		return
	}
	// Bound the requested resources (create had no check; negative/zero were
	// accepted and forwarded to the node). Host CPU ceiling is enforced node-side.
	if msg := validate.ResourceBounds(req.Docker.RAM, req.Docker.CPULimit, req.Docker.DiskLimit, 0); msg != "" {
		sendJSONError(w, msg, http.StatusBadRequest)
		return
	}

	var nodeIDInt int
	fmt.Sscanf(req.NodeID, "%d", &nodeIDInt)

	// Auto-placement: when no explicit nodeId is given, defer to the
	// scheduler. Region and tags (AND-filtered) narrow the candidate
	// pool. With both empty the scheduler considers every online node.
	hasFilters := strings.TrimSpace(req.Region) != "" || len(req.Tags) > 0 || strings.TrimSpace(req.Tag) != ""
	if nodeIDInt == 0 && hasFilters {
		pickReq := PickNodeRequest{
			Region:   req.Region,
			Tags:     req.Tags,
			Tag:      req.Tag,
			RAMMB:    req.Docker.RAM,
			CPUCores: req.Docker.CPULimit,
			DiskGB:   diskMBToGBCeil(req.Docker.DiskLimit),
		}
		// BYON: scope auto-placement to nodes the caller's party owns, so the
		// scheduler never picks a foreign node. No-op when BYON is off. See
		// applyPlacementScope for why an admin is scoped too.
		applyPlacementScope(h.state, r, &pickReq)
		pick := (&PlacementHandler{state: h.state}).pickNode(r.Context(), pickReq)
		if !pick.Success || pick.Picked == nil {
			sendJSONError(w, "No node available: "+pick.Reason, http.StatusServiceUnavailable)
			return
		}
		nodeIDInt = pick.Picked.NodeID
		log.Printf("Placement: picked node %d (%s) region=%q tags=%v — %s",
			nodeIDInt, pick.Picked.NodeName, req.Region, req.Tags, pick.Picked.Reason)
	}

	node, err := h.state.Store.GetNodeByID(nodeIDInt)
	if err != nil {
		sendJSONError(w, "Node not found", 404)
		return
	}

	// BYON placement scoping: a tenant may only deploy on their OWN node. Gated by
	// feature_byon_enabled, so with BYON off this is a no-op and placement behaves
	// as today. Auto-placement is already owner-scoped above; this gate covers the
	// explicit-nodeId path and is belt-and-suspenders. ownershipInForce rather
	// than byonActive: an unreadable flag must not skip the ownership check.
	if ownershipInForce(h.state, r) && !canPlaceOnNode(h.state, r, node) {
		sendJSONError(w, "You can only deploy on your own nodes", http.StatusForbidden)
		return
	}

	// BYON abuse guard: cap the number of servers a tenant can stack on one of
	// their own nodes. The limit protects the shared control plane (every
	// container is a DB row + Redis keys + heartbeat/stats work), not the node's
	// own hardware. Scales with the node's thread count. Admins are exempt.
	if byonActive(h.state, r) && !IsAdmin(r) {
		if reached, capN := h.byonNodeServerCapReached(r.Context(), node); reached {
			sendJSONError(w, fmt.Sprintf("This node's server limit (%d) is reached. Stop/delete a server or add another node.", capN), http.StatusForbidden)
			return
		}
	}

	// Non-admins can only create servers they own. Only admins may set an
	// arbitrary OwnerID; otherwise any authenticated user could create
	// servers under another user's identity and burn their quotas.
	if isAdmin, _ := r.Context().Value("isAdmin").(bool); !isAdmin {
		userID, _ := r.Context().Value("userID").(string)
		if userID == "" {
			sendJSONError(w, "Unauthorized", 401)
			return
		}
		req.OwnerID = userID
	}

	// Validate owner exists before going further.
	if _, err := h.state.Store.GetUserByID(req.OwnerID); err != nil {
		sendJSONError(w, "Owner not found", 404)
		return
	}

	// Container name: Heroku-style slug like "crimson-otter-7a3f". The 4-hex
	// suffix gives ~65k entropy per adj+noun pair, collisions are vanishingly
	// rare; DB uniqueness still enforces final correctness on insert.
	containerName := services.GenerateContainerSlug()

	isFixedVal := true
	if req.IsFixed != nil {
		isFixedVal = *req.IsFixed
	}

	// Default server type to "game" if not specified
	serverType := req.ServerType
	if serverType != "proxy" {
		serverType = "game"
	}

	// Block proxy creation if feature is disabled
	if serverType == "proxy" {
		val, _ := h.state.Store.GetSetting("feature_proxy_enabled")
		if val == "false" {
			sendJSONError(w, "Proxy feature is disabled", 403)
			return
		}
	}

	// RAM is stored without buffer (buffer is added on the Node side)
	srv := &models.Server{
		UUID:            req.UUID,
		Name:            containerName,
		NodeID:          nodeIDInt,
		OwnerID:         req.OwnerID,
		GameImage:       "ghcr.io/dylaris-dev/platform-mc-java21:latest", // Default, overridden during setup
		Port:            25565,
		Memory:          req.Docker.RAM,
		CPULimit:        req.Docker.CPULimit,
		StartCommand:    "",
		Status:          "pending_setup",
		IsFixed:         isFixedVal,
		ActiveSubServer: "",
		ExtraJvmFlags:   "",
		DiskLimit:       req.Docker.DiskLimit,
		ServerType:      serverType,
		AutoMove:        req.AutoMove,
		// From the NODE, not from req.Region. The request field is a scheduler
		// FILTER - it is only read when there is no explicit nodeId, and even then
		// it says which nodes were eligible, not where the server ended up. The
		// node is the only thing that knows where this server physically runs, and
		// that is what servers.region has to mean for CountServersInRegion (the
		// guard on deleting a region) to be right.
		Region: node.Region,
	}

	serverID, err := h.state.Store.CreateServer(srv)
	if err != nil {
		log.Printf("Create Server DB Error: %v", err)
		sendJSONError(w, "Failed to save server", 500)
		return
	}

	// Node: Only create directory + container (no install)
	if h.state.Queue != nil {
		configPayload := map[string]interface{}{
			"uuid":    req.UUID,
			"ownerId": srv.OwnerID,
			"docker": map[string]interface{}{
				"image":      srv.GameImage,
				"ram":        req.Docker.RAM,
				"cpuLimit":   req.Docker.CPULimit,
				"cpusetCpus": effectiveCpuset(srv.CPUPinningMode, srv.Cpuset, node.CpusetCpus),
				"diskLimit":  req.Docker.DiskLimit,
				"command":    "",
			},
		}

		if err := h.state.Queue.SendCommand(context.Background(), node.Token, "create", configPayload, nil); err != nil {
			log.Printf("Redis Queue Failed: %v", err)
		} else {
			// node.ID, not node.Token: the token is the node's gRPC credential.
			// node.Name is no safer - CreateNode seeds it FROM the token, so it
			// is the same string until an operator sets a display name.
			log.Printf("Create command queued for node %d", node.ID)
		}
	}

	h.state.Events.Publish(r.Context(), "servers.changed", nil)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"server_id": serverID,
		"message":   "Server container creation queued",
	})
}

// resolveJavaImage picks the image a setup or reinstall runs with: the one the
// request asks for, else the one already stored on the server.
//
// Setup used to take the requested image raw. A request without one wrote an
// empty image to the DB and dispatched it, and Docker accepts that: it builds a
// container with nothing in it, so the server dies on `exec: "java": executable
// file not found in $PATH` - by which point the previous sub-server's container
// is already gone. Reinstall had the fallback, setup did not; both go through
// here now, and an empty result is a 400 at the caller.
func resolveJavaImage(requested, stored string) string {
	if img := strings.TrimSpace(requested); img != "" {
		return img
	}
	return strings.TrimSpace(stored)
}

// SetupServer (Step 2): User configures the server
// cleanupDeletedServerKeys removes the per-server Redis keys a deleted server
// leaves behind. The node-side counterpart is cleanupDeletedNode (nodes.go),
// added for exactly this reason; servers were never given the same treatment.
//
// Most of the per-server keys carry a TTL and expire on their own. Two do not:
// the log stream dylaris:server:<uuid>:logs[:<sub>] and the stats buffer are
// Redis STREAMS, and while each is length-capped, nothing ever removes them -
// so the COUNT of orphaned streams grew by two per deleted server, forever.
// Measured on a deleted server: 5 of 7 keys had a TTL, both streams had -1.
//
// Scanning by the uuid prefix rather than listing key names keeps this correct
// as keys are added, and covers one log stream per sub-server without having to
// know their names after the rows are gone. The prefix is exact, so the sweep
// cannot reach another server's keys.
//
// Best-effort, like cleanupDeletedNode: the row is already gone and the request
// has succeeded, so a Redis hiccup here must not turn a completed delete into a
// 500. Core runs this rather than the node because the node's Redis ACL is
// scoped to the servers it currently owns - by delete time that grant is on its
// way out, and the node may be offline entirely (the delete proceeds with a
// warning in that case, and these keys still need to go).
func (h *ServerHandler) cleanupDeletedServerKeys(ctx context.Context, uuid string) {
	if h.state.Redis == nil || strings.TrimSpace(uuid) == "" {
		return
	}
	pattern := "dylaris:server:" + uuid + ":*"
	var cursor uint64
	removed := 0
	for {
		keys, next, err := h.state.Redis.Scan(ctx, cursor, pattern, 200).Result()
		if err != nil {
			log.Printf("DeleteServer: scanning leftover Redis keys for %s failed: %v", uuid, err)
			return
		}
		if len(keys) > 0 {
			if err := h.state.Redis.Del(ctx, keys...).Err(); err != nil {
				log.Printf("DeleteServer: removing leftover Redis keys for %s failed: %v", uuid, err)
				return
			}
			removed += len(keys)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	if removed > 0 {
		log.Printf("DeleteServer: server %s — removed %d leftover Redis key(s)", uuid, removed)
	}
}

// validateInstallerRequest rejects a setup the node cannot possibly complete.
// Returns "" when the request is acceptable.
//
// Every requirement below mirrors node/installer.go's InstallServer switch,
// which is where these values are actually consumed: installPaper and
// installVanilla take Version straight to the PaperMC / Mojang APIs,
// installFabric and installForge use it as the mcVersion in their meta lookup
// (both resolve the LOADER when it is blank, so only Version is required),
// installNeoForge is passed Loader as its version, and installFromLibrary
// needs a local path or a fallback URL.
//
// The type allowlist is validate.IsInstallerType, which already existed and
// had no caller at all. Reusing it rather than restating the set here is the
// point: it is also what excludes velocity/waterfall/bungeecord, which Core
// ADVERTISES via /api/versions/software (their version listing shares the
// PaperMC provider) but which are not an install source and have no case in
// the node's switch.
//
// Without this the node was the first and only thing to notice, long after the
// caller was told 200 "Server setup queued". Observed end to end on the
// testbed with type "paper" and no version: install failed, the reconciler
// restarted the container three times, and a brand-new server came to rest at
// "offline" with nothing installed.
//
// "pack" is deliberately accepted with no field requirement: SetupServer
// resolves and authorizes it below and rewrites Installer in place, so its
// own error paths already answer properly.
func validateInstallerRequest(typ, version, loader, url, path string) string {
	if !validate.IsInstallerType(typ) {
		return "Unsupported installer type"
	}
	needsVersion := map[string]bool{"paper": true, "vanilla": true, "fabric": true, "forge": true}
	if needsVersion[typ] && strings.TrimSpace(version) == "" {
		return "installer.version is required for " + typ
	}
	switch typ {
	case "neoforge":
		if strings.TrimSpace(loader) == "" {
			return "installer.loader is required for neoforge"
		}
	case "library":
		if strings.TrimSpace(path) == "" && strings.TrimSpace(url) == "" {
			return "installer.path or installer.url is required for library"
		}
	}
	return ""
}

// SetupServer POST /api/servers/{id}/setup - queues first-time provisioning of
// a created server: image, loader and version. The reply means queued; the
// node reports progress separately.
func (h *ServerHandler) SetupServer(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}

	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", 400)
		return
	}

	var req SetupServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}

	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", 404)
		return
	}

	isAdmin := r.Context().Value("isAdmin").(bool)

	// Atomically claim the install cooldown (30s, admins bypass). SetNX closes
	// the race where two concurrent requests both saw an expired TTL and both
	// enqueued a setup.
	cooldownKey := fmt.Sprintf("dylaris:server:%s:install-start", srv.UUID)
	if !isAdmin {
		acquired, err := h.state.Redis.SetNX(context.Background(), cooldownKey, "1", 30*time.Second).Result()
		if err == nil && !acquired {
			ttl, _ := h.state.Redis.TTL(context.Background(), cooldownKey).Result()
			sendJSONError(w, fmt.Sprintf("Please wait %d seconds before installing again", int(ttl.Seconds())), 429)
			return
		}
	}

	// The same rule SwitchSubServer applies, and for the same reason: this names
	// a directory on the node. It used to sanitize instead - stripping the
	// characters it did not like and accepting whatever was left - which had two
	// consequences. A caller got a sub-server under a name they did not choose
	// ("../escape" became "escape", "my server" became "my_server") and was never
	// told. And because sanitizing has no length bound while the rule does, it
	// could create a 51-character sub-server that no switch would ever accept.
	//
	// The panel already applies this exact rule to the field before submitting,
	// so nothing it can send changes shape here.
	subName := strings.TrimSpace(req.SubServerName)
	if !validate.IsSubServerName(subName) {
		sendJSONError(w, "Invalid sub-server name: letters, numbers, -, _ or +, up to 50 characters", 400)
		return
	}

	// Refuse before anything is written or the container is touched - see
	// resolveJavaImage.
	javaImage := resolveJavaImage(req.JavaImage, srv.GameImage)
	if javaImage == "" {
		sendJSONError(w, "javaImage is required", 400)
		return
	}
	// Same "refuse before anything is written" reason, for the installer. The
	// node is the only thing that ever checked these, and by then the request
	// has already been answered 200 and queued.
	if msg := validateInstallerRequest(
		req.Installer.Type, req.Installer.Version,
		req.Installer.Loader, req.Installer.URL, req.Installer.Path,
	); msg != "" {
		sendJSONError(w, msg, 400)
		return
	}
	// Refused here rather than dropped, and refused BEFORE anything is written or
	// queued: a wipe target the node will not recognise means the operator asked
	// for a clean install and would otherwise get a dirty one reported as a
	// success.
	if err := validateWipePaths(req.Installer.WipePaths); err != nil {
		sendJSONError(w, err.Error(), 400)
		return
	}

	// Enforce sub-server limit. Skipped during first setup only - the comment
	// here used to claim admins were exempt too, and they never have been:
	// the condition below reads srv.Status and nothing else.
	//
	// Read through the shared parser: this site used to hold its own copy of the
	// rule, guarded on `n > 0`, and therefore threw away a stored 0 and fell back
	// to the default of three. An operator asking for none silently got three,
	// and an operator asking for unlimited got three as well.
	if srv.Status != "pending_setup" {
		val, _ := h.state.Store.GetSetting(SettingMaxSubServers)
		if maxSub := services.ParseLimitSetting(val, defaultMaxSubServers); maxSub != nil {
			if known, ok := h.knownSubServers(r.Context(), srv.UUID); ok && services.AtOrOver(maxSub, int64(len(known))) {
				sendJSONError(w, fmt.Sprintf("Sub-server limit reached (%d). Change the limit in Settings → Servers.", *maxSub), 400)
				return
			}
		}
	}

	// Build JVM flags: default/Aikar flags followed by server-specific extra flags.
	// These are forwarded to the node as a dedicated field; the node builds the
	// full java start command itself via buildStartCommand.
	extraFlags := strings.TrimSpace(req.ExtraJvmFlags)
	if extraFlags == "" {
		if srv.Memory >= 12288 {
			extraFlags = aikarsHighMemFlags
		} else {
			extraFlags = aikarsFlags
		}
	}
	combinedJvmFlags := strings.TrimSpace(defaultJvmFlags + " " + extraFlags)

	// Update DB (start_command is display-only; store combined flags in extra_jvm_flags)
	if err := h.state.Store.UpdateServerSetup(serverID, javaImage, "", subName, extraFlags, req.Installer.Type, req.Installer.McVersion, req.Installer.Version); err != nil {
		sendJSONError(w, "Failed to update server", 500)
		return
	}
	h.state.Store.UpdateServerStatus(serverID, "installing")

	// Node command
	if h.state.Queue != nil {
		node, err := h.state.Store.GetNodeByID(srv.NodeID)
		if err != nil {
			sendJSONError(w, "Node not found", 404)
			return
		}

		// Cross-check snapshot inputs: capture the ORIGINAL installer type and
		// the external .mrpack URL BEFORE the pack branch rewrites them below.
		originalInstallerType := req.Installer.Type
		externalMrpackURL := req.Installer.URL
		var snapshotBuild *models.PackBuild

		// Unified pack install: authorize + materialize the build's .mrpack,
		// then rewrite req.Installer in place so the Node only ever sees the
		// existing "modpack" installer path. A foreign pack must never reach
		// the dispatch below.
		if req.Installer.Type == "pack" {
			if !h.state.FeatureFlags.IsModpacksEnabled(r.Context()) {
				sendJSONError(w, "Modpacks are disabled", http.StatusForbidden)
				return
			}

			pack, err := h.state.Store.GetPack(req.Installer.PackID)
			if err != nil || pack == nil {
				sendJSONError(w, "Pack not found", 404)
				return
			}
			packUserID, _ := r.Context().Value("userID").(string)
			if pack.OwnerID != packUserID && !isAdmin {
				sendJSONError(w, "Forbidden", 403)
				return
			}

			build, err := h.state.Store.GetPackBuild(req.Installer.BuildID)
			if err != nil || build == nil || build.PackID != pack.ID {
				sendJSONError(w, "Build not found", 404)
				return
			}
			snapshotBuild = build

			ph := NewPacksHandler(h.state)
			key, err := ph.ensureInstallMrpack(r.Context(), pack, build)
			if err != nil {
				log.Printf("ensureInstallMrpack failed for pack %d build %d: %v", pack.ID, build.ID, err)
				sendJSONError(w, "Failed to prepare pack for install", 500)
				return
			}
			base, err := solderMirrorBase(h.state.Store.GetSetting)
			if err != nil {
				log.Printf("solderMirrorBase failed: %v", err)
				sendJSONError(w, "Failed to prepare pack for install", 500)
				return
			}

			req.Installer.Type = "modpack"
			req.Installer.URL = base + key
			req.Installer.Loader = build.Loader
			req.Installer.McVersion = build.Minecraft
			req.Installer.ModrinthProjectID = ""
			req.Installer.ModrinthVersionID = ""
			req.Installer.ModrinthProjectSlug = ""
		}

		// Record HOW this sub-server is being installed, now that the pack
		// branch above has resolved a pack into its concrete versions.
		//
		// originalInstallerType, not req.Installer.Type: a pack was rewritten to
		// "modpack" for the node's benefit, and the panel has to put the operator
		// back on the Packs tab, not on Modrinth. Written wholesale so a reinstall
		// away from a modpack CLEARS the old reference instead of leaving the
		// panel prefilling a pack the directory no longer contains.
		//
		// Best-effort: a failure here must not fail an install that is about to be
		// dispatched. The cost of losing it is a form that cannot prefill, which is
		// exactly where this feature started.
		if err := h.state.Store.UpsertSubServerInstall(models.SubServerInstall{
			ServerID:            serverID,
			SubServerName:       subName,
			InstallerType:       originalInstallerType,
			McVersion:           req.Installer.McVersion,
			BuildVersion:        req.Installer.Version,
			Loader:              req.Installer.Loader,
			ModrinthProjectID:   req.Installer.ModrinthProjectID,
			ModrinthVersionID:   req.Installer.ModrinthVersionID,
			ModrinthProjectSlug: req.Installer.ModrinthProjectSlug,
			PackID:              req.Installer.PackID,
			PackBuildID:         req.Installer.BuildID,
		}); err != nil {
			log.Printf("sub-server install record failed for server %d/%s: %v", serverID, subName, err)
		}

		configPayload := map[string]interface{}{
			"uuid":    srv.UUID,
			"ownerId": srv.OwnerID,
			"docker": map[string]interface{}{
				"image":         javaImage,
				"ram":           srv.Memory,
				"cpuLimit":      srv.CPULimit,
				"cpusetCpus":    effectiveCpuset(srv.CPUPinningMode, srv.Cpuset, node.CpusetCpus),
				"extraJvmFlags": combinedJvmFlags,
			},
			"activeSubServer": subName,
		}
		installerPayload := map[string]interface{}{
			"type":      req.Installer.Type,
			"version":   req.Installer.Version,
			"loader":    req.Installer.Loader,
			"url":       req.Installer.URL,
			"path":      req.Installer.Path,
			"structure": req.Installer.Structure,
			// Modpack metadata forwarded to the node installer.
			"modrinthProjectId":   req.Installer.ModrinthProjectID,
			"modrinthVersionId":   req.Installer.ModrinthVersionID,
			"modrinthProjectSlug": req.Installer.ModrinthProjectSlug,
			// What to clear first. The node validates these again against its own
			// copy of the vocabulary before it deletes anything.
			"wipePaths": req.Installer.WipePaths,
		}

		if err := h.state.Queue.SendCommand(context.Background(), node.Token, "setup", configPayload, installerPayload); err != nil {
			log.Printf("Redis Queue Failed: %v", err)
			sendJSONError(w, "Failed to queue setup", 500)
			return
		}

		// Set install cooldown
		h.state.Redis.Set(context.Background(), cooldownKey, "1", 30*time.Second)

		// Setup installs and starts the server — mark desired state as online
		h.state.Store.UpdateServerDesiredState(srv.ID, "online")

		// Snapshot the modpack's Modrinth members for the Content-tab
		// cross-check. Advisory only: the helper logs and swallows every
		// failure. Run in the background so the external-modpack case (which
		// fetches the .mrpack over the network) cannot delay this response; the
		// install command was already dispatched above.
		go h.snapshotModpackContents(serverID, subName, originalInstallerType, snapshotBuild, externalMrpackURL)
	}

	actorID, _ := r.Context().Value("userID").(string)
	LogServerAudit(h.state, r, serverID, ServerAuditEventSetup, actorID, "", map[string]interface{}{
		"sub_server": subName,
		"installer":  req.Installer.Type,
		"version":    req.Installer.McVersion,
	})

	h.state.Events.Publish(r.Context(), "servers.changed", nil)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Server setup queued",
	})
}

// ReinstallServer: Reinstalls the active sub-server (version update)
func (h *ServerHandler) ReinstallServer(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}

	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", 400)
		return
	}

	var req struct {
		Installer struct {
			Type      string `json:"type"`
			Version   string `json:"version"`
			McVersion string `json:"mcVersion"`
			Loader    string `json:"loader"`
			URL       string `json:"url"`
			Path      string `json:"path"`
			Structure string `json:"structure"`
		} `json:"installer"`
		JavaImage     string `json:"javaImage"`
		ExtraJvmFlags string `json:"extraJvmFlags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}

	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", 404)
		return
	}

	isAdmin := r.Context().Value("isAdmin").(bool)

	if srv.Status == "pending_setup" {
		sendJSONError(w, "Server must be set up before reinstalling", 400)
		return
	}

	// Atomically claim the install cooldown (30s, admins bypass) — SetNX closes
	// the concurrent-request race.
	cooldownKey := fmt.Sprintf("dylaris:server:%s:install-start", srv.UUID)
	if !isAdmin {
		acquired, err := h.state.Redis.SetNX(context.Background(), cooldownKey, "1", 30*time.Second).Result()
		if err == nil && !acquired {
			ttl, _ := h.state.Redis.TTL(context.Background(), cooldownKey).Result()
			sendJSONError(w, fmt.Sprintf("Please wait %d seconds before reinstalling", int(ttl.Seconds())), 429)
			return
		}
	}

	subName := srv.ActiveSubServer
	if subName == "" {
		sendJSONError(w, "No active sub-server to reinstall", 400)
		return
	}

	// Update image if provided
	javaImage := resolveJavaImage(req.JavaImage, srv.GameImage)
	if javaImage == "" {
		sendJSONError(w, "javaImage is required", 400)
		return
	}

	extraFlags := strings.TrimSpace(req.ExtraJvmFlags)
	if extraFlags == "" {
		extraFlags = srv.ExtraJvmFlags
	}
	combinedJvmFlags := strings.TrimSpace(defaultJvmFlags + " " + extraFlags)

	// Update DB (start_command is display-only; node builds the real command)
	installerType := req.Installer.Type
	if installerType == "" {
		installerType = srv.InstallerType
	}
	mcVersion := req.Installer.McVersion
	if mcVersion == "" {
		mcVersion = srv.MinecraftVersion
	}
	buildNumber := req.Installer.Version
	if buildNumber == "" {
		buildNumber = srv.BuildNumber
	}

	if err := h.state.Store.UpdateServerSetup(serverID, javaImage, "", subName, extraFlags, installerType, mcVersion, buildNumber); err != nil {
		sendJSONError(w, "Failed to update server", 500)
		return
	}
	h.state.Store.UpdateServerStatus(serverID, "installing")

	if h.state.Queue != nil {
		node, err := h.state.Store.GetNodeByID(srv.NodeID)
		if err != nil {
			sendJSONError(w, "Node not found", 404)
			return
		}

		configPayload := map[string]interface{}{
			"uuid": srv.UUID,
			"docker": map[string]interface{}{
				"image":         javaImage,
				"ram":           srv.Memory,
				"cpuLimit":      srv.CPULimit,
				"cpusetCpus":    effectiveCpuset(srv.CPUPinningMode, srv.Cpuset, node.CpusetCpus),
				"extraJvmFlags": combinedJvmFlags,
			},
			"activeSubServer": subName,
		}
		installerPayload := map[string]interface{}{
			"type":      req.Installer.Type,
			"version":   req.Installer.Version,
			"loader":    req.Installer.Loader,
			"url":       req.Installer.URL,
			"path":      req.Installer.Path,
			"structure": req.Installer.Structure,
		}

		if err := h.state.Queue.SendCommand(context.Background(), node.Token, "reinstall", configPayload, installerPayload); err != nil {
			log.Printf("Redis Queue Failed: %v", err)
			sendJSONError(w, "Failed to queue reinstall", 500)
			return
		}

		// Set install cooldown
		h.state.Redis.Set(context.Background(), cooldownKey, "1", 30*time.Second)

		// Refresh the modpack cross-check snapshot for the reinstalled
		// sub-server. Advisory only, never blocks; run in the background so an
		// external-modpack fetch cannot delay this response. The /reinstall
		// request carries no packId/buildId, so the "pack" branch gets a nil
		// build and no-ops (keeping the existing snapshot, which is still
		// correct for a same-pack reinstall); this call therefore refreshes the
		// external-modpack URL case and clears when switching to a non-modpack
		// installer. The panel reinstalls a pack via /setup, not this route.
		go h.snapshotModpackContents(serverID, subName, installerType, nil, req.Installer.URL)
	}

	actorID, _ := r.Context().Value("userID").(string)
	LogServerAudit(h.state, r, serverID, ServerAuditEventReinstall, actorID, "", map[string]interface{}{
		"sub_server": subName,
		"installer":  installerType,
		"version":    mcVersion,
	})

	h.state.Events.Publish(r.Context(), "servers.changed", nil)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Server reinstall queued",
	})
}

// knownSubServers reports which sub-servers the node last saw on disk for a
// server, and whether that answer is known at all.
//
// The source is the node's disk-usage report (dylaris:server:<uuid>:stats:disk),
// which carries a per-sub-server size map. It is the only sub-server inventory
// Core holds without a round trip to the node.
//
// ok=false means "no answer", not "none": the key expires ten minutes after the
// last report, so a server whose node just restarted, or one that has never run,
// legitimately has nothing here. Callers must not treat that as an empty
// inventory - refusing every switch and every install while a cache is cold
// would be a worse failure than the one this guards against, and the node
// refuses an impossible request safely either way.
//
// Both readers of this key go through here. They used to parse it separately,
// which is how one of them ended up asserting against a map that the node's
// non-quota path never filled.
func (h *ServerHandler) knownSubServers(ctx context.Context, serverUUID string) (map[string]bool, bool) {
	if h.state == nil || h.state.Redis == nil {
		return nil, false
	}
	data, err := h.state.Redis.Get(ctx, fmt.Sprintf("dylaris:server:%s:stats:disk", serverUUID)).Result()
	if err != nil {
		return nil, false
	}
	var payload struct {
		SubServers map[string]int64 `json:"subServers"`
	}
	if json.Unmarshal([]byte(data), &payload) != nil || payload.SubServers == nil {
		return nil, false
	}
	known := make(map[string]bool, len(payload.SubServers))
	for name := range payload.SubServers {
		known[name] = true
	}
	return known, true
}

// SwitchSubServer: Switches the active MC server in the container
func (h *ServerHandler) SwitchSubServer(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}

	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", 400)
		return
	}

	var req SwitchSubServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}

	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", 404)
		return
	}

	// Rejected, not sanitized. Setup may sanitize because it is NAMING a new
	// directory; this is picking an existing one, and quietly rewriting the
	// caller's string switches them to a different sub-server than they asked
	// for. "../escape" used to sanitize to "escape" and be accepted.
	subName := strings.TrimSpace(req.SubServerName)
	if !validate.IsSubServerName(subName) {
		sendJSONError(w, "Invalid sub-server name", 400)
		return
	}

	// The sub-server has to exist before it becomes this server's active one.
	// Without this, any owner could switch to a name that is not on disk: the
	// write below succeeded, the node then failed asynchronously with "no
	// runnable server found", and Core's active_sub_server and the node's
	// .active_server disagreed from then on - with Core's pointing at nothing.
	// Everything keyed on the active sub-server (the console above all) reads
	// empty in that state while the old sub-server is still the one running.
	if known, ok := h.knownSubServers(r.Context(), srv.UUID); ok && !known[subName] {
		sendJSONError(w, "No sub-server named "+subName+" on this server", 400)
		return
	}

	if err := h.state.Store.UpdateServerActiveSubServer(serverID, subName); err != nil {
		sendJSONError(w, "Failed to update server", 500)
		return
	}

	// Before the node is told to switch, not after: it restarts the container,
	// and MC reads server.properties once at boot. The RCON config is stored per
	// SERVER and lives per SUB-SERVER, so without this the switch quietly moved
	// to a sub-server whose file never had enable-rcon set - Core kept reporting
	// RCON as on with nothing pending, and the Players tabs unlocked onto a port
	// nothing was listening on. Same reason the loader-metadata refresh below
	// exists: a plain switch used to move only active_sub_server.
	// The flag rides along in both directions. A switch restarts the container,
	// so a stamp that landed IS live afterwards - and only the power route used
	// to clear it, which left "enable RCON, then switch" showing the
	// restart-pending banner and the Players tabs locked until someone pressed
	// restart for no reason. A stamp that did NOT land means the file lacks the
	// setting, so a restart really is still owed.
	needsRestart := !h.syncRconToSubServer(srv, subName)
	if err := h.state.Store.SetServerRconNeedsRestart(serverID, needsRestart); err != nil {
		log.Printf("SwitchSubServer: rcon needs-restart for server %d: %v", serverID, err)
	}

	if h.state.Queue != nil {
		node, err := h.state.Store.GetNodeByID(srv.NodeID)
		if err == nil {
			combinedJvmFlags := strings.TrimSpace(defaultJvmFlags + " " + strings.TrimSpace(srv.ExtraJvmFlags))
			switchPayload := map[string]interface{}{
				"uuid":            srv.UUID,
				"activeSubServer": subName,
				"docker": map[string]interface{}{
					"image":         srv.GameImage,
					"ram":           srv.Memory,
					"cpuLimit":      srv.CPULimit,
					"cpusetCpus":    effectiveCpuset(srv.CPUPinningMode, srv.Cpuset, node.CpusetCpus),
					"extraJvmFlags": combinedJvmFlags,
				},
			}
			h.state.Queue.SendCommand(context.Background(), node.Token, "switch_server", switchPayload, nil)
		}
	}

	// Bring the server row's loader metadata in step with the sub-server we just
	// switched to. installer_type/minecraft_version drive the Content tab's mod
	// browser, the mods-vs-plugins install target, and version highlighting.
	// Setup writes them, but a plain switch used to move only active_sub_server,
	// so after switching between sub-servers of different loaders the row kept
	// pointing at whichever was set up last - installing a plugin onto a
	// switched-to Paper server then wrote into mods/ (or a mod into plugins/) and
	// silently never loaded. The node's .dylaris.json is authoritative per
	// sub-server; refresh from it best-effort. A missing sub-server or an
	// unreachable node leaves the existing values rather than wiping them (the
	// switch itself already needs the node, so a node outage stops it upstream).
	if h.state.GRPCRegistry != nil {
		if resp, ierr := inspectOrphanOnNode(h.state, srv.NodeID, srv.UUID); ierr != nil {
			log.Printf("SwitchSubServer: loader-metadata refresh for %s/%s skipped: %v", srv.UUID, subName, ierr)
		} else if it, mv, build, ok := subServerLoaderFromInspect(resp, subName); ok {
			if err := h.state.Store.UpdateServerLoaderMetadata(serverID, it, mv, build); err != nil {
				log.Printf("SwitchSubServer: persist loader metadata for server %d failed: %v", serverID, err)
			}
		}
	}

	actorID, _ := r.Context().Value("userID").(string)
	LogServerAudit(h.state, r, serverID, ServerAuditEventSubServerSwitched, actorID, "", map[string]interface{}{
		"from": srv.ActiveSubServer,
		"to":   subName,
	})

	h.state.Events.Publish(r.Context(), "servers.changed", nil)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Sub-server switch queued",
	})
}

// GetInstallCooldown returns how many seconds remain on the post-install
// cooldown for a server, so the UI can disable power actions and show a
// countdown instead of letting the user click and get a 429. Returns 0
// when no cooldown is active. Admins still see the real number; the gate
// in ServerPowerHandler is the one that exempts them.
func (h *ServerHandler) GetInstallCooldown(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", 400)
		return
	}
	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", 404)
		return
	}
	seconds := 0
	cooldownKey := fmt.Sprintf("dylaris:server:%s:install-start", srv.UUID)
	if ttl, err := h.state.Redis.TTL(context.Background(), cooldownKey).Result(); err == nil && ttl > 0 {
		seconds = int(ttl.Seconds())
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"seconds": seconds,
	})
}

// ServerPowerHandler: Controls Start, Stop, Kill and Restart
func (h *ServerHandler) ServerPowerHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}

	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", 400)
		return
	}

	var req PowerActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}

	validActions := map[string]bool{"start": true, "stop": true, "restart": true, "kill": true}
	if !validActions[req.Action] {
		sendJSONError(w, "Invalid action", 400)
		return
	}

	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", 404)
		return
	}

	// Server must be set up before it can be started
	if srv.Status == "pending_setup" {
		sendJSONError(w, "Server is not set up yet", 400)
		return
	}

	// Block start/restart when disk quota is full
	if srv.Status == "disk_full" && (req.Action == "start" || req.Action == "restart") {
		sendJSONError(w, "Server cannot start - storage limit reached. Delete files or raise the limit.", 400)
		return
	}

	username := r.Context().Value("username").(string)
	isAdmin := r.Context().Value("isAdmin").(bool)
	userID, _ := r.Context().Value("userID").(string)

	// POWER is in-handler-enforced (Rule R5): the action is in the request body,
	// so a route-level RequireCap cannot distinguish a kill-only holder from a
	// start-only holder. Resolve the per-action cap here instead.
	res, rerr := h.state.Authz.Resolve(authz.Identity{UserID: userID, Username: username, IsAdmin: isAdmin}, srv.ID)
	if rerr != nil || !res.HasCap("power."+req.Action) {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}

	if srv.Status == "suspended" && !isAdmin {
		sendJSONError(w, "Server is suspended. Action blocked.", 403)
		return
	}

	// BYON billing suspend: a suspended tenant keeps read access but cannot
	// start/restart their servers until payment is settled (an operator can
	// reactivate). past_due (grace) is unaffected. Admins bypass.
	if (req.Action == "start" || req.Action == "restart") && !isAdmin {
		if b, err := h.state.Store.GetUserBilling(srv.OwnerID); err == nil && b.Status == "suspended" {
			sendJSONError(w, "Account suspended for non-payment. Settle payment to start your servers.", 403)
			return
		}
	}

	// Operator override transparency: the two suspend gates above deliberately
	// let an admin through (a force-suspended server, or a billing-suspended
	// owner) so support keeps control. Detect such an admin start/restart so the
	// power-action audit entry below records the override instead of it being
	// silent.
	//
	// The override is genuinely temporary, which is what makes letting an admin
	// through safe: suspension state lives in user_billing, NOT on the server
	// row, so starting the server cannot clear it. GetUserBilling still reports
	// suspended afterwards and the hourly billing-lifecycle enforcement pass
	// stops the tenant's servers again.
	//
	// srv.Status == "suspended" is a different matter: no code path in either
	// repo ever writes that value (verified repo-wide incl. SQL - suspension is
	// modelled entirely on the owner), so today it is only reachable by editing
	// the row by hand. The gate stays because it is an authorization check on a
	// power action, and a guard that costs one comparison is the wrong place to
	// economise - but do not read it as evidence that per-server suspension
	// exists.
	suspendOverride := ""
	if isAdmin && (req.Action == "start" || req.Action == "restart") {
		if srv.Status == "suspended" {
			suspendOverride = "server_suspended"
		} else if b, err := h.state.Store.GetUserBilling(srv.OwnerID); err == nil && b.Status == "suspended" {
			suspendOverride = "owner_billing_suspended"
		}
		if suspendOverride != "" {
			log.Printf("Suspend override: admin %s performed %s on server %d (%s)", username, req.Action, srv.ID, suspendOverride)
		}
	}

	// Install cooldown: 30s after setup/reinstall the node sets a Redis key
	// with TTL so the freshly-installed server can boot to a stable state
	// without a foot-gun start/stop/kill during world generation. The same
	// key is checked on setup + reinstall to debounce double-clicks; we
	// gate power actions on it too. Admins bypass but the frontend prompts
	// for an explicit confirmation.
	if !isAdmin {
		cooldownKey := fmt.Sprintf("dylaris:server:%s:install-start", srv.UUID)
		if ttl, err := h.state.Redis.TTL(context.Background(), cooldownKey).Result(); err == nil && ttl > 0 {
			sendJSONError(w, fmt.Sprintf("Server is finishing install — please wait %d seconds", int(ttl.Seconds())), 429)
			return
		}
	}

	// Status-transition sanity: the frontend already disables nonsensical
	// transitions (Start on an online server, Stop on an offline one) but
	// the API was happy to forward them to the node anyway -- a misbehaving
	// client could spam Stop on a stopped server, queueing N redundant
	// commands the node has to chew through. Reject them at the boundary.
	isOffline := srv.Status == "stopped" || srv.Status == "offline" || srv.Status == "disk_full"
	isOnline := srv.Status == "online"
	switch req.Action {
	case "start":
		if isOnline {
			sendJSONError(w, "Server is already running", 409)
			return
		}
	case "stop", "kill", "restart":
		if isOffline {
			sendJSONError(w, "Server is not running", 409)
			return
		}
	}

	node, err := h.state.Store.GetNodeByID(srv.NodeID)
	if err != nil {
		sendJSONError(w, "Node not found", 404)
		return
	}

	newStatus := ""
	switch req.Action {
	case "start", "restart":
		newStatus = "starting"
		h.state.Store.UpdateServerDesiredState(srv.ID, "online")
		// MC reads server.properties at boot and nowhere else, so this is the
		// one moment the stored RCON config can be made true for whichever
		// sub-server is about to run. See rconNeedsStamping for why it can be
		// a different one than SetConfig last wrote.
		rconSynced := h.syncRconToSubServer(srv, srv.ActiveSubServer)
		// A (re)start reloads server.properties, so any pending RCON change is
		// now live: clear the persisted "restart required" flag that keeps the
		// panel banner up and the RCON-dependent Players tabs locked. Non-fatal
		// - failing to clear it must not block the restart itself.
		//
		// Only when the stamp above actually landed: clearing it after a failed
		// write is how the panel came to unlock the Players tabs for a server
		// whose file never got the setting.
		if rconSynced {
			if err := h.state.Store.SetServerRconNeedsRestart(srv.ID, false); err != nil {
				log.Printf("failed to clear rcon needs-restart for server %d: %v", srv.ID, err)
			}
		}
	case "stop", "kill":
		newStatus = "stopping"
		h.state.Store.UpdateServerDesiredState(srv.ID, "stopped")
	}
	h.state.Store.UpdateServerStatus(srv.ID, newStatus)

	if h.state.Queue != nil {
		configPayload := map[string]interface{}{
			"uuid": srv.UUID,
		}
		if err := h.state.Queue.SendCommand(context.Background(), node.Token, req.Action, configPayload, nil); err != nil {
			log.Printf("Redis Queue Failed: %v", err)
			sendJSONError(w, "Failed to queue command", 500)
			return
		}
	}

	powerMeta := map[string]interface{}{"action": req.Action}
	if suspendOverride != "" {
		powerMeta["admin_suspend_override"] = suspendOverride
	}
	LogServerAudit(h.state, r, serverID, ServerAuditEventPowerAction, userID, "", powerMeta)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Action " + req.Action + " queued successfully",
	})
}

// UpdateServerName PATCH /api/servers/{id}/name - renames a server and records
// the old and new name in its audit trail.
func (h *ServerHandler) UpdateServerName(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}

	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", 400)
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}

	// Collapse whitespace, then validate against the shared server-name rule so
	// create and rename agree on one alphabet.
	name := strings.Join(strings.Fields(req.Name), " ")
	if !validate.IsServerName(name) {
		sendJSONError(w, "Invalid name: 1-50 characters, start with a letter or digit, then letters, digits, space, '-', '+' or '_'", 400)
		return
	}

	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", 404)
		return
	}

	previousName := srv.Name
	if err := h.state.Store.UpdateServerName(serverID, name); err != nil {
		sendJSONError(w, "Failed to update name", 500)
		return
	}

	actorID, _ := r.Context().Value("userID").(string)
	LogServerAudit(h.state, r, serverID, ServerAuditEventNameChanged, actorID, "", map[string]interface{}{
		"from": previousName,
		"to":   name,
	})

	h.state.Events.Publish(r.Context(), "servers.changed", nil)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// UpdateServerResources PATCH /api/servers/{id}/resources - changes RAM, CPU
// and disk limits. Beyond the route's capability an admin must have set the
// caller's can_change_resources flag; without it the answer is 403, because
// resources are what a plan is sold on.
func (h *ServerHandler) UpdateServerResources(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}

	// Capability gate. Admins always pass; non-admins need the
	// can_change_resources flag set by an admin in the user-settings UI.
	userID, _ := r.Context().Value("userID").(string)
	perms := LoadEffectivePermissions(h.state, userID)
	if !perms.IsAdmin && !perms.CanChangeResources {
		sendJSONError(w, "Changing server resources requires elevated permissions — contact an administrator", 403)
		return
	}

	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", 400)
		return
	}

	var req struct {
		RAM           int     `json:"ram"`
		CPULimit      float64 `json:"cpuLimit"`
		DiskLimit     int64   `json:"diskLimit"`
		HostPort      int     `json:"hostPort"`      // admin-only
		ContainerPort int     `json:"containerPort"` // admin-only
		// Optional CPU pinning change. Omitted (nil) = leave pinning unchanged.
		CPUPinningMode *string `json:"cpuPinningMode"`
		Cpuset         *string `json:"cpuset"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}

	// Bound all three resources: the update path used to check only RAM, so a
	// negative CPU or disk was forwarded to the node's docker config.
	if msg := validate.ResourceBounds(req.RAM, req.CPULimit, req.DiskLimit, 0); msg != "" {
		sendJSONError(w, msg, 400)
		return
	}

	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", 404)
		return
	}

	isAdmin := r.Context().Value("isAdmin").(bool)

	if err := h.state.Store.UpdateServerResources(serverID, req.RAM, req.CPULimit, req.DiskLimit); err != nil {
		sendJSONError(w, "Failed to update resources", 500)
		return
	}

	// Optional per-server CPU pinning change. Resolved into an effective cpuset
	// and persisted here; it is applied to the container by the recreate below.
	if req.CPUPinningMode != nil && h.state.CPUPinning != nil {
		mode := strings.TrimSpace(*req.CPUPinningMode)
		pinNode, perr := h.state.Store.GetNodeByID(srv.NodeID)
		if perr != nil {
			sendJSONError(w, "Node not found", 404)
			return
		}
		var newCpuset string
		switch mode {
		case "shared":
			newCpuset = ""
		case "manual":
			cs := ""
			if req.Cpuset != nil {
				cs = strings.TrimSpace(*req.Cpuset)
			}
			if verr := h.state.CPUPinning.ValidateCpuset(r.Context(), pinNode.Token, cs, pinNode.CpusetCpus); verr != nil {
				sendJSONError(w, "Invalid cpuset: "+verr.Error(), 400)
				return
			}
			newCpuset = cs
		case "auto":
			// "" when the node has not reported a topology yet; mode stays 'auto'
			// and the effective cpuset falls back to the node default this time.
			newCpuset, _ = h.state.CPUPinning.AutoCpuset(r.Context(), pinNode.Token, srv.NodeID, srv.ID, req.CPULimit, pinNode.CpusetCpus)
		default:
			sendJSONError(w, "Invalid cpuPinningMode (shared|auto|manual)", 400)
			return
		}
		if err := h.state.Store.UpdateServerCPUPinning(srv.ID, mode, newCpuset); err != nil {
			sendJSONError(w, "Failed to save CPU pinning", 500)
			return
		}
		srv.CPUPinningMode = mode
		srv.Cpuset = newCpuset
	}

	// Port changes: admin-only
	portChanged := false
	if isAdmin && (req.HostPort > 0 || req.ContainerPort > 0) {
		newHostPort := req.HostPort
		if newHostPort == 0 {
			newHostPort = srv.HostPort
		}
		newContainerPort := req.ContainerPort
		if newContainerPort == 0 {
			newContainerPort = srv.ContainerPort
		}
		if newContainerPort == 0 {
			newContainerPort = 25565
		}
		// Conflict check: ensure no other server on this node uses the same host port
		if req.HostPort > 0 && req.HostPort != srv.HostPort {
			// Fail CLOSED. Discarding this error left usedPorts empty, the
			// loop below found no conflict, and the port was assigned anyway -
			// so a database hiccup turned a conflict check into a rubber stamp.
			usedPorts, uerr := h.state.Store.GetUsedHostPortsOnNode(srv.NodeID)
			if uerr != nil {
				sendJSONError(w, "Could not verify host port availability", http.StatusInternalServerError)
				return
			}
			for _, p := range usedPorts {
				if p == req.HostPort {
					sendJSONError(w, "Host port already in use on this node", 409)
					return
				}
			}
		}
		if err := h.state.Store.UpdateServerPorts(serverID, newHostPort, newContainerPort); err != nil {
			sendJSONError(w, "Failed to update ports", 500)
			return
		}
		portChanged = req.HostPort > 0 && req.HostPort != srv.HostPort
		srv.HostPort = newHostPort
		srv.ContainerPort = newContainerPort
	}

	// Regenerate start_command with the new RAM value
	newStartCommand := fmt.Sprintf("java -Xms%dM -Xmx%dM %s %s -jar server.jar nogui",
		req.RAM, req.RAM, defaultJvmFlags, strings.TrimSpace(srv.ExtraJvmFlags))
	newStartCommand = strings.Join(strings.Fields(newStartCommand), " ")

	// Persist the updated start_command in the DB
	h.state.Store.UpdateServerSetup(serverID, srv.GameImage, newStartCommand,
		srv.ActiveSubServer, srv.ExtraJvmFlags,
		srv.InstallerType, srv.MinecraftVersion, srv.BuildNumber)

	// Inform node to recreate container with new resources (and port if changed)
	if h.state.Queue != nil {
		node, err := h.state.Store.GetNodeByID(srv.NodeID)
		if err == nil {
			dockerPayload := map[string]interface{}{
				"ram":        req.RAM,
				"cpuLimit":   req.CPULimit,
				"cpusetCpus": effectiveCpuset(srv.CPUPinningMode, srv.Cpuset, node.CpusetCpus),
				"diskLimit":  req.DiskLimit,
				"image":      srv.GameImage,
				"command":    newStartCommand,
			}
			if portChanged {
				dockerPayload["hostPort"] = srv.HostPort
				dockerPayload["containerPort"] = srv.ContainerPort
			}
			payload := map[string]interface{}{
				"uuid":            srv.UUID,
				"activeSubServer": srv.ActiveSubServer,
				"docker":          dockerPayload,
			}
			h.state.Queue.SendCommand(context.Background(), node.Token, "update_resources", payload, nil)
		}
	}

	actorID, _ := r.Context().Value("userID").(string)
	LogServerAudit(h.state, r, serverID, ServerAuditEventResourcesChanged, actorID, "", map[string]interface{}{
		"ram":       req.RAM,
		"cpuLimit":  req.CPULimit,
		"diskLimit": req.DiskLimit,
	})

	h.state.Events.Publish(r.Context(), "servers.changed", nil)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// SetServerAutoMove PATCH /api/servers/{id}/automove
// Flips the auto-move opt-in flag. Migration itself is handled by the
// rebalance worker — this endpoint only stores intent.
func (h *ServerHandler) SetServerAutoMove(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", 400)
		return
	}
	if _, err := h.state.Store.GetServerByID(serverID); err != nil {
		sendJSONError(w, "Server not found", 404)
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}
	if err := h.state.Store.SetServerAutoMove(serverID, req.Enabled); err != nil {
		sendJSONError(w, "Failed to update auto-move", 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// MoveServer (admin oversight, PANEL servers.write) queues a manual node-to-node
// migration of a server to a target node. Async: it only enqueues; the
// leader-elected Core runs the migration step machine and the panel polls the
// orchestration status key. The route is gateway-gated (migration is
// gateway-only) via RequireGatewayEnabled.
func (h *ServerHandler) MoveServer(w http.ResponseWriter, r *http.Request) {
	if h.state.Migration == nil {
		sendJSONError(w, "Migration orchestrator not available", 503)
		return
	}
	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", 400)
		return
	}
	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", 404)
		return
	}
	var req struct {
		TargetNodeID int `json:"targetNodeId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}
	// Oversight moves between the platform's own machines. Either end on a
	// customer's machine is their world leaving their hardware, or ours landing
	// on a box they have root on.
	if foreignToCaller(h.state, r, srv.NodeID) {
		sendJSONError(w, "Server not found", 404)
		return
	}
	target, err := h.state.Store.GetNodeByID(req.TargetNodeID)
	if err != nil || target == nil || foreignToCaller(h.state, r, target.ID) {
		sendJSONError(w, "Target node not found", 404)
		return
	}
	username, _ := r.Context().Value("username").(string)
	h.queueMigration(w, r, srv, target, "manual", username)
}

// TransferServer (tenant) queues a node-to-node migration of a server the caller
// OWNS to a target node they may place on. BYON-only: outside BYON mode, moves
// stay admin-only (use MoveServer). The target picker is the tenant-scoped node
// list, so in practice this covers moving between the user's own nodes (and
// pulling a platform-hosted server onto their own hardware). Same orchestrator +
// status as the admin move; gateway-gated (migration is gateway-only).
func (h *ServerHandler) TransferServer(w http.ResponseWriter, r *http.Request) {
	if h.state.Migration == nil {
		sendJSONError(w, "Migration orchestrator not available", 503)
		return
	}
	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", 400)
		return
	}
	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", 404)
		return
	}
	// Ownership is now enforced by the route's RequireCap("server.settings.write");
	// BYON availability stays a separate feature-gate, not authz.
	username, _ := r.Context().Value("username").(string)
	if !IsAdmin(r) {
		if !byonActive(h.state, r) {
			sendJSONError(w, "Server transfer is not enabled", 403)
			return
		}
	}
	var req struct {
		TargetNodeID int `json:"targetNodeId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}
	target, err := h.state.Store.GetNodeByID(req.TargetNodeID)
	if err != nil {
		sendJSONError(w, "Target node not found", 404)
		return
	}
	// server.settings.write is also something an owner can GRANT, and the target
	// check below only asks where the caller may place. So an invitee could move
	// the owner's world onto the invitee's own machine, and an admin invited on a
	// customer's machine could pull the server onto ours. A transfer is the
	// owner's decision; oversight moves between platform machines are MoveServer.
	if foreignToCaller(h.state, r, srv.NodeID) || (!IsAdmin(r) && srv.OwnerID != byonCallerID(r)) {
		sendJSONError(w, "Only the server's owner can transfer it", 403)
		return
	}
	// Placement authz: the caller must be allowed to place on the target node
	// (their own BYON node, or a platform node for an admin).
	if !canPlaceOnNode(h.state, r, target) {
		sendJSONError(w, "You cannot place servers on the target node", 403)
		return
	}
	h.queueMigration(w, r, srv, target, "transfer", username)
}

// queueMigration runs the shared validation + enqueue for both the admin move
// and the tenant transfer: rejects a same-node target, requires the target be
// online, then enqueues onto the orchestrator (async) and returns 202. Callers
// own the access-control decision before calling this.
func (h *ServerHandler) queueMigration(w http.ResponseWriter, r *http.Request, srv *models.Server, target *models.Node, reason, requestedBy string) {
	if target.ID == srv.NodeID {
		sendJSONError(w, "Target node equals current node", 400)
		return
	}
	if target.Status != "online" {
		sendJSONError(w, "Target node is not online", 409)
		return
	}
	if err := h.state.Migration.EnqueueMigration(r.Context(), srv.ID, target.ID, reason, requestedBy); err != nil {
		sendJSONError(w, "Failed to queue migration", 500)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "migration queued"})
}

// byonNodeServerFallbackCap bounds servers-per-node when the node has not yet
// reported its CPU topology (so we can't scale by thread count). Generous enough
// not to block a legitimate first few servers, low enough to stop runaway abuse.
const byonNodeServerFallbackCap = 16

// byonNodeServerCapReached reports whether a tenant has hit the per-node server
// cap on one of their own BYON nodes, plus the resolved cap (for the message).
// Cap = factor x logical threads (setting byon.max_servers_per_core, default 2);
// when the topology is unknown it falls back to a fixed ceiling. Fail-open on a
// store error so a transient DB blip never blocks a legitimate create.
func (h *ServerHandler) byonNodeServerCapReached(ctx context.Context, node *models.Node) (bool, int) {
	factor := 2
	if v, _ := h.state.Store.GetSetting("byon.max_servers_per_core"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			factor = n
		}
	}

	cores := 0
	if h.state.CPUPinning != nil {
		if topo, _ := h.state.CPUPinning.GetNodeTopology(ctx, node.Token); topo != nil {
			cores = topo.LogicalCount
			if cores == 0 {
				cores = len(topo.Cores)
			}
		}
	}

	capN := factor * cores
	if capN <= 0 {
		capN = byonNodeServerFallbackCap // topology not reported yet
	}

	count, err := h.state.Store.CountServersByNode(node.ID)
	if err != nil {
		return false, capN // fail-open: never block on a store error
	}
	return count >= capN, capN
}

// GetMigrationStatus GET /api/servers/{id}/migration-status
// Returns the orchestrator-owned progress record the migration worker writes
// to dylaris:migration:<uuid>:orchestration, so the panel can poll it while a
// move is in flight. When the key is absent (no migration has ever run, or its
// TTL expired) we return {phase:"none"} so the caller has a stable terminal
// shape to stop polling on. Read-only; owner-or-admin like SetServerAutoMove.
func (h *ServerHandler) GetMigrationStatus(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", 400)
		return
	}
	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", 404)
		return
	}
	if h.state.Redis == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "status": map[string]string{"phase": "none"}})
		return
	}
	key := fmt.Sprintf("dylaris:migration:%s:orchestration", srv.UUID)
	raw, err := h.state.Redis.Get(context.Background(), key).Result()
	if err == redis.Nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "status": map[string]string{"phase": "none"}})
		return
	}
	if err != nil {
		sendJSONError(w, "Failed to read migration status", 500)
		return
	}
	var status map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		sendJSONError(w, "Failed to parse migration status", 500)
		return
	}
	// cancellable: a migration can be cancelled only while it is pre-cutover and
	// still running - the per-server migration lock is held AND the orchestration
	// phase is a pre-cutover in-flight phase ("starting"/"migrating"). Post-cutover
	// ("finalizing"/"done") a cancel is a no-op, so the panel hides the button.
	cancellable := false
	if phase, _ := status["phase"].(string); phase == "starting" || phase == "migrating" {
		if n, lerr := h.state.Redis.Exists(context.Background(), fmt.Sprintf("dylaris:server:%s:migration", srv.UUID)).Result(); lerr == nil && n > 0 {
			cancellable = true
		}
	}
	status["cancellable"] = cancellable
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "status": status})
}

// DeleteSubServer: Delete a single sub-server
func (h *ServerHandler) DeleteSubServer(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}

	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", 400)
		return
	}
	// Same capability gate as DeleteServer, whose comment says "both gates must
	// be open" - and of the two delete handlers in this file, only that one
	// checked both. Both routes carry RequireCap("server.delete"); this one
	// stopped there.
	//
	// A sub-server is a whole instance directory on the node, so deleting one
	// destroys the same kind of data the flag exists to protect, and a server
	// made of sub-servers can be emptied a slot at a time. An operator who
	// deliberately withheld can_delete_servers still handed that out.
	//
	// No panel change is needed: the sub-server delete already renders the
	// response message on failure, and the full-server delete button is
	// likewise shown to everyone with Core doing the refusing.
	userID, _ := r.Context().Value("userID").(string)
	if perms := LoadEffectivePermissions(h.state, userID); !perms.IsAdmin && !perms.CanDeleteServers {
		sendJSONError(w, "Deleting servers requires elevated permissions — contact an administrator", 403)
		return
	}

	subServerName := vars["subServerName"]
	if subServerName == "" {
		sendJSONError(w, "Sub-server name required", 400)
		return
	}
	// The create path sanitizes this; delete did not, yet it is forwarded to the
	// node as a filesystem delete target. Validate the charset (no path metachars).
	if !validate.IsSubServerName(subServerName) {
		sendJSONError(w, "Invalid sub-server name", 400)
		return
	}

	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", 404)
		return
	}

	// Same silence as DeleteServer had, with the same consequence one level down:
	// the slot disappears from the panel while its directory stays on disk,
	// counting against the server's quota with nothing left in the UI pointing at
	// it. An offline node is fine (durable stream); a missing node row, no queue,
	// or an unreachable Redis is not.
	dispatchWarning := ""
	node, nodeErr := h.state.Store.GetNodeByID(srv.NodeID)
	switch {
	case nodeErr != nil:
		dispatchWarning = "the node record is gone"
	case h.state.Queue == nil:
		dispatchWarning = "the command queue is unavailable"
	default:
		configPayload := map[string]interface{}{
			"uuid":            srv.UUID,
			"activeSubServer": subServerName,
		}
		if sendErr := h.state.Queue.SendCommand(context.Background(), node.Token, "delete_sub_server", configPayload, nil); sendErr != nil {
			dispatchWarning = fmt.Sprintf("the delete command could not be queued: %v", sendErr)
		}
	}
	if dispatchWarning != "" {
		log.Printf("DeleteSubServer: sub-server %q of %s removed from the panel but the node was never told to delete it — %s; its files remain on node %d",
			subServerName, srv.UUID, dispatchWarning, srv.NodeID)
	}

	// If deleting the active sub-server, reset to pending_setup AND
	// flip desired_state to "stopped". Without the desired_state flip
	// the Node reconciler races against the delete: it sees the
	// container missing, reads desired_state="online", and recreates
	// the container from the saved config -- which still references
	// the just-deleted sub-server, so Docker auto-creates an empty
	// bind dir and the user's deleted slot resurrects empty. Setting
	// desired_state="stopped" tells the reconciler to leave it alone.
	// The install record outlives the directory otherwise, and the panel would
	// then prefill an edit form for a modpack that is no longer on disk.
	if err := h.state.Store.DeleteSubServerInstall(serverID, subServerName); err != nil {
		log.Printf("sub-server install record cleanup failed for server %d/%s: %v", serverID, subServerName, err)
	}

	wasActive := srv.ActiveSubServer == subServerName
	if wasActive {
		h.state.Store.UpdateServerStatus(serverID, "pending_setup")
		h.state.Store.UpdateServerActiveSubServer(serverID, "")
		h.state.Store.UpdateServerDesiredState(serverID, "stopped")
	}

	actorID, _ := r.Context().Value("userID").(string)
	LogServerAudit(h.state, r, serverID, ServerAuditEventSubServerDeleted, actorID, "", map[string]interface{}{
		"sub_server": subServerName,
		"was_active": wasActive,
	})

	h.state.Events.Publish(r.Context(), "servers.changed", nil)

	resp := map[string]interface{}{"success": true}
	if dispatchWarning != "" {
		resp["warning"] = "The sub-server was removed from the panel, but the node was never told to delete it (" +
			dispatchWarning + "). Its files are still on the node and still count against the disk limit."
	}
	json.NewEncoder(w).Encode(resp)
}

// DeleteServer: Completely delete a server
func (h *ServerHandler) DeleteServer(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}

	// Capability gate. Admins always pass; non-admins need the
	// can_delete_servers flag. This is in addition to the route's
	// RequireCap("server.delete") ownership/grant check — both gates must be open.
	userID, _ := r.Context().Value("userID").(string)
	perms := LoadEffectivePermissions(h.state, userID)
	if !perms.IsAdmin && !perms.CanDeleteServers {
		sendJSONError(w, "Deleting servers requires elevated permissions — contact an administrator", 403)
		return
	}

	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", 400)
		return
	}

	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", 404)
		return
	}

	// Dispatching the node-side delete is the only thing that removes the
	// container and the server's files. SendCommand XADDs to a durable stream, so
	// an offline node is NOT a failure - it picks the command up on reconnect.
	// A failure here means the node row is gone, the queue is unconfigured, or
	// Redis is unreachable, and in all three the data survives on disk while the
	// DB row below disappears: an orphan only the adoption flow can find again,
	// still holding its host port, its RAM and its disk.
	//
	// Deleting anyway is deliberate - refusing would make a server unremovable
	// whenever its node is permanently gone, which is precisely when an operator
	// needs to remove it. But it must not be silent, which it was: no log line,
	// no word in the response.
	dispatchWarning := ""
	node, nodeErr := h.state.Store.GetNodeByID(srv.NodeID)
	switch {
	case nodeErr != nil:
		dispatchWarning = "the node record is gone"
	case h.state.Queue == nil:
		dispatchWarning = "the command queue is unavailable"
	default:
		configPayload := map[string]interface{}{
			"uuid": srv.UUID,
		}
		if sendErr := h.state.Queue.SendCommand(context.Background(), node.Token, "delete", configPayload, nil); sendErr != nil {
			dispatchWarning = fmt.Sprintf("the delete command could not be queued: %v", sendErr)
		}
	}
	if dispatchWarning != "" {
		log.Printf("DeleteServer: server %s (id=%d) removed from the DB but the node was never told to delete it — %s; its container and files remain on node %d",
			srv.UUID, serverID, dispatchWarning, srv.NodeID)
	}

	// Gateway routes are stored independently from the DB row (in Redis +
	// Hub's own DB), so they don't cascade with DeleteServer. Two layers
	// of cleanup, because both have failure modes:
	//
	//   1. Push delete_route to the Hub queue (Gateway.DeleteRoute). This
	//      is the source of truth -- Hub soft-deletes the row in its DB
	//      so SyncData doesn't re-publish the route to Redis on its next
	//      tick. If the Hub queue worker is wedged this step is a no-op
	//      and the route would otherwise come back from Hub's DB.
	//   2. Delete the cache keys directly in Redis. Immediate effect on
	//      traffic routing while we wait for Hub to catch up. Belt-and-
	//      suspenders -- harmless if Hub is healthy (the keys would
	//      vanish from the next SyncData anyway), and the only thing
	//      that works if it isn't.
	//
	// The previous version only did step 1, which is why the route kept
	// reappearing in Redis: when Hub's queue lag bumped into Hub's
	// SyncData cadence, the route was re-published before the delete
	// landed.
	//
	// Delete the authoritative server row FIRST. If it fails we return having
	// touched nothing, so Core and Hub stay consistent — the route cleanup
	// below only runs once the server is actually gone.
	if err := h.state.Store.DeleteServer(serverID); err != nil {
		sendJSONError(w, "Delete failed", 500)
		return
	}

	if h.state.Redis != nil {
		ctx := context.Background()
		routes := services.GetRoutesFromRedis(ctx, h.state.Redis)
		matched := 0
		targetIP := fmt.Sprintf("mc_%s", srv.UUID)
		for _, rt := range routes {
			// Match on either field. Older routes (pre `server_uuid`
			// column being persisted) only have target_ip = "mc_<uuid>";
			// newer ones have both. Match-or means the cleanup catches
			// every shape we've ever written.
			if rt.ServerUUID != srv.UUID && rt.TargetIP != targetIP {
				continue
			}
			matched++
			// Tell Hub to hard-delete the row (source of truth).
			if h.state.Gateway != nil {
				if delErr := h.state.Gateway.DeleteRoute(rt.Domain); delErr != nil {
					log.Printf("DeleteServer: gateway DeleteRoute %s for %s failed: %v", rt.Domain, srv.UUID, delErr)
				}
			}
			// Drop the Redis cache entry immediately so the route stops
			// resolving while Hub processes the queue message.
			pipe := h.state.Redis.Pipeline()
			pipe.Del(ctx, "route:"+rt.Domain)
			pipe.SRem(ctx, "sys:index:routes", rt.Domain)
			if _, err := pipe.Exec(ctx); err != nil {
				log.Printf("DeleteServer: redis cache drop for route %s failed: %v", rt.Domain, err)
			}
		}
		log.Printf("DeleteServer: server %s — cleaned up %d route(s)", srv.UUID, matched)
	}

	h.cleanupDeletedServerKeys(r.Context(), srv.UUID)

	h.state.Events.Publish(r.Context(), "servers.changed", nil)

	resp := map[string]interface{}{"success": true}
	if dispatchWarning != "" {
		resp["warning"] = "The server was removed from the panel, but the node was never told to delete it (" +
			dispatchWarning + "). Its container and files are still on the node and must be cleaned up there."
	}
	json.NewEncoder(w).Encode(resp)
}

// SubServerInstalls GET /api/servers/{id}/installs - how each sub-server of this
// server was installed.
//
// One call for all of them rather than one per sub-server: the setup screen
// switches between them without a round trip, and a per-sub-server endpoint
// would be N requests to fill one form.
//
// A sub-server with NO row is simply absent from the list. That is the honest
// answer for everything installed before this was recorded, and the panel falls
// back to the servers row rather than showing an empty form - "we never wrote
// this down" is not "it was installed with nothing".
func (h *ServerHandler) SubServerInstalls(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}
	serverID, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid server id", http.StatusBadRequest)
		return
	}
	if _, err := h.state.Store.GetServerByID(serverID); err != nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	installs, err := h.state.Store.ListSubServerInstalls(serverID)
	if err != nil {
		sendJSONError(w, "Failed to read installs", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"installs": installs,
	})
}

// UpdateServerRuntime PATCH /api/servers/{id}/runtime - change the Java image
// and the JVM flags WITHOUT reinstalling.
//
// Every settings change used to go through SetupServer, because that was the
// only path that could rebuild a start command. So editing a JVM flag re-ran the
// installer over a live server directory: the operator was asked what to delete
// first, and the answer to "I only wanted a different GC flag" was always
// "nothing", which made the dialog noise in front of a reinstall nobody wanted.
//
// Nothing here touches a file. The node rebuilds the start command from what is
// already on disk and recreates the container around it, leaving the server in
// whatever run state it was in.
func (h *ServerHandler) UpdateServerRuntime(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}
	serverID, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid server id", http.StatusBadRequest)
		return
	}
	var req struct {
		JavaImage     string `json:"javaImage"`
		ExtraJvmFlags string `json:"extraJvmFlags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	if srv.Status == "pending_setup" {
		sendJSONError(w, "Set the server up before changing its runtime", http.StatusBadRequest)
		return
	}
	subName := srv.ActiveSubServer
	if subName == "" {
		sendJSONError(w, "No active sub-server", http.StatusBadRequest)
		return
	}

	// The same resolver the install paths use, so an empty or unknown image
	// falls back to what the server already runs rather than to nothing. An
	// empty image is a container Docker builds with no entrypoint.
	javaImage := resolveJavaImage(req.JavaImage, srv.GameImage)
	if javaImage == "" {
		sendJSONError(w, "javaImage is required", http.StatusBadRequest)
		return
	}
	extraFlags := strings.TrimSpace(req.ExtraJvmFlags)

	// Persist BEFORE dispatching. The node's own saved config is rebuilt from
	// what we send, but the reconciler reads the row - so a dispatch that landed
	// against a row that did not would be undone the next time it ran.
	if err := h.state.Store.UpdateServerRuntime(serverID, javaImage, extraFlags); err != nil {
		sendJSONError(w, "Failed to save the runtime settings", http.StatusInternalServerError)
		return
	}

	if h.state.Queue != nil {
		node, nerr := h.state.Store.GetNodeByID(srv.NodeID)
		if nerr != nil {
			sendJSONError(w, "Node not found", http.StatusNotFound)
			return
		}
		combinedJvmFlags := strings.TrimSpace(defaultJvmFlags + " " + extraFlags)
		payload := map[string]interface{}{
			"uuid":            srv.UUID,
			"ownerId":         srv.OwnerID,
			"activeSubServer": subName,
			"docker": map[string]interface{}{
				"image":         javaImage,
				"ram":           srv.Memory,
				"cpuLimit":      srv.CPULimit,
				"cpusetCpus":    effectiveCpuset(srv.CPUPinningMode, srv.Cpuset, node.CpusetCpus),
				"extraJvmFlags": combinedJvmFlags,
			},
		}
		if err := h.state.Queue.SendCommand(context.Background(), node.Token, "reconfigure", payload, nil); err != nil {
			// Saved but not dispatched. Said out loud rather than swallowed: the
			// panel would otherwise show the new flags against a container still
			// running the old ones, and the difference is invisible until the
			// next restart happens to pick them up.
			log.Printf("UpdateServerRuntime: dispatch failed for server %d: %v", serverID, err)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"warning": "The settings were saved, but the node was not reachable. They apply the next time the server starts.",
			})
			return
		}
	}

	actorID, _ := r.Context().Value("userID").(string)
	LogServerAudit(h.state, r, serverID, ServerAuditEventRuntimeChanged, actorID, "", map[string]interface{}{
		"java_image": javaImage,
		"jvm_flags":  extraFlags,
	})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}
