package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"dylaris-core/mailer"
)

type AuthSettingsHandler struct {
	state *AppState
}

func NewAuthSettingsHandler(state *AppState) *AuthSettingsHandler {
	return &AuthSettingsHandler{state: state}
}

// AuthPolicy mirrors the auth.* settings keys. Defaults are chosen so a
// fresh install behaves conservatively: no self-registration, no email
// verification, no password policy enforcement beyond what users type.
// Operators opt in by toggling.
type AuthPolicy struct {
	RegistrationEnabled bool `json:"registrationEnabled"`
	EmailVerifyRequired bool `json:"emailVerifyRequired"`
	PasswordMinLength   int  `json:"passwordMinLength"`
	// 2FA enforcement. When on, affected users that don't have 2FA
	// configured are forced through a setup flow at the next login
	// instead of receiving a normal session token.
	Require2FAForAdmins   bool `json:"require2FAForAdmins"`
	Require2FAForAllUsers bool `json:"require2FAForAllUsers"`

	// Password-reset token lifetime. Short enough that a leaked link
	// expires quickly, long enough for the user to retrieve the email
	// out-of-band. Range is enforced server-side in SaveAuthPolicy.
	PasswordResetLinkTTLMinutes int `json:"passwordResetLinkTTLMinutes"`

	// Security questions. Master toggle gates the entire feature; the
	// per-use-case requireds layer on top. Count is how many questions
	// the user must pick + answer (3 is the de-facto standard).
	SecurityQuestionsEnabled          bool `json:"securityQuestionsEnabled"`
	SecurityQuestionsRequiredAtSignup bool `json:"securityQuestionsRequiredAtSignup"`
	SecurityQuestionsRequiredAtReset  bool `json:"securityQuestionsRequiredAtReset"`
	SecurityQuestionsCount            int  `json:"securityQuestionsCount"`

	// Auto-delete of inactive users.
	// InactiveDaysBeforeDelete is the dormancy threshold; users idle that
	// long become eligible. HistoryGraceExtraDays tacks on additional time
	// for users with server history (and tickets later) so frequent admins
	// don't accidentally nuke long-time customers. DeleteEmailWarningDays
	// is the warning-to-execution gap. DeletionMode picks between hard-
	// delete (DB row gone) and anonymize (row kept, PII wiped) so admins
	// can satisfy either DSGVO right-to-be-forgotten or audit-trail-keeps
	// requirements depending on their compliance posture.
	InactiveDeleteEnabled    bool   `json:"inactiveDeleteEnabled"`
	InactiveDaysBeforeDelete int    `json:"inactiveDaysBeforeDelete"`
	HistoryGraceExtraDays    int    `json:"historyGraceExtraDays"`
	DeleteEmailWarningDays   int    `json:"deleteEmailWarningDays"`
	DeletionMode             string `json:"deletionMode"` // "anonymize" | "hard_delete"
}

var defaultAuthPolicy = AuthPolicy{
	RegistrationEnabled:               false,
	EmailVerifyRequired:               false,
	PasswordMinLength:                 12,
	Require2FAForAdmins:               false,
	Require2FAForAllUsers:             false,
	PasswordResetLinkTTLMinutes:       60,
	SecurityQuestionsEnabled:          false,
	SecurityQuestionsRequiredAtSignup: false,
	SecurityQuestionsRequiredAtReset:  false,
	SecurityQuestionsCount:            3,
	InactiveDeleteEnabled:             false,
	InactiveDaysBeforeDelete:          90,
	HistoryGraceExtraDays:             90,
	DeleteEmailWarningDays:            7,
	DeletionMode:                      "anonymize",
}

