package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Two operator-set URLs were stored with no check at all while their siblings
// (core public URL, custom tab URL) validated scheme and
// host:
//
//   - beam.download_link is a URL Core GETs server-side and streams to the
//     caller of the deliberately unauthenticated /api/beam/download.
//   - the billing payment URL is handed to every tenant by GetMyBilling and the
//     panel renders the whole banner as <a href={paymentUrl}>.
//
// Both are written by a delegatable panel capability (settings.write /
// plans.write), so "an admin typed it" is not the threat model.

func TestSaveBeamSettings_ValidatesTheDownloadLink(t *testing.T) {
	cases := []struct {
		name       string
		link       string
		wantStatus int
	}{
		{"empty is allowed (feature simply unconfigured)", "", http.StatusOK},
		{"a real https mirror", "https://cdn.example.com/beam", http.StatusOK},
		{"plain http is allowed too", "http://mirror.internal.example.com", http.StatusOK},
		{"a scheme-less string", "cdn.example.com/beam", http.StatusBadRequest},
		{"javascript scheme", "javascript:alert(1)", http.StatusBadRequest},
		{"file scheme", "file:///etc/passwd", http.StatusBadRequest},
		{"credentials in the url", "https://user:pw@cdn.example.com", http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := newCoreStorageHTTPFakeStore()
			h := &SettingsHandler{state: &AppState{Store: fs}}

			body, _ := json.Marshal(BeamSettings{Enabled: true, DownloadLink: c.link})
			rw := httptest.NewRecorder()
			h.SaveBeamSettings(rw, httptest.NewRequest(http.MethodPost, "/api/settings/beam", bytes.NewReader(body)))

			if rw.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rw.Code, c.wantStatus, rw.Body.String())
			}
			if c.wantStatus != http.StatusOK && fs.kv["beam.download_link"] != "" {
				t.Fatalf("a rejected link was persisted: %q", fs.kv["beam.download_link"])
			}
		})
	}
}
