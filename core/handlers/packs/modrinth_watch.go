package packs

import (
	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"
	"encoding/json"
	"github.com/gorilla/mux"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

var modrinthIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Favorites is the authenticated caller's own public Modrinth bookmarks.
func (h *PacksHandler) Favorites(w http.ResponseWriter, r *http.Request) {
	user, _ := r.Context().Value("userID").(string)
	if user == "" {
		handlers.SendJSONError(w, "Unauthenticated", 401)
		return
	}
	if r.Method == http.MethodGet {
		rows, err := h.state.Store.ListModrinthFavorites(r.Context(), user)
		if err != nil {
			handlers.SendJSONError(w, "Could not load favorites", 500)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "favorites": rows})
		return
	}
	var req struct {
		ProjectID string `json:"projectId"`
		Remove    bool   `json:"remove"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || !modrinthIDPattern.MatchString(req.ProjectID) {
		handlers.SendJSONError(w, "Valid projectId required", 400)
		return
	}
	f := models.ModrinthFavorite{ProjectID: req.ProjectID}
	if !req.Remove {
		projects, err := services.FetchProjects(r.Context(), []string{req.ProjectID})
		if err != nil {
			handlers.SendJSONError(w, "Modrinth project lookup failed", 502)
			return
		}
		p, ok := projects[req.ProjectID]
		if !ok {
			handlers.SendJSONError(w, "Project not found", 404)
			return
		}
		f.Title = p.Title
	}
	if err := h.state.Store.SetModrinthFavorite(r.Context(), user, f, req.Remove); err != nil {
		handlers.SendJSONError(w, "Could not save favorite", 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// Watches lists or creates a daily, user-owned release availability alert.
func (h *PacksHandler) Watches(w http.ResponseWriter, r *http.Request) {
	user, _ := r.Context().Value("userID").(string)
	if user == "" {
		handlers.SendJSONError(w, "Unauthenticated", 401)
		return
	}
	if r.Method == http.MethodGet {
		rows, err := h.state.Store.ListModrinthWatches(r.Context(), user, false)
		if err != nil {
			handlers.SendJSONError(w, "Could not load alerts", 500)
			return
		}
		visible := []models.ModrinthWatch{}
		for _, row := range rows {
			if row.BuildID != nil {
				if _, ok := h.packAccess(r, row.PackID, packRead); !ok {
					continue
				}
			}
			visible = append(visible, row)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "watches": visible})
		return
	}
	var req struct {
		ProjectID string   `json:"projectId"`
		PackID    int      `json:"packId"`
		BuildID   int      `json:"buildId"`
		Loader    string   `json:"loader"`
		Targets   []string `json:"targets"`
		Future    bool     `json:"future"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid request", 400)
		return
	}
	if req.Loader != "fabric" && req.Loader != "forge" && req.Loader != "neoforge" && req.Loader != "quilt" {
		handlers.SendJSONError(w, "Choose a mod loader", 400)
		return
	}
	if (!req.Future && len(req.Targets) == 0) || len(req.Targets) > 32 {
		handlers.SendJSONError(w, "Choose up to 32 target versions or future releases", 400)
		return
	}
	all, err := services.FetchGameVersions(r.Context())
	if err != nil {
		handlers.SendJSONError(w, "Could not load Minecraft releases", 502)
		return
	}
	releases := services.ReleaseVersions(all)
	valid := map[string]bool{}
	for _, v := range releases {
		valid[v] = true
	}
	targets := []string{}
	for _, v := range req.Targets {
		if !valid[v] {
			handlers.SendJSONError(w, "Target must be a stable Minecraft release", 400)
			return
		}
		if valid[v] {
			targets = append(targets, v)
			valid[v] = false
		}
	}
	watch := models.ModrinthWatch{UserID: user, Loader: req.Loader, Targets: targets, Future: req.Future, Projects: []models.ModrinthWatchProject{}}
	if len(releases) > 0 {
		watch.Current = releases[0]
	}
	if req.BuildID > 0 {
		pack, ok := h.packAccess(r, req.PackID, packRead)
		if !ok {
			handlers.SendJSONError(w, "Forbidden", 403)
			return
		}
		build, err := h.state.Store.GetPackBuild(req.BuildID)
		if err != nil || build == nil || build.PackID != pack.ID {
			handlers.SendJSONError(w, "Build not found", 404)
			return
		}
		watch.BuildID = &build.ID
		watch.PackID = pack.ID
		watch.Title = pack.InternalName + " / " + build.VersionString
		content, err := h.state.Store.ListBuildContent(build.ID)
		if err != nil {
			handlers.SendJSONError(w, "Could not load source mods", 500)
			return
		}
		seen := map[string]bool{}
		for _, e := range content {
			if e.ContentType != models.ContentTypeMod {
				continue
			}
			if e.ModrinthProjectID == "" {
				watch.Unlinked++
				continue
			}
			if !seen[e.ModrinthProjectID] {
				watch.Projects = append(watch.Projects, models.ModrinthWatchProject{ProjectID: e.ModrinthProjectID, Title: e.PrettyName})
				seen[e.ModrinthProjectID] = true
			}
		}
	} else {
		if !modrinthIDPattern.MatchString(req.ProjectID) {
			handlers.SendJSONError(w, "Valid projectId required", 400)
			return
		}
		projects, err := services.FetchProjects(r.Context(), []string{req.ProjectID})
		if err != nil {
			handlers.SendJSONError(w, "Project lookup failed", 502)
			return
		}
		project, ok := projects[req.ProjectID]
		if !ok {
			handlers.SendJSONError(w, "Project not found", 404)
			return
		}
		watch.Title = project.Title
		watch.Projects = append(watch.Projects, models.ModrinthWatchProject{ProjectID: req.ProjectID, Title: project.Title})
	}
	if len(watch.Projects) == 0 || len(watch.Projects) > 512 {
		handlers.SendJSONError(w, "An alert needs between 1 and 512 linked mods", 422)
		return
	}
	watch.Title = strings.TrimSpace(watch.Title)
	if err := h.state.Store.CreateModrinthWatch(r.Context(), &watch); err != nil {
		handlers.SendJSONError(w, "Could not create alert; check your 100-alert limit", 409)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "watch": watch})
}

// DeleteWatch removes only a watch belonging to the signed-in caller.
func (h *PacksHandler) DeleteWatch(w http.ResponseWriter, r *http.Request) {
	user, _ := r.Context().Value("userID").(string)
	if user == "" {
		handlers.SendJSONError(w, "Unauthenticated", 401)
		return
	}
	id, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil || id <= 0 {
		handlers.SendJSONError(w, "Invalid id", 400)
		return
	}
	if err = h.state.Store.DeleteModrinthWatch(r.Context(), id, user); err != nil {
		handlers.SendJSONError(w, "Could not remove alert", 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
