package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

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
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	var req struct {
		VersionID string `json:"versionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.VersionID = strings.TrimSpace(req.VersionID)
	if req.VersionID == "" {
		sendJSONError(w, "versionId required", http.StatusBadRequest)
		return
	}
	if !modFitKnown(srv.InstallerType, srv.MinecraftVersion) {
		sendJSONError(w, "Declare the server's loader and Minecraft version first", http.StatusConflict)
		return
	}
	mods, err := h.state.Store.ListServerMods(serverID, srv.ActiveSubServer)
	if err != nil {
		sendJSONError(w, "Failed to list mods", http.StatusInternalServerError)
		return
	}
	// A failed row's jar is not there, so its project is still something a
	// dependency has to bring.
	installed := map[string]bool{}
	for _, m := range mods {
		if m.ModrinthProjectID != "" && (m.Status == "installed" || m.Status == "installing") {
			installed[m.ModrinthProjectID] = true
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
			sendJSONError(w, fit.Reason, http.StatusConflict)
		case services.ModrinthNotFound(err):
			sendJSONError(w, "Modrinth has no version "+req.VersionID, http.StatusBadRequest)
		case errors.As(err, &upstream) && upstream.Status == http.StatusTooManyRequests:
			sendCooldown(w, "Modrinth", upstreamCooldownDefault)
		default:
			log.Printf("mod plan: server %d version %s: %v", serverID, req.VersionID, err)
			sendJSONError(w, "Could not reach Modrinth to work out the dependencies. Try again in a moment.", http.StatusBadGateway)
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
