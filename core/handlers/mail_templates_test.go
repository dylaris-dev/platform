package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/mailer"
	"dylaris-core/models"
	"dylaris-core/store"
)

type mailTemplateFakeStore struct {
	store.Store
	rows     map[string]*models.MailTemplate
	settings map[string]string
}

func newMailTemplateFakeStore() *mailTemplateFakeStore {
	return &mailTemplateFakeStore{rows: map[string]*models.MailTemplate{}, settings: map[string]string{}}
}

func (f *mailTemplateFakeStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *mailTemplateFakeStore) GetMailTemplate(k string) (*models.MailTemplate, error) {
	return f.rows[k], nil
}
func (f *mailTemplateFakeStore) UpsertMailTemplate(t *models.MailTemplate) error {
	c := *t
	f.rows[t.Key] = &c
	return nil
}
func (f *mailTemplateFakeStore) DeleteMailTemplate(k string) error {
	delete(f.rows, k)
	return nil
}

func mailTemplateRequest(t *testing.T, h *MailTemplatesHandler, method, key, action string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	path := "/api/admin/mail/templates/" + key
	if action != "" {
		path += "/" + action
	}
	r := httptest.NewRequest(method, path, &buf)
	r = mux.SetURLVars(r, map[string]string{"key": key})
	w := httptest.NewRecorder()
	switch {
	case action == "preview":
		h.Preview(w, r)
	case method == http.MethodPut:
		h.Save(w, r)
	case method == http.MethodDelete:
		h.Reset(w, r)
	default:
		h.Get(w, r)
	}
	return w
}

func newMailTemplatesTestHandler() (*MailTemplatesHandler, *mailTemplateFakeStore) {
	st := newMailTemplateFakeStore()
	st.settings["smtp.default.from_name"] = "Example Host"
	return NewMailTemplatesHandler(&AppState{Store: st, FrontendURL: "https://panel.example.com"}), st
}

// A typo in a variable name renders as nothing at all - an empty gap where the
// link should be, in a mail nobody re-reads. It has to be refused at save time
// or it is found by a customer who cannot finish signing up.
func TestSavingAMailTemplateRefusesAnUndeclaredVariable(t *testing.T) {
	h, st := newMailTemplatesTestHandler()
	w := mailTemplateRequest(t, h, http.MethodPut, mailer.KeyVerifyEmail, "", map[string]string{
		"subject": "Confirm",
		"body":    "Hello {{username}}, go to {{verify_lnik}}",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", w.Code)
	}
	if len(st.rows) != 0 {
		t.Error("a template with an unknown variable was stored")
	}
}

// An empty body is not an edit, it is a mail with nothing in it. Reset is its
// own action and must not be reachable by clearing the box.
func TestSavingAMailTemplateRefusesAnEmptyBody(t *testing.T) {
	h, _ := newMailTemplatesTestHandler()
	w := mailTemplateRequest(t, h, http.MethodPut, mailer.KeyVerifyEmail, "", map[string]string{
		"subject": "Confirm", "body": "   ",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", w.Code)
	}
}

// The editor must open on what WILL be sent. Showing an empty box that silently
// means "the default" is how people end up retyping text that is already there.
func TestAnUneditedTemplateReportsTheDefaultAsItsCurrentText(t *testing.T) {
	h, _ := newMailTemplatesTestHandler()
	w := mailTemplateRequest(t, h, http.MethodGet, mailer.KeyVerifyEmail, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var got struct {
		Template struct {
			Edited         bool   `json:"edited"`
			CurrentSubject string `json:"currentSubject"`
			CurrentBody    string `json:"currentBody"`
		} `json:"template"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Template.Edited {
		t.Error("an untouched template reports itself as edited")
	}
	if got.Template.CurrentBody == "" || got.Template.CurrentSubject == "" {
		t.Error("the editor would open empty instead of on the default wording")
	}
}

// Reset is a DELETE of the row, not a copy of today's default text into it: a
// copy would freeze this release's wording and never pick up a later change.
func TestResettingAMailTemplateRemovesTheRow(t *testing.T) {
	h, st := newMailTemplatesTestHandler()
	if w := mailTemplateRequest(t, h, http.MethodPut, mailer.KeyVerifyEmail, "", map[string]string{
		"subject": "Mine", "body": "Hello {{username}}, here: {{verify_link}}",
	}); w.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", w.Code, w.Body.String())
	}
	if len(st.rows) != 1 {
		t.Fatalf("the edit was not stored: %v", st.rows)
	}
	if w := mailTemplateRequest(t, h, http.MethodDelete, mailer.KeyVerifyEmail, "", nil); w.Code != http.StatusOK {
		t.Fatalf("reset failed: %d", w.Code)
	}
	if len(st.rows) != 0 {
		t.Error("reset left the row behind, so the default can never come back")
	}
}

// The preview must follow the EDITOR, not the saved row, or it shows the old
// wording while somebody is deciding whether to keep the new one.
func TestPreviewRendersTheSubmittedTextRatherThanTheStoredOne(t *testing.T) {
	h, _ := newMailTemplatesTestHandler()
	if w := mailTemplateRequest(t, h, http.MethodPut, mailer.KeyVerifyEmail, "", map[string]string{
		"subject": "Stored", "body": "Stored body with {{verify_link}}",
	}); w.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", w.Code, w.Body.String())
	}
	w := mailTemplateRequest(t, h, http.MethodPost, mailer.KeyVerifyEmail, "preview", map[string]string{
		"subject": "Being edited", "body": "Being edited, link {{verify_link}}",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("preview failed: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		Preview mailer.Rendered `json:"preview"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Preview.Subject != "Being edited" {
		t.Errorf("preview subject %q, want the text being edited", got.Preview.Subject)
	}
	if got.Preview.HTML == "" || got.Preview.Text == "" {
		t.Error("the preview must carry both parts, because both are sent")
	}
	// The sender name doubles as the platform name in the layout.
	if !bytes.Contains([]byte(got.Preview.HTML), []byte("Example Host")) {
		t.Error("the configured sender name did not reach the rendered layout")
	}
}
