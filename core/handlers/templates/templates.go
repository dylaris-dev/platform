package templates

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

// TemplatesHandler serves /api/templates. Every route sits behind
// RequireTemplatesAudience, which is what keeps non-admins out while the
// feature is off or admin-only; the per-template rules below do not repeat it.
type TemplatesHandler struct {
	state *handlers.AppState
}

func NewTemplatesHandler(state *handlers.AppState) *TemplatesHandler {
	return &TemplatesHandler{state: state}
}

const maxTemplateNameLen = 64

// viewer resolves who is asking; see AppState.TemplateViewer.
func (h *TemplatesHandler) viewer(r *http.Request, t *models.Template) (services.TemplateViewer, error) {
	return h.state.TemplateViewer(r, t)
}

func (h *TemplatesHandler) settings(r *http.Request) services.TemplateSettings {
	return services.LoadTemplateSettings(r.Context(), h.state.FeatureFlags)
}

type templateNeed int

const (
	needSee templateNeed = iota
	needEdit
	needShare
	needDelete
)

// load resolves {id} and checks need. A template the caller cannot see is a
// 404 like a missing one, so ids cannot be probed; seen but not allowed is 403.
func (h *TemplatesHandler) load(w http.ResponseWriter, r *http.Request, need templateNeed) (*models.Template, services.TemplateViewer, services.TemplateSettings, bool) {
	s := h.settings(r)
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil || id <= 0 {
		handlers.SendJSONError(w, "Template not found", http.StatusNotFound)
		return nil, services.TemplateViewer{}, s, false
	}
	t, err := h.state.Store.GetTemplate(id)
	if err != nil {
		log.Printf("templates: get %d: %v", id, err)
		handlers.SendJSONError(w, "Could not load the template", http.StatusInternalServerError)
		return nil, services.TemplateViewer{}, s, false
	}
	if t == nil {
		handlers.SendJSONError(w, "Template not found", http.StatusNotFound)
		return nil, services.TemplateViewer{}, s, false
	}
	v, err := h.viewer(r, t)
	if err != nil {
		log.Printf("templates: viewer for %d: %v", id, err)
		handlers.SendJSONError(w, "Could not load the template", http.StatusInternalServerError)
		return nil, v, s, false
	}
	if !services.TemplateCanSee(*t, v, s) {
		handlers.SendJSONError(w, "Template not found", http.StatusNotFound)
		return nil, v, s, false
	}
	allowed := true
	switch need {
	case needEdit:
		allowed = services.TemplateCanEdit(*t, v)
	case needShare:
		allowed = services.TemplateCanShare(*t, v)
	case needDelete:
		// An edit share lets someone change the files, not remove the owner's template.
		allowed = services.TemplateCanEdit(*t, v) && (v.IsAdmin || t.OwnerID == v.UserID)
	}
	if !allowed {
		handlers.SendJSONError(w, "You may not change this template", http.StatusForbidden)
		return nil, v, s, false
	}
	return t, v, s, true
}

type templateItem struct {
	models.Template
	CanEdit    bool   `json:"canEdit"`
	CanShare   bool   `json:"canShare"`
	ShareLevel string `json:"shareLevel"`
}

func item(t models.Template, v services.TemplateViewer) templateItem {
	return templateItem{Template: t, CanEdit: services.TemplateCanEdit(t, v), CanShare: services.TemplateCanShare(t, v), ShareLevel: v.ShareLevel}
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// List GET /api/templates - the templates the caller may see, each with what
// they may do with it, plus the limits the panel shows sizes against.
func (h *TemplatesHandler) List(w http.ResponseWriter, r *http.Request) {
	s := h.settings(r)
	v, _ := h.viewer(r, nil)
	list, err := h.state.Store.ListTemplatesVisibleTo(v.UserID, v.IsAdmin || v.HasAdminRead, v.IsAdmin || s.PlatformForUsers)
	if err != nil {
		log.Printf("templates: list for %s: %v", v.UserID, err)
		handlers.SendJSONError(w, "Could not list templates", http.StatusInternalServerError)
		return
	}
	out := []templateItem{}
	for _, t := range list {
		tv := v
		if t.OwnerID != v.UserID {
			if tv.ShareLevel, err = h.state.Store.TemplateShareLevel(t.ID, v.UserID); err != nil {
				log.Printf("templates: share level %d/%s: %v", t.ID, v.UserID, err)
				handlers.SendJSONError(w, "Could not list templates", http.StatusInternalServerError)
				return
			}
		}
		if services.TemplateCanSee(t, tv, s) {
			out = append(out, item(t, tv))
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"templates":    out,
		"maxSizeBytes": s.MaxSizeBytes,
		"maxPerUser":   s.MaxPerUser,
	})
}

// Get GET /api/templates/{id}
func (h *TemplatesHandler) Get(w http.ResponseWriter, r *http.Request) {
	t, v, _, ok := h.load(w, r, needSee)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, item(*t, v))
}