// LoadAuthPolicy reads the auth.* settings with default fallback. Exposed at
// package level so other handlers (login, registration) can re-check the
// policy on every request without paying for an HTTP round-trip.
func LoadAuthPolicy(state *AppState) AuthPolicy {
	p := defaultAuthPolicy
	if v, _ := state.Store.GetSetting("auth.registration_enabled"); v != "" {
		p.RegistrationEnabled = v == "true"
	}
	if v, _ := state.Store.GetSetting("auth.email_verify_required"); v != "" {
		p.EmailVerifyRequired = v == "true"
	}
	if v, _ := state.Store.GetSetting("auth.password_min_length"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 4 && n <= 128 {
			p.PasswordMinLength = n
		}
	}
	if v, _ := state.Store.GetSetting("auth.require_2fa_for_admins"); v != "" {
		p.Require2FAForAdmins = v == "true"
	}
	if v, _ := state.Store.GetSetting("auth.require_2fa_for_all_users"); v != "" {
		p.Require2FAForAllUsers = v == "true"
	}
	if v, _ := state.Store.GetSetting("auth.password_reset_link_ttl_minutes"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 5 && n <= 24*60 {
			p.PasswordResetLinkTTLMinutes = n
		}
	}
	if v, _ := state.Store.GetSetting("auth.security_questions_enabled"); v != "" {
		p.SecurityQuestionsEnabled = v == "true"
	}
	if v, _ := state.Store.GetSetting("auth.security_questions_required_at_signup"); v != "" {
		p.SecurityQuestionsRequiredAtSignup = v == "true"
	}
	if v, _ := state.Store.GetSetting("auth.security_questions_required_at_reset"); v != "" {
		p.SecurityQuestionsRequiredAtReset = v == "true"
	}
	if v, _ := state.Store.GetSetting("auth.security_questions_count"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 1 && n <= 10 {
			p.SecurityQuestionsCount = n
		}
	}
	if v, _ := state.Store.GetSetting("auth.inactive_delete_enabled"); v != "" {
		p.InactiveDeleteEnabled = v == "true"
	}
	if v, _ := state.Store.GetSetting("auth.inactive_days_before_delete"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 7 && n <= 3650 {
			p.InactiveDaysBeforeDelete = n
		}
	}
	if v, _ := state.Store.GetSetting("auth.history_grace_extra_days"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 && n <= 3650 {
			p.HistoryGraceExtraDays = n
		}
	}
	if v, _ := state.Store.GetSetting("auth.delete_email_warning_days"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 && n <= 30 {
			p.DeleteEmailWarningDays = n
		}
	}
	if v, _ := state.Store.GetSetting("auth.deletion_mode"); v == "hard_delete" || v == "anonymize" {
		p.DeletionMode = v
	}
	return p
}

// GetAuthPolicy GET /api/admin/settings/auth - PANEL settings.read (RequireCap at the route).
func (h *AuthSettingsHandler) GetAuthPolicy(w http.ResponseWriter, r *http.Request) {
	out := map[string]interface{}{
		"success": true,
		"policy":  LoadAuthPolicy(h.state),
	}
	// Whether mail can be sent AT ALL, answered by loading the transport the
	// real senders load rather than by inspecting settings keys. A second
	// "is it configured" rule beside the one that actually sends is how the two
	// drift, and the whole point here is that the screen must not be able to
	// disagree with reality.
	//
	// Every switch above it is a promise that a message will arrive:
	// emailVerifyRequired refuses a login until one does, the reset link IS the
	// message, and the inactive-account sweep warns before it deletes. With no
	// transport the first of those locks every new account out at the moment it
	// is switched on, and nothing anywhere says why - the reset endpoint answers
	// success either way, deliberately, so that an attacker cannot enumerate
	// accounts with it. That anti-enumeration is right and it is also why the
	// operator is the one who needs telling.
	//
	// Purpose "auth", because that is what the reset and verification senders
	// pass; a deployment may configure ticket mail and nothing else.
	if h.state.Store != nil {
		_, mailErr := mailer.Load(h.state.Store, "auth")
		out["mailConfigured"] = mailErr == nil
	}
	// How many accounts "Require during password reset" does not actually
	// cover. That toggle skips any user with no questions stored - deliberately,
	// so it cannot lock out accounts that predate it - and the screen says so.
	// What it could not say is the scale, which the moment it is switched on is
	// EVERY existing account. Measured on the testbed: with the policy on, an
	// account with none on file reset with no answers at all, and the
	// validate-token call did not even mention that answers were required.
	//
	// Best-effort: a count that cannot be read is not a reason to fail the
	// settings screen, it just means the hint is omitted.
	if h.state.Store != nil {
		if missing, total, err := h.state.Store.CountUsersMissingSecurityQuestions(); err == nil {
			out["accountsMissingSecurityQuestions"] = missing
			out["accountsTotal"] = total
		} else {
			log.Printf("auth-settings: could not count accounts without security questions: %v", err)
		}
	}
	json.NewEncoder(w).Encode(out)
}

