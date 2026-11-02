package templates

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"dylaris-core/handlers/access"
	"dylaris-core/handlers/guard"
	"dylaris-core/handlers/httpx"
	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

// TemplatesHandler serves /api/templates. Every route sits behind
// RequireTemplatesAudience, which is what keeps non-admins out while the
// feature is off or admin-only; the per-template rules below do not repeat it.
type TemplatesHandler struct {
	d Deps
}

func NewTemplatesHandler(d Deps) *TemplatesHandler {
	return &TemplatesHandler{d: d}
}

const maxTemplateNameLen = 64

// viewer resolves who is asking; see AppState.TemplateViewer.
func (h *TemplatesHandler) viewer(r *http.Request, t *models.Template) (services.TemplateViewer, error) {
	return access.TemplateViewer(r, h.d.Authz, h.d.Store, t)
}

func (h *TemplatesHandler) settings(r *http.Request) services.TemplateSettings {
	return services.LoadTemplateSettings(r.Context(), h.d.FeatureFlags)
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
		httpx.SendJSONError(w, "Template not found", http.StatusNotFound)
		return nil, services.TemplateViewer{}, s, false
	}
	t, err := h.d.Store.GetTemplate(id)
	if err != nil {
		log.Printf("templates: get %d: %v", id, err)
		httpx.SendJSONError(w, "Could not load the template", http.StatusInternalServerError)
		return nil, services.TemplateViewer{}, s, false
	}
	if t == nil {
		httpx.SendJSONError(w, "Template not found", http.StatusNotFound)
		return nil, services.TemplateViewer{}, s, false
	}
	v, err := h.viewer(r, t)
	if err != nil {
		log.Printf("templates: viewer for %d: %v", id, err)
		httpx.SendJSONError(w, "Could not load the template", http.StatusInternalServerError)
		return nil, v, s, false
	}
	if !services.TemplateCanSee(*t, v, s) {
		httpx.SendJSONError(w, "Template not found", http.StatusNotFound)
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
		httpx.SendJSONError(w, "You may not change this template", http.StatusForbidden)
		return nil, v, s, false
	}
	return t, v, withCap(s, t, h.ownerIsAdmin(t, v)), true
}

// refuseSuspended answers 403 and reports true while ownerID is suspended or
// cut off (an admin panel session passes). Writes call it; reads, downloads
// and deletes do not, so a tenant can always see and leave with what they have.
func (h *TemplatesHandler) refuseSuspended(w http.ResponseWriter, r *http.Request, ownerID string) bool {
	msg := guard.OwnerRefusal(h.d.Store, h.d.SuspendGrace, r, ownerID)
	if msg == "" {
		return false
	}
	httpx.SendJSONError(w, msg, http.StatusForbidden)
	return true
}

// withCap is s with MaxSizeBytes set to t's own cap, so every reader of the
// cap (fits, bodyLimit, capReader, the capture bound) applies the right one.
func withCap(s services.TemplateSettings, t *models.Template, ownerIsAdmin bool) services.TemplateSettings {
	s.MaxSizeBytes = s.SizeCap(t.Visibility, ownerIsAdmin, t.BackupStorageID != nil)
	return s
}

// ownerIsAdmin asks about t's OWNER: an admin editing a user's template does
// not lift it to the admin cap. A failed lookup answers false, the smaller cap.
func (h *TemplatesHandler) ownerIsAdmin(t *models.Template, v services.TemplateViewer) bool {
	if t.OwnerID == v.UserID {
		return v.IsAdmin
	}
	u, err := h.d.Store.GetUserByID(t.OwnerID)
	if err != nil {
		log.Printf("templates: owner of %d: %v", t.ID, err)
	}
	return err == nil && u != nil && u.IsAdmin
}

// ownTarget is the caller's own storage a new template of this visibility
// goes to, nil for Dylaris storage. Only personal templates follow the
// preference; admin-level and platform ones belong to the platform. With own
// storage switched off for the caller the preference is ignored, not cleared,
// so it comes back when the operator turns the feature on again. Edits to an
// EXISTING template keep writing to its recorded own target while off (by
// decision): its files live there, and splitting them would break it. It
// writes the error response itself.
func (h *TemplatesHandler) ownTarget(w http.ResponseWriter, r *http.Request, userID string, isAdmin bool, visibility string) (*int, bool) {
	if visibility != "personal" || userID == "" || !h.d.FeatureFlags.OwnStorageAllowed(r.Context(), isAdmin) {
		return nil, true
	}
	_, own, err := h.d.Store.GetUserStoragePrefs(userID)
	if err != nil {
		log.Printf("templates: storage prefs of %s: %v", userID, err)
		httpx.SendJSONError(w, "Could not create the template", http.StatusInternalServerError)
		return nil, false
	}
	return own, true
}

