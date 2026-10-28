package packs

// The pack's Modrinth project page, edited from the panel: what the website's
// project settings offer for a modpack (title, summary, description, tags,
// links, license, icon, environment), Modrinth's own publishing checklist,
// and "Submit for review". Same access rule and credential as publishing:
// the pack's Modrinth team, each member acting through their OWN token.
// Nothing is stored here except the summary, which mirrors into the local
// pack so the panel shows what Modrinth shows.

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

// modrinthEnvironments are the version environments offered for a modpack.
var modrinthEnvironments = map[string]bool{"client_and_server": true, "client_only": true}

const defaultModpackEnvironment = "client_and_server"

// Modrinth's own icon limit.
const maxModrinthIconBytes = 256 << 10

type modrinthPageResponse struct {
	Success     bool                          `json:"success"`
	Project     *services.ModrinthProjectPage `json:"project"`
	Nags        []services.ModrinthNag        `json:"nags"`
	Environment string                        `json:"environment"`
	Message     string                        `json:"message,omitempty"`
}

// modrinthPageClient resolves the pack, checks edit access and loads the
// owner's credential. It writes the error itself and returns ok=false.
func (h *PacksHandler) modrinthPageClient(w http.ResponseWriter, r *http.Request, need packNeed) (*models.Pack, *services.ModrinthClient, bool) {
	packID, _ := strconv.Atoi(mux.Vars(r)["id"])
	pack, ok := h.packAccess(r, packID, need)
	if !ok || pack == nil {
		handlers.SendJSONError(w, "Forbidden", http.StatusForbidden)
		return nil, nil, false
	}
	if pack.ModrinthProjectID == "" {
		handlers.SendJSONError(w, "This pack has no Modrinth project yet. Publish a build first; that creates it.", http.StatusConflict)
		return nil, nil, false
	}
	// The caller's own token: Modrinth checks their team rights on every call,
	// the same rights the panel just checked.
	_, mc, ok := h.callerModrinth(w, r)
	if !ok {
		return nil, nil, false
	}
	return pack, mc, true
}

// writeModrinthPage answers with the project as Modrinth now has it.
func (h *PacksHandler) writeModrinthPage(w http.ResponseWriter, r *http.Request, pack *models.Pack, mc *services.ModrinthClient, message string) {
	page, err := mc.GetProjectPage(r.Context(), pack.ModrinthProjectID)
	if err != nil {
		handlers.SendJSONError(w, "Could not read the Modrinth project: "+err.Error(), http.StatusBadGateway)
		return
	}
	nags, err := mc.ValidateProject(r.Context(), pack.ModrinthProjectID)
	if err != nil {
		// The page is still useful without the checklist; say so rather than fail.
		log.Printf("packs: modrinth checklist for pack %d unavailable: %v", pack.ID, err)
		nags = nil
		if message == "" {
			message = "Modrinth's checklist could not be loaded; try Refresh."
		}
	}
	env := ""
	if len(page.Versions) > 0 {
		env, _ = mc.VersionEnvironment(r.Context(), page.Versions[len(page.Versions)-1])
	}
	if nags == nil {
		nags = []services.ModrinthNag{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modrinthPageResponse{Success: true, Project: page, Nags: nags, Environment: env, Message: message})
}

// GetModrinthPage GET /api/packs/{id}/modrinth
func (h *PacksHandler) GetModrinthPage(w http.ResponseWriter, r *http.Request) {
	pack, mc, ok := h.modrinthPageClient(w, r, packRead)
	if !ok {
		return
	}
	h.writeModrinthPage(w, r, pack, mc, "")
}

type modrinthPageEdit struct {
	Title                *string   `json:"title"`
	Description          *string   `json:"description"`
	Body                 *string   `json:"body"`
	Categories           *[]string `json:"categories"`
	AdditionalCategories *[]string `json:"additionalCategories"`
	LicenseID            *string   `json:"licenseId"`
	LicenseURL           *string   `json:"licenseUrl"`
	SourceURL            *string   `json:"sourceUrl"`
	IssuesURL            *string   `json:"issuesUrl"`
	WikiURL              *string   `json:"wikiUrl"`
	DiscordURL           *string   `json:"discordUrl"`
	Environment          *string   `json:"environment"`
}

// modrinthPageFields turns an edit into Modrinth's v2 fields, refusing what
// Modrinth would refuse anyway, with a message a person can act on.
func modrinthPageFields(e modrinthPageEdit) (map[string]interface{}, string) {
	f := map[string]interface{}{}
	text := func(key string, v *string, min, max int, label string) string {
		if v == nil {
			return ""
		}
		s := strings.TrimSpace(*v)
		if n := len([]rune(s)); n < min || n > max {
			return label + " must be " + strconv.Itoa(min) + " to " + strconv.Itoa(max) + " characters."
		}
		f[key] = s
		return ""
	}
	for _, msg := range []string{
		text("title", e.Title, 3, 64, "The name"),
		text("description", e.Description, 3, 256, "The summary"),
		text("body", e.Body, 0, 65536, "The description"),
	} {
		if msg != "" {
			return nil, msg
		}
	}
	link := func(key string, v *string) string {
		if v == nil {
			return ""
		}
		s := strings.TrimSpace(*v)
		if s == "" {
			f[key] = nil // clears the link
			return ""
		}
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return "Links must be full http(s) addresses."
		}
		f[key] = s
		return ""
	}
	for _, msg := range []string{link("source_url", e.SourceURL), link("issues_url", e.IssuesURL), link("wiki_url", e.WikiURL), link("discord_url", e.DiscordURL), link("license_url", e.LicenseURL)} {
		if msg != "" {
			return nil, msg
		}
	}
	tags := func(key string, v *[]string) {
		if v == nil {
			return
		}
		out := []string{}
		for _, t := range *v {
			if t = strings.TrimSpace(t); t != "" && len(out) < 32 {
				out = append(out, t)
			}
		}
		f[key] = out
	}
	tags("categories", e.Categories)
	tags("additional_categories", e.AdditionalCategories)
	if e.LicenseID != nil {
		id := strings.TrimSpace(*e.LicenseID)
		if id == "" || len(id) > 128 {
			return nil, "Choose a license."
		}
		f["license_id"] = id
	}
	if e.Environment != nil && !modrinthEnvironments[*e.Environment] {
		return nil, "Unknown environment."
	}
	return f, ""
}

