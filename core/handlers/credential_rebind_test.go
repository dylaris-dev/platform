package handlers

import (
	"bytes"
	"dylaris-core/handlers/handlertest"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The rule these decide: a blank credential on save means "keep the stored
// one", and that convenience must not double as a way to point the stored one
// at somewhere else.
//
// Three storage configs already carry this guard (mergeCoreStorageCandidate,
// mergeBackupStorageSecret, modpackS3SecretRebound) and two credential
// endpoints did not - the two where the credential actually travels. S3 signs
// with its secret and never sends it, so rebinding an S3 config misdirects
// signed requests. SMTP PLAIN AUTH and Redis AUTH hand the password to whatever
// host they are pointed at, and settings.write is a delegatable panel
// capability whose holder can never READ either one.

func TestSMTPCredentialRebound(t *testing.T) {
	stored := map[string]string{
		"smtp.default.host":     "smtp.provider.example",
		"smtp.default.port":     "587",
		"smtp.default.username": "postmaster@example.com",
		"smtp.default.password": "the-real-password",
	}
	get := func(k string) string { return stored[k] }

	base := SMTPConfigDTO{Host: "smtp.provider.example", Port: 587, Username: "postmaster@example.com"}

	cases := []struct {
		name string
		dto  SMTPConfigDTO
		want bool
	}{
		{"unchanged endpoint keeps the password", base, false},
		{"a new host would deliver it elsewhere", SMTPConfigDTO{Host: "collector.attacker.example", Port: 587, Username: base.Username}, true},
		{"a new port is half a new destination", SMTPConfigDTO{Host: base.Host, Port: 2525, Username: base.Username}, true},
		{"a new username would pair with the old password", SMTPConfigDTO{Host: base.Host, Port: base.Port, Username: "someone.else@example.com"}, true},
		{"a submitted password is a rotation, always allowed", SMTPConfigDTO{Host: "new.provider.example", Port: 465, Username: "new", Password: "fresh"}, false},
		{"whitespace is not a change", SMTPConfigDTO{Host: "  smtp.provider.example  ", Port: 587, Username: "  postmaster@example.com  "}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := smtpCredentialRebound(c.dto, "default", get); got != c.want {
				t.Errorf("smtpCredentialRebound() = %v, want %v", got, c.want)
			}
		})
	}
}

// With nothing stored there is nothing to rebind, and refusing the very first
// save would leave an operator unable to configure mail at all.
func TestSMTPFirstSaveIsNotARebind(t *testing.T) {
	get := func(string) string { return "" }
	dto := SMTPConfigDTO{Host: "smtp.provider.example", Port: 587, Username: "postmaster@example.com"}
	if smtpCredentialRebound(dto, "default", get) {
		t.Error("the first SMTP save was refused as a rebind; there was no stored password to rebind")
	}
}

func TestModCacheCredentialRebound(t *testing.T) {
	stored := map[string]string{
		settingModCacheAddr:     "cache.internal:6379",
		settingModCacheUsername: "dylaris",
		settingModCachePassword: "the-real-password",
	}
	get := func(k string) string { return stored[k] }

	cases := []struct {
		name string
		req  modCacheSettings
		want bool
	}{
		{"unchanged endpoint keeps the password", modCacheSettings{Addr: "cache.internal:6379", Username: "dylaris"}, false},
		{"a new address would deliver it elsewhere", modCacheSettings{Addr: "collector.attacker.example:6379", Username: "dylaris"}, true},
		{"a new username would pair with the old password", modCacheSettings{Addr: "cache.internal:6379", Username: "someone-else"}, true},
		{"a submitted password is a rotation, always allowed", modCacheSettings{Addr: "elsewhere:6379", Username: "other", Password: "fresh"}, false},
		{"clearing the address drops the credential rather than moving it", modCacheSettings{Addr: ""}, false},
		{"turning TLS off is a downgrade, not a rebind", modCacheSettings{Addr: "cache.internal:6379", Username: "dylaris", TLS: false}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := modCacheCredentialRebound(c.req, get); got != c.want {
				t.Errorf("modCacheCredentialRebound() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestModCacheFirstSaveIsNotARebind(t *testing.T) {
	get := func(string) string { return "" }
	if modCacheCredentialRebound(modCacheSettings{Addr: "cache.internal:6379"}, get) {
		t.Error("the first mod-cache save was refused as a rebind; there was no stored password to rebind")
	}
}

// End-to-end on the wire. The pure predicates above pass whether or not anyone
// CALLS them, which is the failure mode a helper written to prevent drift
// actually has. These two go through the handler, and they also pin the
// ORDERING: for mod-cache the refusal has to land before Reconfigure, which
// dials and authenticates against the submitted address inside the request.
func TestSaveSMTPConfig_RefusesARebindOnTheWire(t *testing.T) {
	fs := handlertest.NewSettingsStore()
	fs.KV["smtp.default.host"] = "smtp.provider.example"
	fs.KV["smtp.default.port"] = "587"
	fs.KV["smtp.default.username"] = "postmaster@example.com"
	fs.KV["smtp.default.password"] = "the-real-password"
	h := NewAuthSettingsHandler(&AppState{Store: fs})

	body, _ := json.Marshal(SMTPConfigDTO{
		Host:     "collector.attacker.example",
		Port:     587,
		Username: "postmaster@example.com",
		// no Password: the caller cannot read it, and is not supplying one
	})
	rw := httptest.NewRecorder()
	h.SaveSMTPConfig(rw, httptest.NewRequest(http.MethodPut, "/api/admin/settings/smtp", bytes.NewReader(body)))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rw.Code, rw.Body.String())
	}
	if got := fs.KV["smtp.default.host"]; got != "smtp.provider.example" {
		t.Fatalf("the host was rebound anyway, so the next send delivers the password to it: %q", got)
	}
	if got := fs.KV["smtp.default.password"]; got != "the-real-password" {
		t.Fatalf("stored password changed: %q", got)
	}
}

func TestModCacheSet_RefusesARebindOnTheWire(t *testing.T) {
	fs := handlertest.NewSettingsStore()
	fs.KV[settingModCacheAddr] = "cache.internal:6379"
	fs.KV[settingModCacheUsername] = "dylaris"
	fs.KV[settingModCachePassword] = "the-real-password"
	h := NewModCacheSettingsHandler(&AppState{Store: fs})

	body, _ := json.Marshal(modCacheSettings{
		Addr:     "collector.attacker.example:6379",
		Username: "dylaris",
	})
	rw := httptest.NewRecorder()
	h.Set(rw, httptest.NewRequest(http.MethodPut, "/api/admin/settings/mod-cache", bytes.NewReader(body)))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rw.Code, rw.Body.String())
	}
	if got := fs.KV[settingModCacheAddr]; got != "cache.internal:6379" {
		t.Fatalf("the address was rebound anyway: %q", got)
	}
	if got := fs.KV[settingModCachePassword]; got != "the-real-password" {
		t.Fatalf("stored password changed: %q", got)
	}
}