// SaveAuthPolicy PUT /api/admin/settings/auth - PANEL settings.write (RequireCap at the route).
func (h *AuthSettingsHandler) SaveAuthPolicy(w http.ResponseWriter, r *http.Request) {
	var p AuthPolicy
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if p.PasswordMinLength < 4 {
		p.PasswordMinLength = 4
	}
	if p.PasswordMinLength > 128 {
		p.PasswordMinLength = 128
	}
	if p.PasswordResetLinkTTLMinutes < 5 {
		p.PasswordResetLinkTTLMinutes = 5
	}
	if p.PasswordResetLinkTTLMinutes > 24*60 {
		p.PasswordResetLinkTTLMinutes = 24 * 60
	}
	if p.SecurityQuestionsCount < 1 {
		p.SecurityQuestionsCount = 1
	}
	if p.SecurityQuestionsCount > 10 {
		p.SecurityQuestionsCount = 10
	}
	if p.InactiveDaysBeforeDelete < 7 {
		p.InactiveDaysBeforeDelete = 7
	}
	if p.InactiveDaysBeforeDelete > 3650 {
		p.InactiveDaysBeforeDelete = 3650
	}
	if p.HistoryGraceExtraDays < 0 {
		p.HistoryGraceExtraDays = 0
	}
	if p.HistoryGraceExtraDays > 3650 {
		p.HistoryGraceExtraDays = 3650
	}
	if p.DeleteEmailWarningDays < 0 {
		p.DeleteEmailWarningDays = 0
	}
	if p.DeleteEmailWarningDays > 30 {
		p.DeleteEmailWarningDays = 30
	}
	if p.DeletionMode != "hard_delete" && p.DeletionMode != "anonymize" {
		p.DeletionMode = "anonymize"
	}

	actorID, _ := r.Context().Value("userID").(string)
	pairs := []struct{ k, v string }{
		{"auth.registration_enabled", fmt.Sprintf("%t", p.RegistrationEnabled)},
		{"auth.email_verify_required", fmt.Sprintf("%t", p.EmailVerifyRequired)},
		{"auth.password_min_length", fmt.Sprintf("%d", p.PasswordMinLength)},
		{"auth.require_2fa_for_admins", fmt.Sprintf("%t", p.Require2FAForAdmins)},
		{"auth.require_2fa_for_all_users", fmt.Sprintf("%t", p.Require2FAForAllUsers)},
		{"auth.password_reset_link_ttl_minutes", fmt.Sprintf("%d", p.PasswordResetLinkTTLMinutes)},
		{"auth.security_questions_enabled", fmt.Sprintf("%t", p.SecurityQuestionsEnabled)},
		{"auth.security_questions_required_at_signup", fmt.Sprintf("%t", p.SecurityQuestionsRequiredAtSignup)},
		{"auth.security_questions_required_at_reset", fmt.Sprintf("%t", p.SecurityQuestionsRequiredAtReset)},
		{"auth.security_questions_count", fmt.Sprintf("%d", p.SecurityQuestionsCount)},
		{"auth.inactive_delete_enabled", fmt.Sprintf("%t", p.InactiveDeleteEnabled)},
		{"auth.inactive_days_before_delete", fmt.Sprintf("%d", p.InactiveDaysBeforeDelete)},
		{"auth.history_grace_extra_days", fmt.Sprintf("%d", p.HistoryGraceExtraDays)},
		{"auth.delete_email_warning_days", fmt.Sprintf("%d", p.DeleteEmailWarningDays)},
		{"auth.deletion_mode", p.DeletionMode},
	}
	for _, kv := range pairs {
		if err := h.state.Store.SetSettingBy(kv.k, kv.v, actorID); err != nil {
			sendJSONError(w, "Failed to save: "+kv.k, http.StatusInternalServerError)
			return
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "policy": p})
}

