package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestValidatePublicBaseURL pins the input rules for core_public_url. A node is
// pointed at it to download a pack, and it feeds isSnapshotFetchHostAllowed's
// SSRF allowlist, so a scheme-less or hostless string must not persist.
func TestValidatePublicBaseURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"empty is allowed (feature simply unconfigured)", "", true},
		{"https origin", "https://panel.example.com", true},
		{"http origin", "http://10.0.0.5:25500", true},
		{"origin with a path", "https://cdn.example.com/modpacks", true},
		{"trailing slash", "https://cdn.example.com/modpacks/", true},
		{"no scheme", "panel.example.com", false},
		{"scheme-relative", "//panel.example.com", false},
		{"unsupported scheme", "ftp://panel.example.com", false},
		{"file scheme", "file:///etc/passwd", false},
		{"host missing", "https://", false},
		{"embedded credentials", "https://user:pw@panel.example.com", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validatePublicBaseURL("core public URL", c.in)
			if c.ok && err != nil {
				t.Fatalf("validatePublicBaseURL(%q) = %v, want nil", c.in, err)
			}
			if !c.ok && err == nil {
				t.Fatalf("validatePublicBaseURL(%q) = nil, want an error", c.in)
			}
		})
	}
}

// TestModpackSettingsHandler_Set_PersistsCorePublicURL is the regression guard
// for the shipped defect: the key was READ by the mirror base and written by
// nothing at all, so a pack install had no supported way to find Core.
//
// It also pins that the two Solder settings stay gone. The boot schema deletes
// them; a save that wrote them again would put back what the removal took out.
func TestModpackSettingsHandler_Set_PersistsCorePublicURL(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	h := newModpackSettingsTestHandler(fs)

	body, _ := json.Marshal(modpackSettings{
		Provider:      "local",
		CorePublicURL: "https://panel.example.com/",
	})
	rw := httptest.NewRecorder()
	h.Set(rw, httptest.NewRequest(http.MethodPut, "/api/admin/settings/modpacks", bytes.NewReader(body)))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rw.Code, rw.Body.String())
	}
	if got := fs.kv["core_public_url"]; got != "https://panel.example.com/" {
		t.Errorf("settings[core_public_url] = %q, want the saved value", got)
	}
	for _, k := range []string{"solder_mirror_url", "solder_delivery_mode"} {
		if _, ok := fs.kv[k]; ok {
			t.Errorf("save wrote %q, a setting Solder's removal deleted", k)
		}
	}

	// And GET must echo it back, or the panel form loads blank and the next
	// save silently clears what was just configured.
	rw = httptest.NewRecorder()
	h.Get(rw, httptest.NewRequest(http.MethodGet, "/api/admin/settings/modpacks", nil))
	var out struct {
		Settings modpackSettings `json:"settings"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if out.Settings.CorePublicURL != "https://panel.example.com/" {
		t.Errorf("GET corePublicUrl = %q, want the saved value", out.Settings.CorePublicURL)
	}
}

func TestModpackSettingsHandler_Set_RejectsBadCorePublicURL(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	h := newModpackSettingsTestHandler(fs)

	body, _ := json.Marshal(modpackSettings{Provider: "local", CorePublicURL: "panel.example.com"})
	rw := httptest.NewRecorder()
	h.Set(rw, httptest.NewRequest(http.MethodPut, "/api/admin/settings/modpacks", bytes.NewReader(body)))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rw.Code, rw.Body.String())
	}
	if _, ok := fs.kv["modpack_storage_provider"]; ok {
		t.Errorf("a rejected save still reached the store, kv = %v", fs.kv)
	}
}
