package servers

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dylaris-core/handlers"

	"dylaris-core/services"

	"github.com/gorilla/mux"
)

// planModInstall is a variable so tests can stand in for Modrinth.
var planModInstall = services.PlanModInstall

// modPlanWait bounds one plan. The walk queues behind the shared Modrinth
// pacing, so a cold fifty-node graph can take a few seconds, not more.
const modPlanWait = 30 * time.Second

// Plan POST /api/servers/{id}/mods/plan - what installing one Modrinth version
// takes on this server: the required dependencies, dependency-first, that are
// not installed yet, the ones nothing fits, and installed projects it is
// incompatible with. It installs nothing and writes nothing.
func (h *ServerModsHandler) Plan(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	srv, ok := h.getServer(serverID)
	if !ok {
		handlers.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	var req struct {
		VersionID string `json:"versionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.VersionID = strings.TrimSpace(req.VersionID)
	if req.VersionID == "" {
		handlers.SendJSONError(w, "versionId required", http.StatusBadRequest)
		return
	}
	if !handlers.ModFitKnown(srv.InstallerType, srv.MinecraftVersion) {
		handlers.SendJSONError(w, "Declare the server's loader and Minecraft version first", http.StatusConflict)
		return
	}
	mods, err := h.state.Store.ListServerMods(serverID, srv.ActiveSubServer)
	if err != nil {
		handlers.SendJSONError(w, "Failed to list mods", http.StatusInternalServerError)
		return
	}
	// A failed row's jar is not there, so its project is still something a
	// dependency has to bring. The version goes along so the planner can tell
	// an installed build that does not fit the server from one that does.
	installed := map[string]string{}
	for _, m := range mods {
		if m.ModrinthProjectID != "" && (m.Status == "installed" || m.Status == "installing") {
			installed[m.ModrinthProjectID] = m.ModrinthVersionID
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), modPlanWait)
	defer cancel()
	plan, err := planModInstall(ctx, req.VersionID, srv.InstallerType, srv.MinecraftVersion, installed)
	if err != nil {
		var fit *services.ModFitError
		var upstream *services.ModrinthHTTPError
		switch {
		case errors.As(err, &fit):
			handlers.SendJSONError(w, fit.Reason, http.StatusConflict)
		case services.ModrinthNotFound(err):
			handlers.SendJSONError(w, "Modrinth has no version "+req.VersionID, http.StatusBadRequest)
		case errors.As(err, &upstream) && upstream.Status == http.StatusTooManyRequests:
			handlers.SendCooldown(w, "Modrinth", handlers.UpstreamCooldownDefault)
		default:
			log.Printf("mod plan: server %d version %s: %v", serverID, req.VersionID, err)
			handlers.SendJSONError(w, "Could not reach Modrinth to work out the dependencies. Try again in a moment.", http.StatusBadGateway)
		}
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":      true,
		"install":      plan.Install,
		"unresolved":   plan.Unresolved,
		"incompatible": plan.Incompatible,
	})
}