// SMTPConfigDTO is what crosses the wire. Password is *write*-only — never
// emitted in GET (so it can't leak via the admin UI). On save, an empty
// password is treated as "keep the existing one" so a re-save without
// re-typing the password doesn't wipe it.
type SMTPConfigDTO struct {
	// Provider selects the transport: "smtp" (default) or "resend". The From
	// identity below is shared by both, because it is a property of the mail
	// configuration rather than of the wire protocol - switching provider should
	// not ask an operator to retype their own address.
	//
	// A POINTER, for the same reason BeamSettings.ManualOverride is one: absent
	// and empty are different instructions. Treating an omitted field as "smtp"
	// meant any client that predated this field - a tab opened before the deploy,
	// a script, an IaC caller - silently flipped a working Resend setup back to
	// an SMTP profile nobody had configured, and every verification mail, reset
	// and dunning notice stopped with a success response on screen.
	Provider   *string `json:"provider"`
	Host       string  `json:"host"`
	Port       int     `json:"port"`
	Username   string  `json:"username"`
	Password   string  `json:"password,omitempty"` // write-only
	FromEmail  string  `json:"fromEmail"`
	FromName   string  `json:"fromName"`
	Encryption string  `json:"encryption"`
	// PasswordSet is set by the GET handler so the UI can show
	// "(saved — leave blank to keep)" placeholder.
	PasswordSet bool `json:"passwordSet,omitempty"`

	// ResendAPIKey follows the same write-only rule as the SMTP password, and
	// for the same reason: it is a credential that can send mail as the
	// operator's domain, which is the domain their password-reset links come
	// from. Stored encrypted at rest (store.settingsSecretKeys).
	ResendAPIKey    string `json:"resendApiKey,omitempty"` // write-only
	ResendAPIKeySet bool   `json:"resendApiKeySet,omitempty"`
}

// GetSMTPConfig GET /api/admin/settings/smtp - PANEL settings.read (RequireCap at the route).
func (h *AuthSettingsHandler) GetSMTPConfig(w http.ResponseWriter, r *http.Request) {
	dto := loadSMTPConfigForUI(h.state, "default")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"config":  dto,
	})
}

func loadSMTPConfigForUI(state *AppState, purpose string) SMTPConfigDTO {
	get := func(field string) string {
		v, _ := state.Store.GetSetting("smtp." + purpose + "." + field)
		return v
	}
	port := 0
	if v := get("port"); v != "" {
		fmt.Sscanf(v, "%d", &port)
	}
	pw := get("password")
	provider, _ := state.Store.GetSetting(mailer.SettingKeyProvider)
	if strings.TrimSpace(provider) == "" {
		provider = mailer.ProviderSMTP
	}
	resendKey, _ := state.Store.GetSetting(mailer.SettingKeyResendAPIKey)
	return SMTPConfigDTO{
		Provider:        &provider,
		Host:            get("host"),
		Port:            port,
		Username:        get("username"),
		FromEmail:       get("from_email"),
		FromName:        get("from_name"),
		Encryption:      get("encryption"),
		PasswordSet:     pw != "",
		ResendAPIKeySet: strings.TrimSpace(resendKey) != "",
	}
}

// smtpCredentialRebound reports whether this save would send the STORED SMTP
// password to a different server than the one it was entered for, without the
// caller having supplied a new one.
//
// The fourth copy of a guard core storage, backup storages and modpack storage
// already carry (mergeCoreStorageCandidate, mergeBackupStorageSecret,
// modpackS3SecretRebound), and the one where it matters most. Those three
// protect an S3 secret, which SigV4 signs with and never transmits; rebinding
// them redirects signed requests. SMTP transmits the credential itself:
// smtp.PlainAuth hands the operator's username and password to whatever host
// mailer.Send dials.
//
// settings.write is a delegatable panel capability and GetSMTPConfig returns
// only passwordSet, so a holder who cannot READ the password could point the
// host at a server they control - with a certificate genuinely valid for their
// own name, which is all the stdlib checks - and collect it. TestSendSMTP makes
// that synchronous: no need to wait for a real mail event.
//
// Port and username are in the comparison for the same reasons as the S3 trio:
// the port is half the destination, and a new username silently paired with the
// old password is an auth failure with nothing pointing at the edit.
//
// A submitted password is a genuine rotation and is always allowed; with no
// stored password there is nothing to rebind.
func smtpCredentialRebound(dto SMTPConfigDTO, purpose string, get func(string) string) bool {
	if dto.Password != "" || get("smtp."+purpose+".password") == "" {
		return false
	}
	return strings.TrimSpace(dto.Host) != get("smtp."+purpose+".host") ||
		fmt.Sprintf("%d", dto.Port) != get("smtp."+purpose+".port") ||
		strings.TrimSpace(dto.Username) != get("smtp."+purpose+".username")
}