// EditModrinthPage PATCH /api/packs/{id}/modrinth
func (h *PacksHandler) EditModrinthPage(w http.ResponseWriter, r *http.Request) {
	pack, mc, ok := h.modrinthPageClient(w, r, packEditPage)
	if !ok {
		return
	}
	var edit modrinthPageEdit
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&edit); err != nil {
		handlers.SendJSONError(w, "Invalid request", http.StatusBadRequest)
		return
	}
	fields, msg := modrinthPageFields(edit)
	if msg != "" {
		handlers.SendJSONError(w, msg, http.StatusBadRequest)
		return
	}
	if len(fields) > 0 {
		if err := mc.EditProject(r.Context(), pack.ModrinthProjectID, fields); err != nil {
			handlers.SendJSONError(w, "Modrinth refused the change: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	if edit.Environment != nil {
		page, err := mc.GetProjectPage(r.Context(), pack.ModrinthProjectID)
		if err != nil {
			handlers.SendJSONError(w, "Could not read the Modrinth project: "+err.Error(), http.StatusBadGateway)
			return
		}
		for _, v := range page.Versions {
			if err := mc.SetVersionEnvironment(r.Context(), v, *edit.Environment); err != nil {
				handlers.SendJSONError(w, "Modrinth refused the environment: "+err.Error(), http.StatusBadGateway)
				return
			}
		}
	}
	// The panel shows the local summary; keep it what Modrinth shows.
	if s, ok := fields["description"].(string); ok && s != pack.Summary {
		pack.Summary = s
		if err := h.state.Store.UpdatePack(pack); err != nil {
			log.Printf("packs: summary of pack %d not mirrored: %v", pack.ID, err)
		}
	}
	publishPackEvent(r, h.state, "packs.changed", pack.ID, map[string]interface{}{})
	h.writeModrinthPage(w, r, pack, mc, "Saved to Modrinth.")
}

// UploadModrinthIcon POST /api/packs/{id}/modrinth/icon (multipart "icon")
func (h *PacksHandler) UploadModrinthIcon(w http.ResponseWriter, r *http.Request) {
	pack, mc, ok := h.modrinthPageClient(w, r, packEditPage)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxModrinthIconBytes+64<<10)
	file, _, err := r.FormFile("icon")
	if err != nil {
		handlers.SendJSONError(w, "Choose an image of at most 256 KB.", http.StatusBadRequest)
		return
	}
	defer file.Close()
	img, err := io.ReadAll(io.LimitReader(file, maxModrinthIconBytes+1))
	if err != nil || len(img) == 0 || len(img) > maxModrinthIconBytes {
		handlers.SendJSONError(w, "Choose an image of at most 256 KB.", http.StatusBadRequest)
		return
	}
	// The bytes decide the type, not the file name.
	ext := map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp", "image/gif": "gif"}
	ct := http.DetectContentType(img)
	e, known := ext[ct]
	if !known {
		handlers.SendJSONError(w, "The icon must be a PNG, JPEG, WebP or GIF image.", http.StatusBadRequest)
		return
	}
	if err := mc.SetProjectIcon(r.Context(), pack.ModrinthProjectID, e, ct, img); err != nil {
		handlers.SendJSONError(w, "Modrinth refused the icon: "+err.Error(), http.StatusBadGateway)
		return
	}
	h.writeModrinthPage(w, r, pack, mc, "Icon updated.")
}

// SubmitModrinthReview POST /api/packs/{id}/modrinth/submit
func (h *PacksHandler) SubmitModrinthReview(w http.ResponseWriter, r *http.Request) {
	pack, mc, ok := h.modrinthPageClient(w, r, packEditPage)
	if !ok {
		return
	}
	nags, err := mc.ValidateProject(r.Context(), pack.ModrinthProjectID)
	if err == nil {
		for _, n := range nags {
			if n.Severity == "required" {
				handlers.SendJSONError(w, "Complete the required items of the checklist first.", http.StatusConflict)
				return
			}
		}
	}
	if err := mc.SubmitForReview(r.Context(), pack.ModrinthProjectID); err != nil {
		handlers.SendJSONError(w, "Modrinth did not accept the submission: "+err.Error(), http.StatusBadGateway)
		return
	}
	h.writeModrinthPage(w, r, pack, mc, "Submitted for review. Modrinth's moderators decide; this usually takes a few days.")
}

// setUploadedVersionEnvironment gives a freshly uploaded version the
// environment the project's earlier versions have, or client and server for
// the first one. A failure is reported, not fatal: the version is up.
func setUploadedVersionEnvironment(ctx context.Context, mc *services.ModrinthClient, projectVersions []string, versionID string) error {
	env := defaultModpackEnvironment
	for i := len(projectVersions) - 1; i >= 0; i-- {
		if projectVersions[i] == versionID {
			continue
		}
		if prev, err := mc.VersionEnvironment(ctx, projectVersions[i]); err == nil && modrinthEnvironments[prev] {
			env = prev
		}
		break
	}
	return mc.SetVersionEnvironment(ctx, versionID, env)
}
