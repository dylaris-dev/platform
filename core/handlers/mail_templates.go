package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gorilla/mux"

	"dylaris-core/mailer"
	"dylaris-core/models"
	"dylaris-core/services"
)

// Editing the outgoing mail.
//
// Every route is PANEL settings.write (RequireCap at the route). Reading is
// gated too, not only writing: a template body carries the exact wording of a
// password-reset mail, which is a phishing template for anyone who can read it.

type MailTemplatesHandler struct{ state *AppState }

func NewMailTemplatesHandler(state *AppState) *MailTemplatesHandler {
	return &MailTemplatesHandler{state: state}
}

// templateView is one row of the list: what the platform can send, plus whether
// this install has changed it.
type templateView struct {
	mailer.Definition
	Edited bool `json:"edited"`
	// The operator's current text, or the default when they have not edited it.
	// One field rather than "default plus override": the editor opens on what
	// WILL be sent, and an empty box that silently means "the default" is the
	// shape that makes people retype what is already there.
	CurrentSubject string `json:"currentSubject"`
	CurrentBody    string `json:"currentBody"`
}

func (h *MailTemplatesHandler) view(def mailer.Definition) templateView {
	v := templateView{Definition: def, CurrentSubject: def.Subject, CurrentBody: def.Body}
	row, err := h.state.Store.GetMailTemplate(def.Key)
	if err != nil || row == nil {
		return v
	}
	v.Edited = true
	if s := strings.TrimSpace(row.Subject); s != "" {
		v.CurrentSubject = row.Subject
	}
	if b := strings.TrimSpace(row.Body); b != "" {
		v.CurrentBody = row.Body
	}
	return v
}

// List GET /api/admin/mail/templates
func (h *MailTemplatesHandler) List(w http.ResponseWriter, r *http.Request) {
	defs := mailer.Definitions()
	out := make([]templateView, 0, len(defs))
	for _, d := range defs {
		out = append(out, h.view(d))
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true, "templates": out})
}

// Get GET /api/admin/mail/templates/{key}
func (h *MailTemplatesHandler) Get(w http.ResponseWriter, r *http.Request) {
	def, ok := mailer.DefinitionByKey(mux.Vars(r)["key"])
	if !ok {
		sendJSONError(w, "Unknown template", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true, "template": h.view(def)})
}

type saveTemplateRequest struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// Save PUT /api/admin/mail/templates/{key}
func (h *MailTemplatesHandler) Save(w http.ResponseWriter, r *http.Request) {
	def, ok := mailer.DefinitionByKey(mux.Vars(r)["key"])
	if !ok {
		sendJSONError(w, "Unknown template", http.StatusNotFound)
		return
	}
	var req saveTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid request", http.StatusBadRequest)
		return
	}
	req.Subject, req.Body = strings.TrimSpace(req.Subject), strings.TrimSpace(req.Body)
	if req.Body == "" {
		// An empty body is not an edit, it is a mail with nothing in it. Reset
		// is its own action (DELETE) and says so.
		sendJSONError(w, "The body cannot be empty. Use Reset to go back to the default wording.", http.StatusBadRequest)
		return
	}
	// Refuse a placeholder the definition does not declare. A typo would
	// otherwise render as nothing at all: an empty gap where a link should be,
	// in a mail nobody re-reads, found by a customer who cannot finish signing up.
	if err := def.Validate(mailer.Template{Subject: req.Subject, Body: req.Body}); err != nil {
		sendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.state.Store.UpsertMailTemplate(&models.MailTemplate{
		Key: def.Key, Subject: req.Subject, Body: req.Body,
	}); err != nil {
		sendJSONError(w, "Could not save the template", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true, "template": h.view(def)})
}

// Reset DELETE /api/admin/mail/templates/{key} — back to the built-in wording.
func (h *MailTemplatesHandler) Reset(w http.ResponseWriter, r *http.Request) {
	def, ok := mailer.DefinitionByKey(mux.Vars(r)["key"])
	if !ok {
		sendJSONError(w, "Unknown template", http.StatusNotFound)
		return
	}
	if err := h.state.Store.DeleteMailTemplate(def.Key); err != nil {
		sendJSONError(w, "Could not reset the template", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true, "template": h.view(def)})
}

// Preview POST /api/admin/mail/templates/{key}/preview
//
// Renders the SUBMITTED text rather than the saved one, so the preview follows
// the editor before anything is committed. Example values come from the
// definition, which is also what makes them safe to show: no real customer's
// name or link goes through here.
func (h *MailTemplatesHandler) Preview(w http.ResponseWriter, r *http.Request) {
	def, ok := mailer.DefinitionByKey(mux.Vars(r)["key"])
	if !ok {
		sendJSONError(w, "Unknown template", http.StatusNotFound)
		return
	}
	var req saveTemplateRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	var override *mailer.Template
	if strings.TrimSpace(req.Subject) != "" || strings.TrimSpace(req.Body) != "" {
		override = &mailer.Template{Key: def.Key, Subject: req.Subject, Body: req.Body}
		if err := def.Validate(*override); err != nil {
			sendJSONError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	_, senderName := mailer.SenderIdentity(h.state.Store, def.Purpose)
	out := mailer.Render(def, override,
		mailer.Brand{SiteName: senderName, PanelURL: h.state.FrontendURL},
		exampleValues(def))
	json.NewEncoder(w).Encode(map[string]any{"success": true, "preview": out})
}

// TestSend POST /api/admin/mail/templates/{key}/test
func (h *MailTemplatesHandler) TestSend(w http.ResponseWriter, r *http.Request) {
	def, ok := mailer.DefinitionByKey(mux.Vars(r)["key"])
	if !ok {
		sendJSONError(w, "Unknown template", http.StatusNotFound)
		return
	}
	var req struct {
		To string `json:"to"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	to := strings.TrimSpace(req.To)
	if to == "" {
		// The admin's own address by default, so a misclick cannot reach a real
		// customer with a test. Same rule as the SMTP test button.
		actorID, _ := r.Context().Value("userID").(string)
		if u, err := h.state.Store.GetUserByID(actorID); err == nil && u != nil {
			to = u.Email
		}
	}
	if to == "" {
		sendJSONError(w, "No recipient - provide an address or set your account email first", http.StatusBadRequest)
		return
	}
	// Example values, never real ones: a test send must not put a working
	// password-reset link into anybody's inbox.
	if err := services.SendMail(h.state.Store, def.Key, to, h.state.FrontendURL, exampleValues(def)); err != nil {
		json.NewEncoder(w).Encode(map[string]any{"success": false, "message": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "Sent to " + to})
}

// exampleValues fills every declared variable with its example.
func exampleValues(def mailer.Definition) map[string]string {
	out := map[string]string{}
	for _, v := range def.Variables {
		out[v.Name] = v.Example
	}
	return out
}