type templateItem struct {
	models.Template
	CanEdit    bool   `json:"canEdit"`
	CanShare   bool   `json:"canShare"`
	ShareLevel string `json:"shareLevel"`
	// WriteRefusal is what a write to this template would be answered with
	// while its owner is suspended; reads, downloads and deletes still work.
	// Only List and Get fill it.
	WriteRefusal string `json:"writeRefusal,omitempty"`
	// MaxSizeBytes is this template's own size cap (see withCap): the admin cap
	// for admin-level, platform and admin-owned ones, none (null) on the
	// owner's own storage, else the user cap.
	MaxSizeBytes *int64 `json:"maxSizeBytes"`
}

func item(t models.Template, v services.TemplateViewer, maxSize *int64) templateItem {
	return templateItem{Template: t, CanEdit: services.TemplateCanEdit(t, v), CanShare: services.TemplateCanShare(t, v), ShareLevel: v.ShareLevel, MaxSizeBytes: maxSize}
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
	list, err := h.d.Store.ListTemplatesVisibleTo(v.UserID, v.IsAdmin || v.HasAdminRead, v.IsAdmin || s.PlatformForUsers)
	if err != nil {
		log.Printf("templates: list for %s: %v", v.UserID, err)
		httpx.SendJSONError(w, "Could not list templates", http.StatusInternalServerError)
		return
	}
	out := []templateItem{}
	refusals := map[string]string{} // per owner: one billing read each
	admins := map[string]bool{}     // per owner: one user read each
	for _, t := range list {
		tv := v
		if t.OwnerID != v.UserID {
			if tv.ShareLevel, err = h.d.Store.TemplateShareLevel(t.ID, v.UserID); err != nil {
				log.Printf("templates: share level %d/%s: %v", t.ID, v.UserID, err)
				httpx.SendJSONError(w, "Could not list templates", http.StatusInternalServerError)
				return
			}
		}
		if services.TemplateCanSee(t, tv, s) {
			ownerAdmin, seen := admins[t.OwnerID]
			if !seen {
				ownerAdmin = h.ownerIsAdmin(&t, v)
				admins[t.OwnerID] = ownerAdmin
			}
			it := item(t, tv, withCap(s, &t, ownerAdmin).MaxSizeBytes)
			msg, seen := refusals[t.OwnerID]
			if !seen {
				msg = guard.OwnerRefusal(h.d.Store, h.d.SuspendGrace, r, t.OwnerID)
				refusals[t.OwnerID] = msg
			}
			it.WriteRefusal = msg
			out = append(out, it)
		}
	}
	var own *int
	if h.d.FeatureFlags.OwnStorageAllowed(r.Context(), v.IsAdmin) {
		if _, own, err = h.d.Store.GetUserStoragePrefs(v.UserID); err != nil {
			log.Printf("templates: storage prefs of %s: %v", v.UserID, err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"templates":         out,
		"maxSizeBytes":      s.MaxSizeBytes,
		"maxSizeBytesAdmin": s.MaxSizeBytesAdmin,
		"maxPerUser":        s.MaxPerUser,
		// New personal templates go to the caller's own storage, where neither
		// limit applies.
		"ownStorage": own != nil,
	})
}

// Get GET /api/templates/{id}
func (h *TemplatesHandler) Get(w http.ResponseWriter, r *http.Request) {
	t, v, s, ok := h.load(w, r, needSee)
	if !ok {
		return
	}
	it := item(*t, v, s.MaxSizeBytes)
	it.WriteRefusal = guard.OwnerRefusal(h.d.Store, h.d.SuspendGrace, r, t.OwnerID)
	writeJSON(w, http.StatusOK, it)
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

// sentence is a validation error as the panel shows it: capitalised, one full stop.
func sentence(err error) string {
	msg := err.Error()
	return strings.ToUpper(msg[:1]) + msg[1:] + "."
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
		httpx.SendJSONError(w, "Name must be 1 to 64 characters", http.StatusBadRequest)
		return nil, s, false
	}
	if m.Kind != "server" && m.Kind != "proxy" {
		httpx.SendJSONError(w, "Kind must be server or proxy", http.StatusBadRequest)
		return nil, s, false
	}
	if !validVisibility(m.Visibility) {
		httpx.SendJSONError(w, "Visibility must be personal, admin or platform", http.StatusBadRequest)
		return nil, s, false
	}
	if !validField(m.Software) || !validField(m.McVersion) || !validField(m.SoftwareVersion) {
		httpx.SendJSONError(w, "Software and version fields must be at most 64 characters", http.StatusBadRequest)
		return nil, s, false
	}
	sw := models.Template{Software: m.Software, McVersion: m.McVersion, SoftwareVersion: m.SoftwareVersion}
	if err := services.ValidateTemplateSoftware(&sw); err != nil {
		httpx.SendJSONError(w, sentence(err), http.StatusBadRequest)
		return nil, s, false
	}
	m.Software, m.McVersion, m.SoftwareVersion = sw.Software, sw.McVersion, sw.SoftwareVersion
	v, _ := h.viewer(r, nil)
	own, ok := h.ownTarget(w, r, v.UserID, v.IsAdmin, m.Visibility)
	if !ok || !h.mayCreate(w, s, v, m.Visibility, own) {
		return nil, s, false
	}
	t := &models.Template{
		OwnerID: v.UserID, Name: m.Name, Kind: m.Kind, Visibility: m.Visibility,
		Software: m.Software, McVersion: m.McVersion, SoftwareVersion: m.SoftwareVersion,
		StorageConnectionID: s.StorageConnectionID, BackupStorageID: own,
	}
	if own != nil {
		// The files are in the tenant's bucket, not on that connection: a stamp
		// would count as a reference in CountTemplatesOnStorageConnection and
		// send the janitor to a connection that never holds this template.
		t.StorageConnectionID = 0
	}
	if err := h.d.Store.CreateTemplate(t); err != nil {
		log.Printf("templates: create for %s: %v", v.UserID, err)
		httpx.SendJSONError(w, "Could not create the template", http.StatusInternalServerError)
		return nil, s, false
	}
	return t, withCap(s, t, v.IsAdmin), true
}

// mayCreate applies the create rule and, for personal templates on Dylaris
// storage, the count limit. Admin-level and platform templates belong to the
// platform, so only personal ones count against the creator. own is the
// caller's own storage the template would go to (nil = ours).
func (h *TemplatesHandler) mayCreate(w http.ResponseWriter, s services.TemplateSettings, v services.TemplateViewer, visibility string, own *int) bool {
	if v.UserID == "" || !services.TemplateCanCreate(visibility, v, s) {
		httpx.SendJSONError(w, "You may not create this template", http.StatusForbidden)
		return false
	}
	// Before any row exists: a row whose files can never be written would count
	// against the limit and be stuck.
	if err := h.d.CoreStorage.TemplateStorageReady(v.UserID, s.StorageConnectionID, own); err != nil {
		log.Printf("templates: storage not usable: %v", err)
		msg := "Template storage is not available right now. Try again later or ask an admin to check Settings > Storage."
		switch {
		case own != nil:
			// The detail stays in the log: it can name the endpoint.
			msg = "Your own template storage is not usable. Check it under Account > Storage, or switch templates back to platform storage there."
		case s.StorageConnectionID == 0 && !h.d.CoreStorage.Configured():
			msg = "Templates need Core file storage. An admin can set it up under Settings > Storage."
		}
		httpx.SendJSONError(w, msg, http.StatusServiceUnavailable)
		return false
	}
	if visibility != "personal" || own != nil || s.MaxPerUser == nil {
		return true
	}
	n, err := h.personalCount(v.UserID)
	if err != nil {
		log.Printf("templates: count for %s: %v", v.UserID, err)
		httpx.SendJSONError(w, "Could not create the template", http.StatusInternalServerError)
		return false
	}
	limit := int64(*s.MaxPerUser)
	if services.AtOrOver(&limit, int64(n)) {
		httpx.SendJSONErrorCode(w, "You have reached the maximum number of templates", "templates_limit", http.StatusConflict)
		return false
	}
	return true
}

// personalCount counts the user's own personal templates on Dylaris storage;
// those on their own storage cost us nothing and are not limited.
// ponytail: check-then-insert, two parallel creates can both pass the cap by one.
func (h *TemplatesHandler) personalCount(userID string) (int, error) {
	list, err := h.d.Store.ListTemplatesVisibleTo(userID, false, false)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range list {
		if t.OwnerID == userID && t.Visibility == "personal" && t.BackupStorageID == nil {
			n++
		}
	}
	return n, nil
}

// Create POST /api/templates - an empty template.
func (h *TemplatesHandler) Create(w http.ResponseWriter, r *http.Request) {
	if caller, _ := h.viewer(r, nil); h.refuseSuspended(w, r, caller.UserID) {
		return
	}
	if !httpx.CapBody(w, r, 64<<10) {
		return
	}
	var m templateMeta
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		httpx.SendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	t, s, ok := h.newTemplate(w, r, m)
	if !ok {
		return
	}
	v, _ := h.viewer(r, t)
	writeJSON(w, http.StatusCreated, item(*t, v, s.MaxSizeBytes))
}

// Update PATCH /api/templates/{id} - name, visibility, software and versions.
// A new visibility must pass the create rule for that level.
func (h *TemplatesHandler) Update(w http.ResponseWriter, r *http.Request) {
	t, v, s, ok := h.load(w, r, needEdit)
	if !ok || h.refuseSuspended(w, r, t.OwnerID) {
		return
	}
	if !httpx.CapBody(w, r, 64<<10) {
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
		httpx.SendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if p.Name != nil {
		t.Name = strings.TrimSpace(*p.Name)
		if !validName(t.Name) {
			httpx.SendJSONError(w, "Name must be 1 to 64 characters", http.StatusBadRequest)
			return
		}
	}
	if p.Visibility != nil && *p.Visibility != t.Visibility {
		if !validVisibility(*p.Visibility) {
			httpx.SendJSONError(w, "Visibility must be personal, admin or platform", http.StatusBadRequest)
			return
		}
		if !services.TemplateCanCreate(*p.Visibility, v, s) {
			httpx.SendJSONError(w, "You may not give this template that visibility", http.StatusForbidden)
			return
		}
		// A shared template must not depend on one tenant's bucket and credentials.
		if *p.Visibility != "personal" && t.BackupStorageID != nil {
			httpx.SendJSONError(w, "This template is stored in its owner's own storage. Admin and platform templates must live on platform storage.", http.StatusConflict)
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
			httpx.SendJSONError(w, "Software and version fields must be at most 64 characters", http.StatusBadRequest)
			return
		}
		*f.out = *f.in
	}
	// Only when one of them changes: a rename must not fail on what an older
	// release let through.
	if p.Software != nil || p.McVersion != nil || p.SoftwareVersion != nil {
		if err := services.ValidateTemplateSoftware(t); err != nil {
			httpx.SendJSONError(w, sentence(err), http.StatusBadRequest)
			return
		}
	}
	if err := h.d.Store.UpdateTemplate(t); err != nil {
		log.Printf("templates: update %d: %v", t.ID, err)
		httpx.SendJSONError(w, "Could not save the template", http.StatusInternalServerError)
		return
	}
	// Not s: that is the cap of the visibility loaded, which this edit may have changed.
	writeJSON(w, http.StatusOK, item(*t, v, withCap(h.settings(r), t, h.ownerIsAdmin(t, v)).MaxSizeBytes))
}

// ownFilesMayRemain is what a delete answers when the row of a template on its
// owner's own storage went but its files could not be removed.
const ownFilesMayRemain = "The template was removed. Files may remain in your own storage."

// deleteFiles removes t's files ahead of its row and reports whether the row
// may go. On OUR storage a failure keeps the row, so the delete can be retried
// and the janitor never sees files without one. On the owner's own storage the
// row goes anyway (filesLeft): an unreachable bucket would otherwise make the
// template undeletable, and its row would block removing that storage forever.
// Nothing of ours sweeps a tenant bucket, so what is left there is theirs.
func (h *TemplatesHandler) deleteFiles(ctx context.Context, t *models.Template) (rowMayGo, filesLeft bool) {
	err := h.d.CoreStorage.TemplateDelete(ctx, t, "")
	switch {
	case err == nil:
		return true, false
	case t.BackupStorageID != nil:
		log.Printf("templates: delete files of %d on own storage %d, removing the row anyway: %v", t.ID, *t.BackupStorageID, err)
		return true, true
	case h.d.CoreStorage.TemplateStorageNeverConfigured(t):
		// Never-configured storage holds no files; this is what lets rows
		// created before the storage check be cleaned up.
		return true, false
	default:
		log.Printf("templates: delete files of %d: %v", t.ID, err)
		return false, false
	}
}

// Delete DELETE /api/templates/{id} - the files first, then the row, so a
// storage failure leaves a row that can be deleted again rather than orphaned files; on the owner's own storage the row goes anyway, with a warning.
func (h *TemplatesHandler) Delete(w http.ResponseWriter, r *http.Request) {
	t, _, _, ok := h.load(w, r, needDelete)
	if !ok {
		return
	}
	rowMayGo, filesLeft := h.deleteFiles(r.Context(), t)
	if !rowMayGo {
		httpx.SendJSONError(w, "Could not delete the template files", http.StatusInternalServerError)
		return
	}
	if err := h.d.Store.DeleteTemplate(t.ID); err != nil {
		log.Printf("templates: delete %d: %v", t.ID, err)
		httpx.SendJSONError(w, "Could not delete the template", http.StatusInternalServerError)
		return
	}
	body := map[string]interface{}{"success": true}
	if filesLeft {
		body["warning"] = ownFilesMayRemain
	}
	writeJSON(w, http.StatusOK, body)
}