type templateMeta struct {
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	Visibility      string `json:"visibility"`
	Software        string `json:"software"`
	McVersion       string `json:"mcVersion"`
	SoftwareVersion string `json:"softwareVersion"`
}

func validName(n string) bool {
	if n == "" || len(n) > maxTemplateNameLen {
		return false
	}
	for _, r := range n {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// validField bounds the free-text software/version fields.
func validField(s string) bool {
	if len(s) > 64 {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validVisibility(v string) bool { return v == "personal" || v == "admin" || v == "platform" }

// newTemplate validates meta and the create rules (visibility, count limit)
// and inserts the row. It writes the error response itself.
func (h *TemplatesHandler) newTemplate(w http.ResponseWriter, r *http.Request, m templateMeta) (*models.Template, services.TemplateSettings, bool) {
	s := h.settings(r)
	m.Name = strings.TrimSpace(m.Name)
	if m.Visibility == "" {
		m.Visibility = "personal"
	}
	if !validName(m.Name) {
		handlers.SendJSONError(w, "Name must be 1 to 64 characters", http.StatusBadRequest)
		return nil, s, false
	}
	if m.Kind != "server" && m.Kind != "proxy" {
		handlers.SendJSONError(w, "Kind must be server or proxy", http.StatusBadRequest)
		return nil, s, false
	}
	if !validVisibility(m.Visibility) {
		handlers.SendJSONError(w, "Visibility must be personal, admin or platform", http.StatusBadRequest)
		return nil, s, false
	}
	if !validField(m.Software) || !validField(m.McVersion) || !validField(m.SoftwareVersion) {
		handlers.SendJSONError(w, "Software and version fields must be at most 64 characters", http.StatusBadRequest)
		return nil, s, false
	}
	v, _ := h.viewer(r, nil)
	if !h.mayCreate(w, s, v, m.Visibility) {
		return nil, s, false
	}
	t := &models.Template{
		OwnerID: v.UserID, Name: m.Name, Kind: m.Kind, Visibility: m.Visibility,
		Software: m.Software, McVersion: m.McVersion, SoftwareVersion: m.SoftwareVersion,
		StorageConnectionID: s.StorageConnectionID,
	}
	if err := h.state.Store.CreateTemplate(t); err != nil {
		log.Printf("templates: create for %s: %v", v.UserID, err)
		handlers.SendJSONError(w, "Could not create the template", http.StatusInternalServerError)
		return nil, s, false
	}
	return t, s, true
}

// mayCreate applies the create rule and, for personal templates, the count
// limit. Admin-level and platform templates belong to the platform, so only
// personal ones count against the creator.
func (h *TemplatesHandler) mayCreate(w http.ResponseWriter, s services.TemplateSettings, v services.TemplateViewer, visibility string) bool {
	if v.UserID == "" || !services.TemplateCanCreate(visibility, v, s) {
		handlers.SendJSONError(w, "You may not create this template", http.StatusForbidden)
		return false
	}
	// Before any row exists: a row whose files can never be written would count
	// against the limit and be stuck.
	if err := h.state.TemplateStorageReady(s.StorageConnectionID); err != nil {
		log.Printf("templates: storage not usable: %v", err)
		msg := "Template storage is not available right now. Try again later or ask an admin to check Settings > Storage."
		if s.StorageConnectionID == 0 && !h.state.CoreStorageConfigured() {
			msg = "Templates need Core file storage. An admin can set it up under Settings > Storage."
		}
		handlers.SendJSONError(w, msg, http.StatusServiceUnavailable)
		return false
	}
	if visibility != "personal" || s.MaxPerUser == nil {
		return true
	}
	n, err := h.personalCount(v.UserID)
	if err != nil {
		log.Printf("templates: count for %s: %v", v.UserID, err)
		handlers.SendJSONError(w, "Could not create the template", http.StatusInternalServerError)
		return false
	}
	limit := int64(*s.MaxPerUser)
	if services.AtOrOver(&limit, int64(n)) {
		handlers.SendJSONErrorCode(w, "You have reached the maximum number of templates", "templates_limit", http.StatusConflict)
		return false
	}
	return true
}

// personalCount counts the user's own personal templates.
// ponytail: check-then-insert, two parallel creates can both pass the cap by one.
func (h *TemplatesHandler) personalCount(userID string) (int, error) {
	list, err := h.state.Store.ListTemplatesVisibleTo(userID, false, false)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range list {
		if t.OwnerID == userID && t.Visibility == "personal" {
			n++
		}
	}
	return n, nil
}

// Create POST /api/templates - an empty template.
func (h *TemplatesHandler) Create(w http.ResponseWriter, r *http.Request) {
	if !handlers.CapBody(w, r, 64<<10) {
		return
	}
	var m templateMeta
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		handlers.SendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	t, _, ok := h.newTemplate(w, r, m)
	if !ok {
		return
	}
	v, _ := h.viewer(r, t)
	writeJSON(w, http.StatusCreated, item(*t, v))
}

// Update PATCH /api/templates/{id} - name, visibility, software and versions.
// A new visibility must pass the create rule for that level.
func (h *TemplatesHandler) Update(w http.ResponseWriter, r *http.Request) {
	t, v, s, ok := h.load(w, r, needEdit)
	if !ok {
		return
	}
	if !handlers.CapBody(w, r, 64<<10) {
		return
	}
	var p struct {
		Name            *string `json:"name"`
		Visibility      *string `json:"visibility"`
		Software        *string `json:"software"`
		McVersion       *string `json:"mcVersion"`
		SoftwareVersion *string `json:"softwareVersion"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		handlers.SendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if p.Name != nil {
		t.Name = strings.TrimSpace(*p.Name)
		if !validName(t.Name) {
			handlers.SendJSONError(w, "Name must be 1 to 64 characters", http.StatusBadRequest)
			return
		}
	}
	if p.Visibility != nil && *p.Visibility != t.Visibility {
		if !validVisibility(*p.Visibility) {
			handlers.SendJSONError(w, "Visibility must be personal, admin or platform", http.StatusBadRequest)
			return
		}
		if !services.TemplateCanCreate(*p.Visibility, v, s) {
			handlers.SendJSONError(w, "You may not give this template that visibility", http.StatusForbidden)
			return
		}
		t.Visibility = *p.Visibility
	}
	for _, f := range []struct {
		in  *string
		out *string
	}{{p.Software, &t.Software}, {p.McVersion, &t.McVersion}, {p.SoftwareVersion, &t.SoftwareVersion}} {
		if f.in == nil {
			continue
		}
		if !validField(*f.in) {
			handlers.SendJSONError(w, "Software and version fields must be at most 64 characters", http.StatusBadRequest)
			return
		}
		*f.out = *f.in
	}
	if err := h.state.Store.UpdateTemplate(t); err != nil {
		log.Printf("templates: update %d: %v", t.ID, err)
		handlers.SendJSONError(w, "Could not save the template", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, item(*t, v))
}

// Delete DELETE /api/templates/{id} - the files first, then the row, so a
// storage failure leaves a row that can be deleted again rather than orphaned files.
func (h *TemplatesHandler) Delete(w http.ResponseWriter, r *http.Request) {
	t, _, _, ok := h.load(w, r, needDelete)
	if !ok {
		return
	}
	// Never-configured storage holds no files, so the row can go; this is what
	// lets rows created before the storage check be cleaned up.
	if err := h.state.TemplateDelete(r.Context(), t, ""); err != nil && !h.state.TemplateStorageNeverConfigured(t) {
		log.Printf("templates: delete files of %d: %v", t.ID, err)
		handlers.SendJSONError(w, "Could not delete the template files", http.StatusInternalServerError)
		return
	}
	if err := h.state.Store.DeleteTemplate(t.ID); err != nil {
		log.Printf("templates: delete %d: %v", t.ID, err)
		handlers.SendJSONError(w, "Could not delete the template", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}
