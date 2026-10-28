package packs

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func strp(s string) *string { return &s }

func TestModrinthPageFieldsRefusesWhatModrinthWould(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit modrinthPageEdit
		bad  bool
	}{
		{"short name", modrinthPageEdit{Title: strp("ab")}, true},
		{"long summary", modrinthPageEdit{Description: strp(strings.Repeat("x", 257))}, true},
		{"relative link", modrinthPageEdit{SourceURL: strp("github.com/x")}, true},
		{"javascript link", modrinthPageEdit{DiscordURL: strp("javascript:alert(1)")}, true},
		{"unknown environment", modrinthPageEdit{Environment: strp("anywhere")}, true},
		{"empty license", modrinthPageEdit{LicenseID: strp(" ")}, true},
		{"valid", modrinthPageEdit{Title: strp(" Bartis Lands "), SourceURL: strp("https://github.com/x/y"), Environment: strp("client_only")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, msg := modrinthPageFields(tc.edit)
			if (msg != "") != tc.bad {
				t.Fatalf("message %q, want refusal=%v", msg, tc.bad)
			}
		})
	}
	f, _ := modrinthPageFields(modrinthPageEdit{Title: strp(" Bartis Lands "), WikiURL: strp(""), Categories: &[]string{" adventure ", ""}})
	if f["title"] != "Bartis Lands" {
		t.Fatalf("title not trimmed: %#v", f["title"])
	}
	if v, ok := f["wiki_url"]; !ok || v != nil {
		t.Fatalf("an emptied link must be sent as null to clear it, got %#v (present=%v)", v, ok)
	}
	if c, _ := f["categories"].([]string); len(c) != 1 || c[0] != "adventure" {
		t.Fatalf("categories = %#v", f["categories"])
	}
	if _, ok := f["environment"]; ok {
		t.Fatal("environment is a version field, not a project field")
	}
}

// Submit must not reach Modrinth while its own checklist has a required item.
func TestSubmitModrinthReviewWaitsForRequiredItems(t *testing.T) {
	h, base, _ := newPackCleanup(t)
	base.pack.ModrinthProjectID = "proj"
	h.SetPATLoader(&publishBindingPAT{})
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	submitted := false
	http.DefaultTransport = modrinthAddTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPatch {
			submitted = true
		}
		body := `{"nags":[{"kind":"description_too_short","severity":"required"}]}`
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	w := httptest.NewRecorder()
	h.SubmitModrinthReview(w, packReq(http.MethodPost, map[string]string{"id": "1"}, ""))
	if w.Code != http.StatusConflict || submitted {
		t.Fatalf("status=%d submitted=%v body=%s", w.Code, submitted, w.Body.String())
	}
}
