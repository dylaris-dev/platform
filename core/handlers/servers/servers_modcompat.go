package servers

import (
	"context"
	"dylaris-pkg/validate"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"dylaris-core/handlers"

	"github.com/gorilla/mux"

	"dylaris-core/models"
	"dylaris-core/services"
)

// ServerCompat GET /api/servers/{id}/mods/compat
func (h *ServerModsHandler) ServerCompat(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	srv, ok := h.getServer(serverID)
	if !ok {
		handlers.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	if srv.MinecraftVersion == "" || !validate.IsModrinthLoader(strings.ToLower(srv.InstallerType)) {
		handlers.SendJSONError(w, "Declare this server's loader and Minecraft version first, otherwise there is nothing to compare against", http.StatusUnprocessableEntity)
		return
	}
	mods, err := h.state.Store.ListServerMods(serverID, srv.ActiveSubServer)
	if err != nil {
		handlers.SendJSONError(w, "Failed to list mods", http.StatusInternalServerError)
		return
	}

	items, err := serverCompatItems(r.Context(), mods)
	if err != nil {
		handlers.SendJSONError(w, "Could not read project metadata from Modrinth: "+err.Error(), http.StatusBadGateway)
		return
	}

	mode, specific := handlers.CompatQuery(r)
	targets, err := handlers.ResolveTargets(r.Context(), srv.MinecraftVersion, mode, specific)
	if err != nil {
		handlers.SendJSONError(w, "Could not read the Minecraft version list from Modrinth: "+err.Error(), http.StatusBadGateway)
		return
	}
	loader := strings.ToLower(srv.InstallerType)
	matrix, err := services.BuildMatrix(r.Context(), services.CompatRequest{
		Items:   items,
		Loader:  loader,
		Current: srv.MinecraftVersion,
		Mode:    mode,
		Targets: targets,
		Cache:   h.state.AvailabilityCache(),
	})
	if err != nil {
		handlers.SendJSONError(w, "Availability check failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "matrix": matrix})
}

// serverCompatItems turns installed server mods into compat items. Unlike a
// pack build, a server has no author-declared side, so the side comes from each
// project's own client_side/server_side declaration. One batched Modrinth call
// covers the whole list.
//
// A project the batch does not answer for falls to the zero-value meta, and
// SideFromModrinth reads an empty declaration as both-sided. That is the severe
// reading on purpose: guessing single-sided would turn a red row orange and
// understate what a migration costs.
func serverCompatItems(ctx context.Context, mods []models.ServerMod) ([]services.CompatItem, error) {
	ids := make([]string, 0, len(mods))
	seen := map[string]bool{}
	for _, m := range mods {
		if m.ModrinthProjectID == "" || seen[m.ModrinthProjectID] {
			continue
		}
		seen[m.ModrinthProjectID] = true
		ids = append(ids, m.ModrinthProjectID)
	}
	meta := map[string]services.ModrinthProjectMeta{}
	if len(ids) > 0 {
		var err error
		meta, err = services.FetchProjects(ctx, ids)
		if err != nil {
			return nil, err
		}
	}

	items := make([]services.CompatItem, 0, len(mods))
	for _, m := range mods {
		p := meta[m.ModrinthProjectID]
		title := m.Title
		if title == "" {
			title = p.Title
		}
		slug := m.ModrinthProjectSlug
		if slug == "" {
			slug = p.Slug
		}
		items = append(items, services.CompatItem{
			Key:              strconv.Itoa(m.ID),
			ProjectID:        m.ModrinthProjectID,
			Title:            title,
			Slug:             slug,
			Side:             services.SideFromModrinth(p.ClientSide, p.ServerSide),
			CurrentVersionID: m.ModrinthVersionID,
		})
	}
	return items, nil
}
