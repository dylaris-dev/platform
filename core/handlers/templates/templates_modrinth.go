package templates

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"dylaris-core/handlers"
	"dylaris-core/handlers/servers"
	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

// fetchModrinthVersion is a var so tests can stand in for Modrinth. Paced: a
// user can repeat this at will, and Core is one IP against Modrinth's limit.
var fetchModrinthVersion = services.FetchModrinthVersionPaced

var (
	modrinthVersionID = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
	hexSha1           = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	hexSha512         = regexp.MustCompile(`^[0-9a-fA-F]{128}$`)
)

// maxTemplateRefs caps the Modrinth references of one template (see
// services.MaxTemplateModrinthRefs).
const maxTemplateRefs = services.MaxTemplateModrinthRefs

// templateLoaders is what a template's software may load besides itself, by
// Modrinth loader name. Only used for a warning, never to refuse.
var templateLoaders = map[string][]string{
	"paper":      {"paper", "spigot", "bukkit", "purpur", "folia"},
	"purpur":     {"purpur", "paper", "spigot", "bukkit"},
	"spigot":     {"spigot", "bukkit"},
	"waterfall":  {"waterfall", "bungeecord"},
	"bungeecord": {"bungeecord", "waterfall"},
	"quilt":      {"quilt", "fabric"},
}

// modrinthWarning names a mismatch between the version and the template's
// software or Minecraft version. Users know their stack, so it is not a refusal.
func modrinthWarning(v *services.ModrinthVersion, software, mcVersion string) string {
	loaders := templateLoaders[software]
	if loaders == nil {
		loaders = []string{software}
	}
	var parts []string
	if !slices.ContainsFunc(v.Loaders, func(l string) bool { return slices.Contains(loaders, strings.ToLower(l)) }) {
		parts = append(parts, "is not listed for "+software)
	}
	if mcVersion != "" && !slices.Contains(v.GameVersions, mcVersion) {
		parts = append(parts, "is not listed for Minecraft "+mcVersion)
	}
	if len(parts) == 0 {
		return ""
	}
	return "This version " + strings.Join(parts, " and ") + " on Modrinth. It was added anyway."
}

