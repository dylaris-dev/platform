package servers

import (
	"dylaris-pkg/validate"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"dylaris-core/handlers/audit"
	"dylaris-core/handlers/httpx"

	"github.com/gorilla/mux"
)

// DeclareServerLoaderMetadata persists a server's InstallerType (loader) +
// MinecraftVersion (and optional BuildNumber) WITHOUT reinstalling or wiping
// the server. This is the "declare" path for an imported/uploaded server: it
// arrives with a blank MinecraftVersion and a non-loader InstallerType (e.g.
// "upload"), which silently disables the Content tab's loader/version
// auto-filtering. Declaring the real loader + MC version re-enables that
// filtering without installing anything - it only writes the three metadata
// columns (store.UpdateServerLoaderMetadata), never touches game_image/
// start_command/active_sub_server/extra_jvm_flags, never flips server status,
// and never queues a node command. Contrast with SetupServer/ReinstallServer
// in servers_lifecycle.go, which persist the same fields but ALSO dispatch a
// "setup" command that reinstalls the active sub-server.
// PATCH /api/servers/{id}/loader-metadata (gated by RequireCap("server.settings.write") at the route)
func (h *ServerHandler) DeclareServerLoaderMetadata(w http.ResponseWriter, r *http.Request) {
	if h.d.Store == nil {
		httpx.SendJSONError(w, "Database not connected", 503)
		return
	}

	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		httpx.SendJSONError(w, "Invalid server ID", 400)
		return
	}

	var req struct {
		InstallerType    string `json:"installerType"`
		MinecraftVersion string `json:"minecraftVersion"`
		BuildNumber      string `json:"buildNumber"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", 400)
		return
	}

	installerType := strings.ToLower(strings.TrimSpace(req.InstallerType))
	if !validate.IsModrinthLoader(installerType) {
		httpx.SendJSONError(w, "Invalid installerType: must be a known loader (paper, spigot, bukkit, purpur, fabric, forge, quilt, neoforge, velocity, waterfall, bungeecord)", 400)
		return
	}

	minecraftVersion := strings.TrimSpace(req.MinecraftVersion)
	if !validate.IsMcVersion(minecraftVersion) {
		httpx.SendJSONError(w, "Invalid minecraftVersion: expected a Minecraft version like 1.20 or 1.20.4", 400)
		return
	}

	buildNumber := strings.TrimSpace(req.BuildNumber)

	srv, err := h.d.Store.GetServerByID(serverID)
	if err != nil || srv == nil {
		httpx.SendJSONError(w, "Server not found", 404)
		return
	}

	// A repeat declare with no build number must not clobber one set by an
	// earlier declare (or by SetupServer/ReinstallServer) - preserve the
	// server's current value when the request omits it.
	if buildNumber == "" {
		buildNumber = srv.BuildNumber
	}

	if err := h.d.Store.UpdateServerLoaderMetadata(serverID, installerType, minecraftVersion, buildNumber); err != nil {
		httpx.SendJSONError(w, "Failed to update server metadata", 500)
		return
	}

	actorID, _ := r.Context().Value("userID").(string)
	audit.LogServer(h.d.Store, r, serverID, audit.ServerAuditEventLoaderMetadataDeclared, actorID, "", map[string]interface{}{
		"from_installer_type":    srv.InstallerType,
		"to_installer_type":      installerType,
		"from_minecraft_version": srv.MinecraftVersion,
		"to_minecraft_version":   minecraftVersion,
	})

	h.d.Events.Publish(r.Context(), "servers.changed", nil)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Loader and Minecraft version declared",
	})
}