// SaveSMTPConfig PUT /api/admin/settings/smtp - PANEL settings.write (RequireCap at the route).
func (h *AuthSettingsHandler) SaveSMTPConfig(w http.ResponseWriter, r *http.Request) {
	var dto SMTPConfigDTO
	if err := json.NewDecoder(r.Body).Decode(&dto); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	enc := strings.ToLower(strings.TrimSpace(dto.Encryption))
	if enc != "none" && enc != "starttls" && enc != "tls" {
		enc = "starttls"
	}

	// Absent means "leave it alone"; an unknown VALUE is coerced to SMTP, since
	// the only way to send one is a client out of step with this build and a 400
	// there is not something an operator can act on.
	provider := ""
	if dto.Provider != nil {
		provider = strings.ToLower(strings.TrimSpace(*dto.Provider))
		if provider != mailer.ProviderResend {
			provider = mailer.ProviderSMTP
		}
	}

	actorID, _ := r.Context().Value("userID").(string)
	purpose := "default"
	get := func(k string) string {
		v, _ := h.state.Store.GetSetting(k)
		return v
	}
	if smtpCredentialRebound(dto, purpose, get) {
		sendJSONError(w, "the SMTP host, port or username changed, so the stored password cannot be reused - re-enter the password with this change", http.StatusBadRequest)
		return
	}
	pairs := []struct{ k, v string }{}
	if provider != "" {
		pairs = append(pairs, struct{ k, v string }{mailer.SettingKeyProvider, provider})
	}
	pairs = append(pairs, []struct{ k, v string }{
		{"smtp." + purpose + ".host", strings.TrimSpace(dto.Host)},
		{"smtp." + purpose + ".port", fmt.Sprintf("%d", dto.Port)},
		{"smtp." + purpose + ".username", strings.TrimSpace(dto.Username)},
		{"smtp." + purpose + ".from_email", strings.TrimSpace(dto.FromEmail)},
		{"smtp." + purpose + ".from_name", strings.TrimSpace(dto.FromName)},
		{"smtp." + purpose + ".encryption", enc},
	}...)
	for _, kv := range pairs {
		if err := h.state.Store.SetSettingBy(kv.k, kv.v, actorID); err != nil {
			sendJSONError(w, "Failed to save: "+kv.k, http.StatusInternalServerError)
			return
		}
	}
	// Empty password on save means "leave it as-is" — avoids accidentally
	// wiping the secret when an admin re-opens the form to flip from_name.
	if dto.Password != "" {
		if err := h.state.Store.SetSettingBy("smtp."+purpose+".password", dto.Password, actorID); err != nil {
			sendJSONError(w, "Failed to save password", http.StatusInternalServerError)
			return
		}
	}
	// Same rule for the Resend key: blank means keep. Switching provider back
	// and forth must not cost the credential.
	if strings.TrimSpace(dto.ResendAPIKey) != "" {
		if err := h.state.Store.SetSettingBy(mailer.SettingKeyResendAPIKey, strings.TrimSpace(dto.ResendAPIKey), actorID); err != nil {
			sendJSONError(w, "Failed to save the Resend API key", http.StatusInternalServerError)
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"config":  loadSMTPConfigForUI(h.state, purpose),
	})
}

// TestSendRequest tells the test endpoint which address to send to.
// Defaults to the caller's own email when omitted.
type testSendRequest struct {
	To string `json:"to"`
}

// TestSendSMTP POST /api/admin/settings/smtp/test — sends a fixed body so
// admins can verify their config works without leaking real verification tokens.
// PANEL settings.write (RequireCap at the route).
func (h *AuthSettingsHandler) TestSendSMTP(w http.ResponseWriter, r *http.Request) {
	var req testSendRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	to := strings.TrimSpace(req.To)
	if to == "" {
		// Default to the admin's own email so a misclick on Test Send doesn't
		// reach a real user mailbox.
		actorID, _ := r.Context().Value("userID").(string)
		if u, err := h.state.Store.GetUserByID(actorID); err == nil {
			to = u.Email
		}
	}
	if to == "" {
		sendJSONError(w, "No recipient — provide 'to' or set your account email first", http.StatusBadRequest)
		return
	}

	transport, err := mailer.Load(h.state.Store, "default")
	if err != nil {
		sendJSONError(w, "Mail not configured: "+err.Error(), http.StatusBadRequest)
		return
	}
	err = transport.Send(mailer.Message{
		To:      to,
		Subject: "Dylaris — mail test",
		Body: "If you can read this, your Dylaris mail configuration is working.\n\n" +
			"Sent via " + transport.Describe() + ".\n" +
			"Triggered from Settings -> User settings -> Email.",
	})
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "Send failed: " + err.Error(),
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Test email sent to " + to,
	})
}
