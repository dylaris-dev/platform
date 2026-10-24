package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"dylaris-pkg/validate"

	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

// Cross-Minecraft-version availability for a modpack build and for a modded
// server. Both endpoints answer the same question and return the same shape,
// which is what lets one panel component render either: "if this moved to
// Minecraft X, which content would still have a version, and which would be
// lost".
//
// The per-version work lives in services/modcompat.go. These handlers only
// build the item list, which is the one thing the two surfaces genuinely differ
// on: a pack build carries the side its author declared, a server has to ask
// Modrinth what sides its mods serve.

// availabilityCache hands the compat service the same metadata cache the
// Modrinth proxy uses, so an operator who moved that cache off the control
// plane moved this one with it. A nil cache is read as "nothing cached" rather
// than as a failure.
func (s *AppState) availabilityCache() services.AvailabilityCache {
	if s == nil || s.Cache == nil {
		return nil
	}
	return s.Cache
}

// compatQuery reads the shared mode/mc query parameters.
func compatQuery(r *http.Request) (mode, specific string) {
	mode = strings.TrimSpace(r.URL.Query().Get("mode"))
	if mode == "" {
		mode = services.CompatModeAllNewer
	}
	return mode, strings.TrimSpace(r.URL.Query().Get("mc"))
}

// resolveTargets turns the request's mode into a concrete target list using
// Modrinth's own release ordering.
func resolveTargets(ctx context.Context, current, mode, specific string) ([]string, error) {
	tags, err := services.FetchGameVersions(ctx)
	if err != nil {
		return nil, err
	}
	return services.SelectCompatTargets(services.ReleaseVersions(tags), current, mode, specific), nil
}

// ServerCompat GET /api/servers/{id}/mods/compat
func (h *ServerModsHandler) ServerCompat(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	srv, ok := h.getServer(serverID)
	if !ok {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	if srv.MinecraftVersion == "" || !validate.IsModrinthLoader(strings.ToLower(srv.InstallerType)) {
		sendJSONError(w, "Declare this server's loader and Minecraft version first, otherwise there is nothing to compare against", http.StatusUnprocessableEntity)
		return
	}
	mods, err := h.state.Store.ListServerMods(serverID, srv.ActiveSubServer)
	if err != nil {
		sendJSONError(w, "Failed to list mods", http.StatusInternalServerError)
		return
	}

	items, err := serverCompatItems(r.Context(), mods)
	if err != nil {
		sendJSONError(w, "Could not read project metadata from Modrinth: "+err.Error(), http.StatusBadGateway)
		return
	}

	mode, specific := compatQuery(r)
	targets, err := resolveTargets(r.Context(), srv.MinecraftVersion, mode, specific)
	if err != nil {
		sendJSONError(w, "Could not read the Minecraft version list from Modrinth: "+err.Error(), http.StatusBadGateway)
		return
	}
	loader := strings.ToLower(srv.InstallerType)
	matrix, err := services.BuildMatrix(r.Context(), services.CompatRequest{
		Items:   items,
		Loader:  loader,
		Current: srv.MinecraftVersion,
		Mode:    mode,
		Targets: targets,
		Cache:   h.state.availabilityCache(),
	})
	if err != nil {
		sendJSONError(w, "Availability check failed: "+err.Error(), http.StatusBadGateway)
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