// AddModrinth POST /api/templates/{id}/modrinth - body {versionId}: keeps that Modrinth version's primary file as a reference in plugins/ or mods/.
// Works under every content policy: a reference stores no bytes. The last
// write at a path wins, so a stored file at the reference path is removed.
func (h *TemplatesHandler) AddModrinth(w http.ResponseWriter, r *http.Request) {
	t, _, _, ok := h.load(w, r, needEdit)
	if !ok || h.refuseSuspended(w, r, t.OwnerID) {
		return
	}
	if !handlers.CapBody(w, r, 4<<10) {
		return
	}
	var p struct {
		VersionID string `json:"versionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil || !modrinthVersionID.MatchString(p.VersionID) {
		handlers.SendJSONError(w, "A Modrinth version id is required", http.StatusBadRequest)
		return
	}
	software := strings.ToLower(strings.TrimSpace(t.Software))
	if software == "" || software == "vanilla" {
		handlers.SendJSONError(w, "Vanilla servers load no mods or plugins. Set the template software to a loader such as paper or fabric first.", http.StatusBadRequest)
		return
	}
	v, err := fetchModrinthVersion(r.Context(), p.VersionID)
	if err != nil {
		if services.ModrinthNotFound(err) {
			handlers.SendJSONError(w, "Modrinth does not know this version", http.StatusNotFound)
			return
		}
		log.Printf("templates: modrinth version %s: %v", p.VersionID, err)
		handlers.SendJSONError(w, "Modrinth did not answer. Try again in a moment.", http.StatusBadGateway)
		return
	}
	if v.ID == "" || v.ProjectID == "" || len(v.Files) == 0 {
		handlers.SendJSONError(w, "This Modrinth version has no file", http.StatusBadRequest)
		return
	}
	f := v.Files[0]
	for _, c := range v.Files {
		if c.Primary {
			f = c
			break
		}
	}
	if !strings.HasPrefix(f.URL, "https://cdn.modrinth.com/") || !hexSha1.MatchString(f.Hashes["sha1"]) || !hexSha512.MatchString(f.Hashes["sha512"]) {
		handlers.SendJSONError(w, "This Modrinth file is not served from the Modrinth CDN with checksums", http.StatusBadRequest)
		return
	}
	// The file name is Modrinth's, so it must stay one path segment.
	name := f.Filename
	if strings.ContainsAny(name, `/\`) || !strings.HasSuffix(strings.ToLower(name), ".jar") {
		handlers.SendJSONError(w, "This Modrinth file is not a plain jar", http.StatusBadRequest)
		return
	}
	rel, ok := services.TemplateRelPath(servers.DefaultTargetDirForLoader(software) + "/" + name)
	if !ok || strings.Count(rel, "/") != 1 {
		handlers.SendJSONError(w, "This Modrinth file name cannot be used as a template path", http.StatusBadRequest)
		return
	}
	fail := func(what string, err error) {
		log.Printf("templates: modrinth %s %d/%s: %v", what, t.ID, rel, err)
		handlers.SendJSONError(w, "Could not store the Modrinth reference", http.StatusInternalServerError)
	}
	refs, err := h.state.Store.ListTemplateModrinth(t.ID)
	if err != nil {
		fail("list", err)
		return
	}
	// Another version of the same project is replaced, not kept beside it:
	// two builds of one plugin would both load.
	var older []int
	kept := 0
	for _, old := range refs {
		switch {
		case old.Path == rel:
		case old.ProjectID == v.ProjectID:
			older = append(older, old.ID)
		default:
			kept++
		}
	}
	if kept >= maxTemplateRefs {
		handlers.SendJSONError(w, "This template already has "+strconv.Itoa(maxTemplateRefs)+" Modrinth references, the most one template may keep. Remove one first.", http.StatusConflict)
		return
	}
	ref := models.TemplateModrinthRef{
		TemplateID: t.ID, Path: rel, ProjectID: v.ProjectID, VersionID: v.ID, FileName: name,
		URL: f.URL, Sha1: strings.ToLower(f.Hashes["sha1"]), Sha512: strings.ToLower(f.Hashes["sha512"]), SizeBytes: f.Size,
	}
	// The reference first: if a later step fails, the template still has the
	// jar at that path, once as a reference and maybe once stored.
	if err := h.state.Store.UpsertTemplateModrinth(&ref); err != nil {
		fail("upsert", err)
		return
	}
	for _, id := range older {
		if _, err := h.state.Store.DeleteTemplateModrinth(t.ID, id); err != nil {
			fail("replace", err)
			return
		}
	}
	files, err := h.state.TemplateList(r.Context(), t)
	if err != nil {
		fail("list files", err)
		return
	}
	for _, sf := range files {
		if sf.Name == rel {
			if err := h.state.TemplateDelete(r.Context(), t, rel); err != nil {
				fail("delete stored", err)
				return
			}
			break
		}
	}
	if err := refreshSize(r.Context(), h.state, t); err != nil {
		log.Printf("templates: size of %d: %v", t.ID, err)
	}
	body := map[string]interface{}{"success": true, "reference": ref, "sizeBytes": t.SizeBytes}
	if warn := modrinthWarning(v, software, strings.TrimSpace(t.McVersion)); warn != "" {
		body["warning"] = warn
	}
	writeJSON(w, http.StatusOK, body)
}

// DeleteModrinth DELETE /api/templates/{id}/modrinth/{refId} - removes one Modrinth reference of this template.
func (h *TemplatesHandler) DeleteModrinth(w http.ResponseWriter, r *http.Request) {
	t, _, _, ok := h.load(w, r, needEdit)
	if !ok {
		return
	}
	refID, err := strconv.Atoi(mux.Vars(r)["refId"])
	if err != nil || refID <= 0 {
		handlers.SendJSONError(w, "Reference not found", http.StatusNotFound)
		return
	}
	found, err := h.state.Store.DeleteTemplateModrinth(t.ID, refID)
	if err != nil {
		log.Printf("templates: delete modrinth %d/%d: %v", t.ID, refID, err)
		handlers.SendJSONError(w, "Could not remove the reference", http.StatusInternalServerError)
		return
	}
	if !found {
		handlers.SendJSONError(w, "Reference not found", http.StatusNotFound)
		return
	}
	h.written(w, r, t, nil)
}
