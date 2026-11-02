package services

import (
	"fmt"

	"dylaris-core/store"
)

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

// LoadAuthPolicy reads the auth.* settings with default fallback. Shared by the
// login, registration, account and settings handlers, which re-check the
// policy on every request.
func LoadAuthPolicy(st store.Store) AuthPolicy {
	p := defaultAuthPolicy
	if v, _ := st.GetSetting("auth.registration_enabled"); v != "" {
		p.RegistrationEnabled = v == "true"
	}
	if v, _ := st.GetSetting("auth.email_verify_required"); v != "" {
		p.EmailVerifyRequired = v == "true"
	}
	if v, _ := st.GetSetting("auth.password_min_length"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 4 && n <= 128 {
			p.PasswordMinLength = n
		}
	}
	if v, _ := st.GetSetting("auth.require_2fa_for_admins"); v != "" {
		p.Require2FAForAdmins = v == "true"
	}
	if v, _ := st.GetSetting("auth.require_2fa_for_all_users"); v != "" {
		p.Require2FAForAllUsers = v == "true"
	}
	if v, _ := st.GetSetting("auth.password_reset_link_ttl_minutes"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 5 && n <= 24*60 {
			p.PasswordResetLinkTTLMinutes = n
		}
	}
	if v, _ := st.GetSetting("auth.security_questions_enabled"); v != "" {
		p.SecurityQuestionsEnabled = v == "true"
	}
	if v, _ := st.GetSetting("auth.security_questions_required_at_signup"); v != "" {
		p.SecurityQuestionsRequiredAtSignup = v == "true"
	}
	if v, _ := st.GetSetting("auth.security_questions_required_at_reset"); v != "" {
		p.SecurityQuestionsRequiredAtReset = v == "true"
	}
	if v, _ := st.GetSetting("auth.security_questions_count"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 1 && n <= 10 {
			p.SecurityQuestionsCount = n
		}
	}
	if v, _ := st.GetSetting("auth.inactive_delete_enabled"); v != "" {
		p.InactiveDeleteEnabled = v == "true"
	}
	if v, _ := st.GetSetting("auth.inactive_days_before_delete"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 7 && n <= 3650 {
			p.InactiveDaysBeforeDelete = n
		}
	}
	if v, _ := st.GetSetting("auth.history_grace_extra_days"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 && n <= 3650 {
			p.HistoryGraceExtraDays = n
		}
	}
	if v, _ := st.GetSetting("auth.delete_email_warning_days"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 && n <= 30 {
			p.DeleteEmailWarningDays = n
		}
	}
	if v, _ := st.GetSetting("auth.deletion_mode"); v == "hard_delete" || v == "anonymize" {
		p.DeletionMode = v
	}
	return p
}
